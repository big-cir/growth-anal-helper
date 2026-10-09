package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"growth-lab/internal/jsstr"
	"math"
	"sort"
	"strings"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/trace"
)

// RunRecord is what one run ended with.
type RunRecord struct {
	CaseID       string
	Run          int
	Status       string // preview, question, refused, failed, cancelled, blocked_preflight
	FailReason   *string
	FailMessage  *string
	Spec         *panels.Spec
	Columns      []panels.Column
	Rows         []panels.Row
	AskQuestions []string
	RefuseReason *string
	// trace spans of the run (agent calls, probes and panel runs with SQL)
	Steps []jsjson.Object
	// the case's own path signals
	Asks            int
	AskLoop         bool
	OffdictAttempts int
	// a question came before any probe or panel
	AskedFirst bool
	Turns      int
	Probes     int
	Fixes      int
	PanelsRun  int
	Ms         float64
	Cost       float64
}

// GradedRun is a run with its verdict.
type GradedRun struct {
	RunRecord
	Verdict string // verified, unreviewed, workaround, failed
	Codes   []string
	Diag    []string
	Diffs   []Diff
	Labels  []LabelReview
}

func sp(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// JS is the run as the report stores it.
func (g GradedRun) JS() jsjson.Object {
	var spec, asks any
	if g.Spec != nil {
		spec = g.Spec.JS()
	}
	if g.AskQuestions != nil {
		asks = g.AskQuestions
	}
	steps := make([]any, len(g.Steps))
	for i, s := range g.Steps {
		steps[i] = s
	}
	diffs := make([]any, len(g.Diffs))
	for i, d := range g.Diffs {
		diffs[i] = jsjson.Object{{Key: "key", Value: cellsJS(d.Key)}, {Key: "role", Value: d.Role}, {Key: "expected", Value: cellJS(d.Expected)}, {Key: "actual", Value: cellJS(d.Actual)}}
	}
	labels := make([]any, len(g.Labels))
	for i, l := range g.Labels {
		labels[i] = jsjson.Object{{Key: "role", Value: l.Role}, {Key: "key", Value: l.Key}, {Key: "expected", Value: cellJS(l.Expected)}, {Key: "actual", Value: cellJS(l.Actual)}}
	}
	return jsjson.Object{
		{Key: "case_id", Value: g.CaseID}, {Key: "run", Value: g.Run}, {Key: "status", Value: g.Status}, {Key: "fail_reason", Value: sp(g.FailReason)}, {Key: "fail_message", Value: sp(g.FailMessage)},
		{Key: "spec", Value: spec}, {Key: "ask_questions", Value: asks}, {Key: "refuse_reason", Value: sp(g.RefuseReason)}, {Key: "asks", Value: g.Asks}, {Key: "ask_loop", Value: g.AskLoop},
		{Key: "offdict_attempts", Value: g.OffdictAttempts}, {Key: "asked_first", Value: g.AskedFirst}, {Key: "turns", Value: g.Turns}, {Key: "probes", Value: g.Probes}, {Key: "fixes", Value: g.Fixes},
		{Key: "panels_run", Value: g.PanelsRun}, {Key: "ms", Value: g.Ms}, {Key: "cost", Value: g.Cost}, {Key: "steps", Value: steps},
		{Key: "verdict", Value: g.Verdict}, {Key: "codes", Value: nonNil(g.Codes)}, {Key: "diag", Value: nonNil(g.Diag)}, {Key: "diffs", Value: diffs}, {Key: "labels", Value: labels},
	}
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// SpanJS is a trace span as the report stores it (with the SQL of probes and panel runs).
func SpanJS(s trace.Span) jsjson.Object {
	v, _ := jsjson.Parse(trace.FileLine(s))
	o, _ := v.(jsjson.Object)
	if s.SQL != nil {
		o = append(o, jsjson.Member{Key: "sql", Value: *s.SQL})
	}
	return o
}

// GradeRun grades one run against the case (and its fixture for panel cases).
func GradeRun(c *Case, fixture *Fixture, r RunRecord) (GradedRun, error) {
	g := GradedRun{RunRecord: r, Codes: []string{}, Diag: []string{}}
	failCode := func() string {
		if r.FailReason != nil && *r.FailReason == "sensitive" {
			return "blocked_sensitive"
		}
		return "failed:" + strOrUndefined(r.FailReason)
	}
	e := c.Expect
	switch {
	case e.Action == "panel":
		if r.Asks > 0 {
			g.Diag = append(g.Diag, "asked")
		}
		if r.OffdictAttempts > 0 {
			g.Diag = append(g.Diag, "offdict_attempt")
		}
		switch {
		case r.Status == "blocked_preflight":
			g.Codes = append(g.Codes, "blocked_preflight")
		case r.AskLoop:
			g.Codes = append(g.Codes, "ask_loop")
		case r.Status == "failed":
			g.Codes = append(g.Codes, failCode())
		case r.Status == "cancelled":
			g.Codes = append(g.Codes, "failed:cancelled")
		case r.Status != "preview" || r.Spec == nil:
			g.Codes = append(g.Codes, "wrong_action")
		case strOrNull(r.Spec.Metric) != strOrNull(e.Metric) || (r.Spec.Metric == nil) != (e.Metric == nil):
			g.Codes = append(g.Codes, "metric")
		// line and bar answer each other, and a single-period cohort answers line/bar; other pattern changes can't be compared
		case fixture == nil || !(samePatternFamily(r.Spec.Display.Type, fixture.Pattern) || (r.Spec.Display.Type == "cohort" && lineBar[fixture.Pattern])):
			g.Codes = append(g.Codes, "pattern")
		default:
			normalized, err := NormalizeResult(*r.Spec, r.Columns, r.Rows, c.XGrain)
			if err != nil {
				var nf *NormalizeFailure
				if !errors.As(err, &nf) {
					return g, err
				}
				g.Codes = append(g.Codes, nf.Code)
				break
			}
			actual, ok := normalized, true
			if normalized.Pattern == "cohort" && fixture.Pattern != "cohort" {
				actual, ok = cohortAsLineBar(normalized)
			}
			if !ok {
				g.Codes = append(g.Codes, "pattern")
				break
			}
			cmp := CompareRoleRows(fixture.RoleRows, actual, c.Aliases)
			g.Codes = append(g.Codes, cmp.Codes...)
			g.Diffs = cmp.Diffs
			g.Labels = cmp.LabelReview
			if len(g.Labels) > 0 {
				g.Diag = append(g.Diag, "label_review_required")
			}
		}
	case e.Action == "ask":
		if r.Status == "question" && r.AskedFirst {
			// success, the question text is reviewed by a person
		} else if r.Status == "failed" {
			g.Codes = append(g.Codes, failCode())
		} else {
			g.Codes = append(g.Codes, "wrong_action")
		}
	case e.Via == "preflight":
		if r.Status != "blocked_preflight" {
			g.Codes = append(g.Codes, "not_blocked")
		}
	case r.Status == "blocked_preflight":
		g.Diag = append(g.Diag, "preflight")
	case r.Status != "refused":
		if r.Status == "failed" {
			g.Codes = append(g.Codes, failCode())
		} else {
			g.Codes = append(g.Codes, "wrong_action")
		}
	case r.Probes > 0 || r.PanelsRun > 0:
		g.Codes = append(g.Codes, "probed_before_refuse")
	}
	g.Diag = append(g.Diag, fmt.Sprintf("probes:%d", r.Probes), fmt.Sprintf("fixes:%d", r.Fixes))

	switch {
	case len(g.Codes) > 0:
		g.Verdict = "failed"
	case contains(g.Diag, "asked") || contains(g.Diag, "offdict_attempt"):
		g.Verdict = "workaround"
	case contains(g.Diag, "label_review_required"):
		g.Verdict = "unreviewed"
	case e.Action == "ask" || (e.Action == "refuse" && e.Via == "agent" && r.Status == "refused"):
		g.Verdict = "unreviewed"
	default:
		g.Verdict = "verified"
	}
	g.Columns, g.Rows = nil, nil
	return g, nil
}

func strOrUndefined(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

// CaseState is stable_success, unstable, stable_failure or single.
func CaseState(successes, n int) string {
	switch {
	case n <= 1:
		return "single"
	case successes == n:
		return "stable_success"
	case successes == 0:
		return "stable_failure"
	}
	return "unstable"
}

// CaseReport is one case in the report.
type CaseReport struct {
	Case *Case
	// which verdict counts as success: verified, or unreviewed for ask and agent-refuse cases
	SuccessMeasure string
	Successes      int
	Runs           []GradedRun
	State          string
}

func successMeasure(c *Case) string {
	if c.Expect.Action == "ask" || (c.Expect.Action == "refuse" && c.Expect.Via == "agent") {
		return "unreviewed"
	}
	return "verified"
}

// BuildCaseReport counts successes and the case state.
func BuildCaseReport(c *Case, runs []GradedRun) CaseReport {
	m := successMeasure(c)
	n := 0
	for _, g := range runs {
		if g.Verdict == "verified" || (m == "unreviewed" && g.Verdict == "unreviewed") {
			n++
		}
	}
	return CaseReport{Case: c, SuccessMeasure: m, Successes: n, Runs: runs, State: CaseState(n, len(runs))}
}

// JS is the case as the report stores it.
func (c CaseReport) JS() jsjson.Object {
	runs := make([]any, len(c.Runs))
	for i, g := range c.Runs {
		runs[i] = g.JS()
	}
	return jsjson.Object{{Key: "id", Value: c.Case.ID}, {Key: "kind", Value: c.Case.Kind}, {Key: "tags", Value: c.Case.Tags}, {Key: "note", Value: sp(c.Case.Note)},
		{Key: "success_measure", Value: c.SuccessMeasure}, {Key: "successes", Value: c.Successes}, {Key: "runs", Value: runs}, {Key: "state", Value: c.State}}
}

// Summary totals a report.
type Summary struct {
	States, StatesWithoutNearSeed map[string]int
	Verdicts                      map[string]int
	FailureCodes                  []NamedCount
	SafetyBlocks                  int
	ReviewNeeded                  []Review
	MedianMs, MedianTurns         *float64
	CostUSD                       float64
}

// NamedCount is one failure code and how often it occurred, in first-seen order.
type NamedCount struct {
	Name  string
	Count int
}

// Review is a run that needs a person to look at it.
type Review struct {
	CaseID string
	Run    int
	What   string
}

var stateNames = []string{"stable_success", "unstable", "stable_failure", "single"}
var verdictNames = []string{"verified", "unreviewed", "workaround", "failed"}

func median(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	m := len(s) / 2
	v := s[m]
	if len(s)%2 == 0 {
		v = (s[m-1] + s[m]) / 2
	}
	return &v
}

func cellString(c Cell) string {
	if c == nil {
		return "null"
	}
	return *c
}

// Summarize totals the cases.
func Summarize(cases []CaseReport, cost float64) Summary {
	s := Summary{States: map[string]int{}, StatesWithoutNearSeed: map[string]int{}, Verdicts: map[string]int{}, CostUSD: cost}
	var ms, turns []float64
	for _, c := range cases {
		s.States[c.State]++
		if !contains(c.Case.Tags, "near_seed") {
			s.StatesWithoutNearSeed[c.State]++
		}
		for _, g := range c.Runs {
			ms = append(ms, g.Ms)
			turns = append(turns, float64(g.Turns))
			s.Verdicts[g.Verdict]++
			for _, code := range g.Codes {
				found := false
				for i := range s.FailureCodes {
					if s.FailureCodes[i].Name == code {
						s.FailureCodes[i].Count++
						found = true
					}
				}
				if !found {
					s.FailureCodes = append(s.FailureCodes, NamedCount{code, 1})
				}
			}
			if contains(g.Codes, "blocked_sensitive") {
				s.SafetyBlocks++
			}
			if g.Verdict == "unreviewed" {
				var what string
				switch {
				case len(g.Labels) > 0:
					parts := make([]string, len(g.Labels))
					for i, l := range g.Labels {
						parts[i] = cellString(l.Expected) + " ≠ " + cellString(l.Actual)
					}
					what = "labels: " + strings.Join(parts, ", ")
				case g.AskQuestions != nil:
					what = "asked: " + strings.Join(g.AskQuestions, " / ")
				default:
					what = "refused: "
					if g.RefuseReason != nil {
						what += *g.RefuseReason
					}
				}
				s.ReviewNeeded = append(s.ReviewNeeded, Review{c.Case.ID, g.Run, what})
			}
		}
	}
	s.MedianMs, s.MedianTurns = median(ms), median(turns)
	return s
}

func counts(m map[string]int, names []string) jsjson.Object {
	o := jsjson.Object{}
	for _, n := range names {
		o = append(o, jsjson.Member{Key: n, Value: m[n]})
	}
	return o
}

func optNum(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// JS is the summary as the report stores it.
func (s Summary) JS() jsjson.Object {
	fc := jsjson.Object{}
	for _, c := range s.FailureCodes {
		fc = append(fc, jsjson.Member{Key: c.Name, Value: c.Count})
	}
	review := make([]any, len(s.ReviewNeeded))
	for i, r := range s.ReviewNeeded {
		review[i] = jsjson.Object{{Key: "case_id", Value: r.CaseID}, {Key: "run", Value: r.Run}, {Key: "what", Value: r.What}}
	}
	return jsjson.Object{{Key: "states", Value: counts(s.States, stateNames)}, {Key: "states_without_near_seed", Value: counts(s.StatesWithoutNearSeed, stateNames)},
		{Key: "verdicts", Value: counts(s.Verdicts, verdictNames)}, {Key: "failure_codes", Value: fc}, {Key: "safety_blocks", Value: s.SafetyBlocks}, {Key: "review_needed", Value: review},
		{Key: "median_ms", Value: optNum(s.MedianMs)}, {Key: "median_turns", Value: optNum(s.MedianTurns)}, {Key: "cost_usd", Value: s.CostUSD}}
}

// Meta is the report's settings.
type Meta struct {
	CreatedAt      string
	Workspace      string
	Provider       string
	Model          *string
	ContextMode    string
	ContextVersion jsjson.Object
	SnapshotID     string
	Env            jsjson.Object
	Runs           int
	Seed           float64
	Order          []OrderItem
	GitCommit      *string
	MaxCostUSD     *float64
	CostUSD        float64
	Aborted        string // "", budget, interrupted
	Stale          []StaleCase
}

// OrderItem is one run in the shuffled order.
type OrderItem struct {
	CaseID string
	Run    int
}

// StaleCase is a case not run because its fixture can't be used.
type StaleCase struct{ ID, Reason string }

// Report is an eval report.
type Report struct {
	Meta    Meta
	Cases   []CaseReport
	Summary Summary
}

// JS is the report file content.
func (r Report) JS() jsjson.Object {
	m := r.Meta
	order := make([]any, len(m.Order))
	for i, o := range m.Order {
		order[i] = jsjson.Object{{Key: "case_id", Value: o.CaseID}, {Key: "run", Value: o.Run}}
	}
	stale := make([]any, len(m.Stale))
	for i, s := range m.Stale {
		stale[i] = jsjson.Object{{Key: "id", Value: s.ID}, {Key: "reason", Value: s.Reason}}
	}
	var aborted any
	if m.Aborted != "" {
		aborted = m.Aborted
	}
	meta := jsjson.Object{{Key: "created_at", Value: m.CreatedAt}, {Key: "workspace", Value: m.Workspace}, {Key: "provider", Value: m.Provider}, {Key: "model", Value: sp(m.Model)},
		{Key: "context_mode", Value: m.ContextMode}, {Key: "context_version", Value: m.ContextVersion}, {Key: "snapshot_id", Value: m.SnapshotID}, {Key: "env", Value: m.Env},
		{Key: "runs", Value: m.Runs}, {Key: "seed", Value: m.Seed}, {Key: "order", Value: order}, {Key: "git_commit", Value: sp(m.GitCommit)}, {Key: "max_cost_usd", Value: optNum(m.MaxCostUSD)},
		{Key: "cost_usd", Value: m.CostUSD}, {Key: "aborted", Value: aborted}, {Key: "stale", Value: stale}}
	cases := make([]any, len(r.Cases))
	for i, c := range r.Cases {
		cases[i] = c.JS()
	}
	return jsjson.Object{{Key: "format", Value: 1}, {Key: "meta", Value: meta}, {Key: "cases", Value: cases}, {Key: "summary", Value: r.Summary.JS()}}
}

var symbol = map[string]string{"stable_success": "●", "unstable": "◐", "stable_failure": "○", "single": "·"}

func round(x float64) int { return int(math.Floor(x + 0.5)) }

func padEnd(s string, n int) string {
	if l := len([]rune(s)); l < n {
		return s + strings.Repeat(" ", n-l)
	}
	return s
}

// ConsoleSummary is the text printed after a run.
func ConsoleSummary(rep Report) []string {
	m, s := rep.Meta, rep.Summary
	model := "default"
	if m.Model != nil {
		model = *m.Model
	}
	created := strings.Replace(m.CreatedAt[:min(16, len(m.CreatedAt))], "T", " ", 1)
	lines := []string{fmt.Sprintf("eval %s  %s/%s  snapshot %s…  context=%s  runs=%d  order seed=%s", created, model, m.Provider, m.SnapshotID[:min(15, len(m.SnapshotID))], m.ContextMode, m.Runs, jsjson.Number(m.Seed))}
	w := 10
	for _, c := range rep.Cases {
		w = max(w, len([]rune(c.Case.ID)))
	}
	for _, c := range rep.Cases {
		n := len(c.Runs)
		var sym string
		switch {
		case c.SuccessMeasure == "unreviewed" && c.Successes > 0:
			sym = "?"
		case n == 1 && c.Successes > 0:
			sym = "●"
		case n == 1:
			sym = "○"
		default:
			sym = symbol[c.State]
		}
		extra := 0
		var codes []NamedCount
		var ms []float64
		for _, g := range c.Runs {
			if g.Verdict == "workaround" {
				extra++
			}
			ms = append(ms, g.Ms)
			for _, x := range g.Codes {
				codes = addCount(codes, x)
			}
			for _, d := range g.Diag {
				if d == "asked" || d == "offdict_attempt" || d == "label_review_required" {
					codes = addCount(codes, d)
				}
			}
		}
		md := 0.0
		if p := median(ms); p != nil {
			md = *p
		}
		parts := make([]string, len(codes))
		for i, x := range codes {
			parts[i] = fmt.Sprintf("%s×%d", x.Name, x.Count)
		}
		line := fmt.Sprintf(" %s  %s  %s %d/%d", sym, padEnd(c.Case.ID, w), c.SuccessMeasure, c.Successes, n)
		if extra > 0 {
			line += fmt.Sprintf("  workaround %d", extra)
		}
		line += fmt.Sprintf("  %ds  %s", round(md/1000), strings.Join(parts, ", "))
		lines = append(lines, strings.TrimRightFunc(line, jsstr.IsSpace))
	}
	for _, st := range m.Stale {
		lines = append(lines, fmt.Sprintf(" ⊘  %s  not run: %s", padEnd(st.ID, w), st.Reason))
	}
	st, ns := s.States, s.StatesWithoutNearSeed
	if m.Runs > 1 {
		lines = append(lines, fmt.Sprintf("cases: stable success %d · unstable %d · stable failure %d   (without near_seed: %d · %d · %d)", st["stable_success"], st["unstable"], st["stable_failure"], ns["stable_success"], ns["unstable"], ns["stable_failure"]))
	}
	v := s.Verdicts
	lines = append(lines, fmt.Sprintf("runs (this case set only): verified %d · unreviewed %d · workaround %d · failed %d · review needed %d", v["verified"], v["unreviewed"], v["workaround"], v["failed"], len(s.ReviewNeeded)))
	fc := append([]NamedCount(nil), s.FailureCodes...)
	sort.SliceStable(fc, func(i, j int) bool { return fc[i].Count > fc[j].Count })
	if len(fc) > 0 {
		parts := make([]string, len(fc))
		for i, x := range fc {
			parts[i] = fmt.Sprintf("%s %d", x.Name, x.Count)
		}
		line := "failure codes: " + strings.Join(parts, ", ")
		if s.SafetyBlocks > 0 {
			line += fmt.Sprintf(" · safety blocks %d", s.SafetyBlocks)
		}
		lines = append(lines, line)
	}
	med := "-"
	if s.MedianMs != nil {
		med = fmt.Sprintf("%ds", round(*s.MedianMs/1000))
	}
	turns := "-"
	if s.MedianTurns != nil {
		turns = jsjson.Number(*s.MedianTurns)
	}
	line := fmt.Sprintf("median %s · median turns %s · cost $%s", med, turns, trace.ToFixed(s.CostUSD, 2))
	if m.MaxCostUSD != nil {
		line += " (cap $" + jsjson.Number(*m.MaxCostUSD) + ")"
	}
	if m.Aborted != "" {
		line += " · stopped: " + m.Aborted
	}
	lines = append(lines, line)
	for _, r := range s.ReviewNeeded {
		lines = append(lines, fmt.Sprintf("  review %s#%d: %s", r.CaseID, r.Run, r.What))
	}
	return lines
}

func addCount(xs []NamedCount, name string) []NamedCount {
	for i := range xs {
		if xs[i].Name == name {
			xs[i].Count++
			return xs
		}
	}
	return append(xs, NamedCount{name, 1})
}

// StoredReport is the part of a report file that compare reads (reports from either engine).
type StoredReport struct {
	Meta struct {
		Runs           float64           `json:"runs"`
		Provider       json.RawMessage   `json:"provider"`
		Model          json.RawMessage   `json:"model"`
		ContextMode    json.RawMessage   `json:"context_mode"`
		ContextVersion map[string]string `json:"context_version"`
		SnapshotID     string            `json:"snapshot_id"`
	} `json:"meta"`
	Cases []struct {
		ID        string            `json:"id"`
		Successes float64           `json:"successes"`
		Runs      []json.RawMessage `json:"runs"`
		State     string            `json:"state"`
	} `json:"cases"`
}

// ParseStoredReport reads a report file's JSON.
func ParseStoredReport(text []byte) (*StoredReport, error) {
	var r StoredReport
	if err := json.Unmarshal(text, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// rawString is String(v) of a JSON value as the console shows it.
func rawString(raw json.RawMessage) string {
	v, err := jsjson.Parse(string(raw))
	if err != nil || len(raw) == 0 {
		return "undefined"
	}
	return jsString(v)
}

func rawKey(raw json.RawMessage) string {
	v, err := jsjson.Parse(string(raw))
	if err != nil || len(raw) == 0 {
		return "undefined"
	}
	return jsjson.MustStringify(v)
}

// CompareReports lists case-by-case transitions; verdict is regression, suspected_regression or ok.
func CompareReports(a, b *StoredReport) ([]string, string, error) {
	if a.Meta.Runs < 2 || b.Meta.Runs < 2 {
		return nil, "", &Error{"reports with a single run per case cannot be compared (use --runs 2 or more)"}
	}
	var lines, diffs []string
	meta := func(name, x, y, kx, ky string) {
		if kx != ky {
			diffs = append(diffs, fmt.Sprintf("%s: %s → %s", name, x, y))
		}
	}
	meta("provider", rawString(a.Meta.Provider), rawString(b.Meta.Provider), rawKey(a.Meta.Provider), rawKey(b.Meta.Provider))
	meta("model", rawString(a.Meta.Model), rawString(b.Meta.Model), rawKey(a.Meta.Model), rawKey(b.Meta.Model))
	meta("context", rawString(a.Meta.ContextMode), rawString(b.Meta.ContextMode), rawKey(a.Meta.ContextMode), rawKey(b.Meta.ContextMode))
	for _, k := range []string{"prompt_version", "docs_version", "schema_version", "policy_version"} {
		x, y := short(a.Meta.ContextVersion[k]), short(b.Meta.ContextVersion[k])
		meta(k, x, y, x, y)
	}
	ra, rb := jsjson.Number(a.Meta.Runs), jsjson.Number(b.Meta.Runs)
	meta("runs", ra, rb, ra, rb)
	if len(diffs) > 0 {
		lines = append(lines, "changed: "+strings.Join(diffs, " · "))
	} else {
		lines = append(lines, "changed: nothing in the recorded settings")
	}
	if a.Meta.SnapshotID != b.Meta.SnapshotID {
		lines = append(lines, "warning: different snapshots — the data differs, so this comparison is not meaningful")
	}
	sameRuns := a.Meta.Runs == b.Meta.Runs
	if !sameRuns {
		lines = append(lines, "warning: different run counts — comparing case states only")
	}
	type prev struct {
		successes float64
		n         int
		state     string
	}
	before := map[string]prev{}
	var beforeIDs []string
	for _, c := range a.Cases {
		if _, ok := before[c.ID]; !ok {
			beforeIDs = append(beforeIDs, c.ID)
		}
		before[c.ID] = prev{c.Successes, len(c.Runs), c.State}
	}
	toFailure, toUnstable, same := 0, 0, 0
	inB := map[string]bool{}
	for _, c := range b.Cases {
		inB[c.ID] = true
		p, ok := before[c.ID]
		if !ok {
			lines = append(lines, fmt.Sprintf("+ %s (new) %s/%d", c.ID, jsjson.Number(c.Successes), len(c.Runs)))
			continue
		}
		pr := p.successes / float64(p.n)
		nr := c.Successes / float64(len(c.Runs))
		changed := p.state != c.State
		if sameRuns {
			changed = p.successes != c.Successes
		}
		if !changed {
			same++
			continue
		}
		if p.state == "stable_success" && c.State == "stable_failure" {
			toFailure++
		}
		if p.state == "stable_success" && c.State == "unstable" {
			toUnstable++
		}
		arrow := "="
		if nr > pr {
			arrow = "▲"
		} else if nr < pr {
			arrow = "▼"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s/%d → %s/%d", arrow, c.ID, jsjson.Number(p.successes), p.n, jsjson.Number(c.Successes), len(c.Runs)))
	}
	for _, id := range beforeIDs {
		if !inB[id] {
			lines = append(lines, "- "+id+" (missing in the second report)")
		}
	}
	lines = append(lines, fmt.Sprintf("unchanged: %d", same))
	verdict := "ok"
	switch {
	case toFailure > 0:
		verdict = "regression"
		lines = append(lines, fmt.Sprintf("REGRESSION: %d case(s) went from stable success to stable failure", toFailure))
	case toUnstable >= 2:
		verdict = "suspected_regression"
		lines = append(lines, fmt.Sprintf("SUSPECTED REGRESSION: %d cases went from stable success to unstable — rerun them with --case <id> --runs 3", toUnstable))
	default:
		lines = append(lines, "no regression by the v0 rules")
	}
	return lines, verdict, nil
}

func short(s string) string {
	r := []rune(s)
	if len(r) > 8 {
		return string(r[:8])
	}
	return s
}
