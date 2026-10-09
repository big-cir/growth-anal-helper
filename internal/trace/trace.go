// Package trace writes one JSON line per request step (agent call, probe, panel run, wait for the user, request summary)
// to <outDir>/logs/trace/YYYY-MM-DD.jsonl (0600). Lines never hold question text, SQL or result values.
package trace

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"growth-lab/internal/jsjson"
)

// Span is one step. Optional fields are nil when absent; SQL is kept in memory only (eval reports).
type Span struct {
	T         string   `json:"t"`
	RequestID string   `json:"request_id"`
	Conv      *string  `json:"conv"`
	Span      string   `json:"span"`
	Ms        float64  `json:"ms"`
	OK        bool     `json:"ok"`
	Turn      *float64 `json:"turn,omitempty"`
	Provider  *string  `json:"provider,omitempty"`
	Model     *string  `json:"-"`
	HasModel  bool     `json:"-"`
	RunMs     *float64 `json:"run_ms,omitempty"`
	APIMs     *float64 `json:"api_ms,omitempty"`
	In        *float64 `json:"in,omitempty"`
	Out       *float64 `json:"out,omitempty"`
	CacheRead *float64 `json:"cache_read,omitempty"`
	CacheWrt  *float64 `json:"cache_write,omitempty"`
	CostUSD   *float64 `json:"cost_usd,omitempty"`
	Action    *string  `json:"action,omitempty"`
	Error     *string  `json:"error,omitempty"`
	Rows      *float64 `json:"rows,omitempty"`
	Stage     *string  `json:"stage,omitempty"`
	Wait      *string  `json:"wait,omitempty"`
	Outcome   *string  `json:"outcome,omitempty"`
	Reason    *string  `json:"reason,omitempty"`
	Turns     *float64 `json:"turns,omitempty"`
	Probes    *float64 `json:"probes,omitempty"`
	Panels    *float64 `json:"panels,omitempty"`
	SQL       *string  `json:"-"`
}

func opt[T any](v *T) any {
	if v == nil {
		return jsjson.Undefined
	}
	return *v
}

// FileLine is the JSON line written to the file (listed fields only, in this order).
func FileLine(s Span) string {
	var conv any
	if s.Conv != nil {
		conv = *s.Conv
	}
	model := any(jsjson.Undefined)
	if s.HasModel {
		model = nil
		if s.Model != nil {
			model = *s.Model
		}
	}
	return jsjson.MustStringify(jsjson.Object{
		{Key: "t", Value: s.T}, {Key: "request_id", Value: s.RequestID}, {Key: "conv", Value: conv}, {Key: "span", Value: s.Span}, {Key: "ms", Value: s.Ms}, {Key: "ok", Value: s.OK},
		{Key: "turn", Value: opt(s.Turn)}, {Key: "provider", Value: opt(s.Provider)}, {Key: "model", Value: model}, {Key: "run_ms", Value: opt(s.RunMs)}, {Key: "api_ms", Value: opt(s.APIMs)},
		{Key: "in", Value: opt(s.In)}, {Key: "out", Value: opt(s.Out)}, {Key: "cache_read", Value: opt(s.CacheRead)}, {Key: "cache_write", Value: opt(s.CacheWrt)},
		{Key: "cost_usd", Value: opt(s.CostUSD)}, {Key: "action", Value: opt(s.Action)}, {Key: "error", Value: opt(s.Error)}, {Key: "rows", Value: opt(s.Rows)}, {Key: "stage", Value: opt(s.Stage)},
		{Key: "wait", Value: opt(s.Wait)}, {Key: "outcome", Value: opt(s.Outcome)}, {Key: "reason", Value: opt(s.Reason)}, {Key: "turns", Value: opt(s.Turns)}, {Key: "probes", Value: opt(s.Probes)}, {Key: "panels", Value: opt(s.Panels)},
	})
}

// UnmarshalJSON keeps "model": null apart from a missing model.
func (s *Span) UnmarshalJSON(b []byte) error {
	type plain Span
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*s = Span(p)
	if raw, ok := m["model"]; ok {
		s.HasModel = true
		var v *string
		_ = json.Unmarshal(raw, &v)
		s.Model = v
	}
	return nil
}

var fileRE = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.jsonl$`)

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Log is the trace folder of an output root.
type Log struct {
	Dir    string
	warned bool
}

// NewLog returns <outDir>/logs/trace.
func NewLog(outDir string) *Log { return &Log{Dir: filepath.Join(outDir, "logs", "trace")} }

// Write never fails: a trace failure must not stop a request.
func (l *Log) Write(s Span) {
	err := func() error {
		if err := os.MkdirAll(l.Dir, 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(l.Dir, s.T[:10]+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(FileLine(s) + "\n")
		return err
	}()
	if err != nil {
		if !l.warned {
			fmt.Fprintf(os.Stderr, "Trace write failed: %s\n", err.Error())
		}
		l.warned = true
	}
}

// Prune removes files older than retentionDays.
func (l *Log) Prune(retentionDays int) {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		return
	}
	cutoff := day(time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour))
	for _, e := range entries {
		if m := fileRE.FindStringSubmatch(e.Name()); m != nil && m[1] < cutoff {
			os.Remove(filepath.Join(l.Dir, e.Name()))
		}
	}
}

// Read returns the spans of the last days days (today included); now is milliseconds.
func (l *Log) Read(days int, nowMs int64) []Span {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	from := day(time.UnixMilli(nowMs - int64(days-1)*86_400_000))
	var spans []Span
	for _, n := range names {
		m := fileRE.FindStringSubmatch(n)
		if m == nil || m[1] < from {
			continue
		}
		b, err := os.ReadFile(filepath.Join(l.Dir, n))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var s Span
			if json.Unmarshal([]byte(line), &s) == nil {
				spans = append(spans, s)
			}
		}
	}
	return spans
}

// ToFixed is Number.prototype.toFixed (ties round away from zero on the exact binary value).
func ToFixed(x float64, digits int) string {
	if math.IsNaN(x) {
		return "NaN"
	}
	if math.Abs(x) >= 1e21 {
		return jsjson.Number(x)
	}
	neg := x < 0
	exact := new(big.Float).SetPrec(0).SetFloat64(math.Abs(x)).Text('f', 1100)
	intPart, frac, _ := strings.Cut(exact, ".")
	frac += strings.Repeat("0", digits+1)
	keep := intPart + frac[:digits]
	n, _ := new(big.Int).SetString(keep, 10)
	if frac[digits] >= '5' {
		n.Add(n, big.NewInt(1))
	}
	s := n.String()
	if len(s) <= digits {
		s = strings.Repeat("0", digits-len(s)+1) + s
	}
	out := s
	if digits > 0 {
		out = s[:len(s)-digits] + "." + s[len(s)-digits:]
	}
	if neg {
		out = "-" + out
	}
	return out
}

func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

func pct(values []float64, p float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	s := append([]float64(nil), values...)
	sort.SliceStable(s, func(i, j int) bool { return s[i] < s[j] })
	i := math.Min(float64(len(s)-1), math.Max(0, math.Ceil((p/100)*float64(len(s)))-1))
	return &s[int(i)]
}

func sec(ms *float64) string {
	if ms == nil {
		return "-"
	}
	return ToFixed(*ms/1000, 1) + "s"
}

func sum(xs []float64) float64 {
	t := 0.0
	for _, x := range xs {
		t += x
	}
	return t
}

func avg(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	return sum(xs) / float64(len(xs))
}

func countBy(items []string) string {
	var keys []string
	n := map[string]int{}
	for _, i := range items {
		if n[i] == 0 {
			keys = append(keys, i)
		}
		n[i]++
	}
	sort.SliceStable(keys, func(a, b int) bool { return n[keys[a]] > n[keys[b]] })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, n[k])
	}
	return strings.Join(parts, ", ")
}

func str(p *string) string {
	if p == nil {
		return "undefined"
	}
	return *p
}

func num(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func padEnd(s string, n int) string {
	if l := len([]rune(s)); l < n {
		return s + strings.Repeat(" ", n-l)
	}
	return s
}

func padStart(s string, n int) string {
	if l := len([]rune(s)); l < n {
		return strings.Repeat(" ", n-l) + s
	}
	return s
}

// Summarize is the text report of `trace`.
func Summarize(spans []Span, slowest int) []string {
	of := func(k string) []Span {
		var out []Span
		for _, s := range spans {
			if s.Span == k {
				out = append(out, s)
			}
		}
		return out
	}
	reqs := of("request")
	if len(reqs) == 0 {
		return []string{"No requests in this period"}
	}
	llm, probes, panels := of("llm"), of("probe_sql"), of("panel_run")
	ms := func(ss []Span) []float64 {
		out := make([]float64, len(ss))
		for i, s := range ss {
			out[i] = s.Ms
		}
		return out
	}
	field := func(ss []Span, f func(Span) float64) []float64 {
		out := make([]float64, len(ss))
		for i, s := range ss {
			out[i] = f(s)
		}
		return out
	}
	line := func(label string, xs []float64) string {
		return fmt.Sprintf("%sp50 %s  p95 %s  max %s  (n=%d)", padEnd(label, 14), sec(pct(xs, 50)), sec(pct(xs, 95)), sec(pct(xs, 100)), len(xs))
	}
	outcomes := make([]string, len(reqs))
	for i, r := range reqs {
		outcomes[i] = "?"
		if r.Outcome != nil {
			outcomes[i] = *r.Outcome
		}
	}
	out := []string{
		fmt.Sprintf("Requests      %d (%s)", len(reqs), countBy(outcomes)),
		line("Request", ms(reqs)),
		fmt.Sprintf("Per request   agent calls %s, probes %s, panel runs %s (average)", ToFixed(avg(field(reqs, func(s Span) float64 { return num(s.Turns) })), 1),
			ToFixed(avg(field(reqs, func(s Span) float64 { return num(s.Probes) })), 1), ToFixed(avg(field(reqs, func(s Span) float64 { return num(s.Panels) })), 1)),
		line("Agent call", ms(llm)),
	}
	var queue, overhead []float64
	anyQueue := false
	for _, s := range llm {
		if s.RunMs != nil {
			q := s.Ms - *s.RunMs
			queue = append(queue, q)
			if q > 0 {
				anyQueue = true
			}
			if s.APIMs != nil {
				overhead = append(overhead, *s.RunMs-*s.APIMs)
			}
		}
	}
	if anyQueue {
		out = append(out, line("  slot wait", queue))
	}
	if len(overhead) > 0 {
		out = append(out, line("  outside API", overhead))
	}
	out = append(out, line("Probe query", ms(probes)), line("Panel run", ms(panels)))

	var withUsage []Span
	for _, s := range llm {
		if s.In != nil {
			withUsage = append(withUsage, s)
		}
	}
	if len(withUsage) > 0 {
		prompt := sum(field(withUsage, func(s Span) float64 { return num(s.In) + num(s.CacheRead) + num(s.CacheWrt) }))
		read := sum(field(withUsage, func(s Span) float64 { return num(s.CacheRead) }))
		hit := 0.0
		if prompt != 0 {
			hit = jsRound((read / prompt) * 100)
		}
		out = append(out, fmt.Sprintf("Cache hit     %s%% of prompt tokens (%d calls, avg prompt %s tokens, avg output %s)", jsjson.Number(hit), len(withUsage),
			jsjson.Number(jsRound(prompt/float64(len(withUsage)))), jsjson.Number(jsRound(avg(field(withUsage, func(s Span) float64 { return num(s.Out) }))))))
	}
	var models []string
	byModel := map[string][]Span{}
	for _, s := range llm {
		m := "default"
		if s.Model != nil {
			m = *s.Model
		}
		k := str(s.Provider) + "/" + m
		if _, ok := byModel[k]; !ok {
			models = append(models, k)
		}
		byModel[k] = append(byModel[k], s)
	}
	for _, k := range models {
		ss := byModel[k]
		reqIDs := map[string]bool{}
		for _, s := range ss {
			reqIDs[s.RequestID] = true
		}
		cost := sum(field(ss, func(s Span) float64 { return num(s.CostUSD) })) / float64(len(reqIDs))
		out = append(out, fmt.Sprintf("Cost          %s: $%s per request (%d requests)", k, ToFixed(cost, 3), len(reqIDs)))
	}

	var failures []string
	orQ := func(p *string) string {
		if p == nil {
			return "?"
		}
		return *p
	}
	for _, s := range llm {
		if !s.OK {
			failures = append(failures, "agent call "+orQ(s.Error))
		}
	}
	for _, s := range probes {
		if !s.OK {
			failures = append(failures, "probe "+orQ(s.Stage))
		}
	}
	for _, s := range panels {
		if !s.OK {
			failures = append(failures, "panel "+orQ(s.Stage))
		}
	}
	for _, r := range reqs {
		if r.Outcome != nil && *r.Outcome == "failed" {
			failures = append(failures, "request "+orQ(r.Reason))
		}
	}
	if len(failures) > 0 {
		out = append(out, "Failures      "+countBy(failures))
	}

	slow := append([]Span(nil), reqs...)
	sort.SliceStable(slow, func(i, j int) bool { return slow[i].Ms > slow[j].Ms })
	if len(slow) > slowest {
		slow = slow[:slowest]
	}
	out = append(out, "Slowest requests")
	for _, r := range slow {
		mine := func(k string) []Span {
			var o []Span
			for _, s := range spans {
				if s.RequestID == r.RequestID && s.Span == k {
					o = append(o, s)
				}
			}
			return o
		}
		var parts []string
		for _, p := range []struct{ label, kind string }{{"agent", "llm"}, {"probe", "probe_sql"}, {"panel", "panel_run"}} {
			if ss := mine(p.kind); len(ss) > 0 {
				t := sum(ms(ss))
				parts = append(parts, fmt.Sprintf("%s %d×%s", p.label, len(ss), sec(&t)))
			}
		}
		conv := ""
		if r.Conv != nil && *r.Conv != "" {
			conv = " (conversation " + *r.Conv + ")"
		}
		t := r.T
		if len(t) > 16 {
			t = t[:16]
		}
		t = strings.Replace(t, "T", " ", 1)
		rms := r.Ms
		out = append(out, fmt.Sprintf("  %s  %s  request %s%s  %s  %s", padStart(sec(&rms), 7), t, r.RequestID, conv, str(r.Outcome), strings.Join(parts, ", ")))
	}
	return out
}

// Report is the `trace` command output for the last days days.
func Report(outDir string, days int, nowMs int64) []string {
	lines := []string{fmt.Sprintf("Last %d day(s), wait for the user excluded", days)}
	return append(lines, Summarize(NewLog(outDir).Read(days, nowMs), 5)...)
}
