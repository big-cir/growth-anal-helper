package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"growth-lab/internal/coverage"
	"growth-lab/internal/panels"
)

func panelCall(t *testing.T, sql string) CallResult {
	raw := `{"step": {"action": "panel", "plan": "p", "panel": {"metric": "demo_metric", "title": "t", "question": "q", "sql": ` + strings.TrimSpace(mustJSON(sql)) +
		`, "display": {"type": "number", "x": null, "numerator": "numerator", "denominator": "denominator", "series": null, "headline": null}, "definition": [["Population", "x"]], "caveats": [], "answers": []}}}`
	return CallResult{OK: true, SessionID: "s", Structured: parse(t, json.RawMessage(raw)), CostUsd: 0.01, Ms: 1}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// runCoverage runs a request whose agent sends the given SQLs in turn; every panel run reports a gap finding.
func runCoverage(t *testing.T, sqls []string) (calls []string, final *Event) {
	var i int
	req := NewRequest("r", LoopDeps{
		Call: func(_ context.Context, input, _ string, _ float64) CallResult {
			calls = append(calls, input)
			if i >= len(sqls) {
				return CallResult{Type: "error", Message: "script ended", Ms: 1}
			}
			i++
			return panelCall(t, sqls[i-1])
		},
		Probe: func(context.Context, string) QueryResult { return QueryResult{} },
		Panel: func(context.Context, panels.Spec) PanelResult {
			return PanelResult{OK: true, Columns: []panels.Column{{Name: "numerator"}, {Name: "denominator"}}, Real: []panels.Row{{1.0, 2.0}}, Agent: []panels.Row{{1.0, 2.0}}, Tables: []string{"e_posts"},
				Signals: []coverage.Signal{{Rule: "gap_dependent", Ask: true, Agent: "uses pre-gap rows", Caveat: "pre-gap caveat"}}}
		},
		Sleep:    func(context.Context, int) {},
		Outbound: &Outbound{Mode: "pseudonymized"},
		Limits:   Limits{MaxTurns: 8, MaxProbes: 4, MaxFixes: 2, CallBudgetUsd: 0.5, RequestBudgetUsd: 1.0},
		Emit: func(ev Event, _ int) {
			if ev.Type == "preview" {
				e := ev
				final = &e
			}
		},
		HeartbeatMs: 2000000000,
		Lang:        "en",
	})
	req.Run(StartInput{Text: "q"})
	return calls, final
}

func TestCoverageAskThenSamePanelAccepted(t *testing.T) {
	calls, final := runCoverage(t, []string{"SELECT 1 AS numerator, 2 AS denominator", "SELECT 1 AS numerator, 2 AS denominator"})
	if len(calls) != 2 || !strings.Contains(calls[1], "uses pre-gap rows") {
		t.Fatalf("calls %q", calls)
	}
	if final == nil || len(final.Spec.Caveats) != 1 || final.Spec.Caveats[0] != "pre-gap caveat" {
		t.Fatalf("final %+v", final)
	}
}

func TestCoverageAsksAtMostTwice(t *testing.T) {
	calls, final := runCoverage(t, []string{"SELECT 1 AS numerator, 2 AS denominator", "SELECT 2 AS numerator, 2 AS denominator", "SELECT 3 AS numerator, 2 AS denominator"})
	if len(calls) != 3 || !strings.Contains(calls[2], "uses pre-gap rows") {
		t.Fatalf("calls %q", calls)
	}
	if final == nil || !strings.Contains(final.Spec.SQL, "SELECT 3") || final.Spec.Caveats[0] != "pre-gap caveat" {
		t.Fatalf("third panel not accepted with a caveat: %+v", final)
	}
}
