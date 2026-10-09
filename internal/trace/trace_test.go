package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
)

type vectors struct {
	Lines []struct {
		Span json.RawMessage `json:"span"`
		Line string          `json:"line"`
	} `json:"lines"`
	Reads []struct {
		Days  int      `json:"days"`
		Now   int64    `json:"now"`
		Lines []string `json:"lines"`
	} `json:"reads"`
	Empty    []string `json:"empty"`
	Slowest2 []string `json:"slowest2"`
	Tracer   []struct {
		Steps [][]json.RawMessage `json:"steps"`
		Spans []string            `json:"spans"`
	} `json:"tracer"`
}

func load(t *testing.T) vectors {
	var v vectors
	if err := contract.Load("misc-trace.json", &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func eq(t *testing.T, label string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got:\n%s\nwant:\n%s", label, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestFileLinesAndReports(t *testing.T) {
	v := load(t)
	dir := t.TempDir()
	log := NewLog(dir)
	var all []Span
	for _, l := range v.Lines {
		var s Span
		if err := json.Unmarshal(l.Span, &s); err != nil {
			t.Fatal(err)
		}
		if got := FileLine(s); got != l.Line {
			t.Errorf("line:\n got %s\nwant %s", got, l.Line)
		}
		log.Write(s)
		all = append(all, s)
	}
	fi, err := os.Stat(filepath.Join(log.Dir, "2026-10-01.jsonl"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode: %v %v", fi, err)
	}
	for _, r := range v.Reads {
		eq(t, "read", Report(dir, r.Days, r.Now)[1:], r.Lines)
	}
	eq(t, "empty", Summarize(nil, 5), v.Empty)
	eq(t, "slowest 2", Summarize(all, 2), v.Slowest2)
	t.Logf("%d lines, %d reports", len(v.Lines), len(v.Reads))
}

func TestTracer(t *testing.T) {
	v := load(t)
	for i, sc := range v.Tracer {
		clock := time.UnixMilli(1791515045678).UTC()
		now := func() time.Time { return clock }
		var got []string
		conv := "c1"
		tr := NewTracer("r1", &conv, "claude-code", nil, func(s Span) {
			line := FileLine(s)
			o, _ := jsjson.Parse(line)
			obj := o.(jsjson.Object)
			if s.SQL != nil {
				obj = append(obj, jsjson.Member{Key: "sql", Value: *s.SQL})
			}
			got = append(got, jsjson.MustStringify(obj))
		}, now)
		for _, st := range sc.Steps {
			var kind string
			var d float64
			_ = json.Unmarshal(st[0], &kind)
			_ = json.Unmarshal(st[1], &d)
			elapse := func() { clock = clock.Add(time.Duration(d) * time.Millisecond) }
			t0 := clock
			switch kind {
			case "call":
				var r struct {
					OK         bool     `json:"ok"`
					Type       string   `json:"type"`
					Ms         float64  `json:"ms"`
					APIMs      *float64 `json:"apiMs"`
					CostUSD    float64  `json:"costUsd"`
					Usage      *Usage   `json:"usage"`
					Structured *struct {
						Step *struct {
							Action any `json:"action"`
						} `json:"step"`
					} `json:"structured"`
				}
				_ = json.Unmarshal(st[2], &r)
				var action any
				if r.OK && r.Structured != nil && r.Structured.Step != nil {
					action = r.Structured.Step.Action
				}
				elapse()
				tr.AgentCall(t0, Call{OK: r.OK, RunMs: r.Ms, APIMs: r.APIMs, Usage: r.Usage, CostUSD: r.CostUSD, Action: action, ErrType: r.Type})
			case "probe", "panel":
				var sql string
				_ = json.Unmarshal(st[2], &sql)
				elapse()
				if kind == "probe" {
					tr.Probe(t0, sql != "bad", 3, "lint", sql)
				} else {
					tr.Panel(t0, sql != "bad", 2, "exec", sql)
				}
			default:
				var extra struct {
					Reason string `json:"reason"`
					Spec   *struct {
						Metric *string `json:"metric"`
					} `json:"spec"`
				}
				if len(st) > 2 {
					_ = json.Unmarshal(st[2], &extra)
				}
				elapse()
				tr.Event(kind, extra.Spec != nil && extra.Spec.Metric == nil, extra.Reason)
			}
		}
		eq(t, "tracer "+string(rune('0'+i)), got, sc.Spans)
	}
}

func TestToFixed(t *testing.T) {
	for _, c := range []struct {
		x    float64
		d    int
		want string
	}{{1.25, 1, "1.3"}, {0.25, 1, "0.3"}, {1.35, 1, "1.4"}, {1.45, 1, "1.4"}, {0.0125, 3, "0.013"}, {0, 1, "0.0"}, {-0.04, 1, "-0.0"}, {-1.25, 1, "-1.3"}, {123456.789, 0, "123457"}, {0.5, 0, "1"}} {
		if got := ToFixed(c.x, c.d); got != c.want {
			t.Errorf("ToFixed(%v, %d) = %s, want %s", c.x, c.d, got, c.want)
		}
	}
}
