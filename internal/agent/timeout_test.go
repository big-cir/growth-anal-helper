package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/query"
	"growth-lab/internal/sqlitec"
)

const (
	panelStep  = `{"step":{"action":"panel","plan":"counted","panel":{"metric":"demo_metric","title":"t","question":"q","sql":"SELECT 1 AS numerator, 2 AS denominator","display":{"type":"number","x":null,"numerator":"numerator","denominator":"denominator","series":null,"headline":null},"definition":[["Population","x"]],"caveats":[],"answers":[]}}}`
	probeStep  = `{"step":{"action":"probe","plan":"check counts","purpose":"cohort size","sql":"SELECT count(*) FROM d_x"}}`
	refuseStep = `{"step":{"action":"refuse","reason":"stop","alternatives":[]}}`
)

var (
	okPanel      = PanelResult{OK: true, Columns: []panels.Column{{Name: "numerator"}, {Name: "denominator"}}, Real: []panels.Row{{1.0, 2.0}}, Agent: []panels.Row{{1.0, 2.0}}, Tables: []string{"d_x"}}
	timeoutPanel = PanelResult{Stage: "timeout", Message: "Query did not finish within the time limit (60s)"}
	timeoutProbe = QueryResult{Kind: "timeout", Message: "Query did not finish within the time limit (60s)"}
)

// timeoutLoop runs a scripted request; panel and probe results are taken in order.
type timeoutLoop struct {
	steps      []string
	probes     []QueryResult
	panels     []PanelResult
	inputs     []string
	events     []Event
	indexCalls []bool
	noIndexes  bool
	onPanel    func(ctx context.Context)
	req        *Request
}

func (l *timeoutLoop) run(t *testing.T) *Request {
	t.Helper()
	deps := LoopDeps{
		Call: func(_ context.Context, input, _ string, _ float64) CallResult {
			l.inputs = append(l.inputs, input)
			if len(l.steps) == 0 {
				return CallResult{Type: "error", Message: "script ended"}
			}
			s, err := jsjson.Parse(l.steps[0])
			if err != nil {
				t.Fatal(err)
			}
			l.steps = l.steps[1:]
			return CallResult{OK: true, SessionID: "sess-1", Structured: s, CostUsd: 0.01}
		},
		Probe: func(context.Context, string) QueryResult {
			r := l.probes[0]
			l.probes = l.probes[1:]
			return r
		},
		Panel: func(ctx context.Context, _ panels.Spec) PanelResult {
			if l.onPanel != nil {
				l.onPanel(ctx)
			}
			r := l.panels[0]
			l.panels = l.panels[1:]
			return r
		},
		Sleep:       func(context.Context, int) {},
		Outbound:    &Outbound{Mode: "pseudonymized"},
		Limits:      Limits{MaxTurns: 8, MaxProbes: 4, MaxFixes: 2, CallBudgetUsd: 0.5, RequestBudgetUsd: 1},
		Emit:        func(ev Event, _ int) { l.events = append(l.events, ev) },
		HeartbeatMs: 2000000000,
		Lang:        "en",
	}
	if !l.noIndexes {
		deps.Indexes = func(panel bool) []string {
			l.indexCalls = append(l.indexCalls, panel)
			if panel {
				return []string{"d_x(a)"}
			}
			return []string{"d_x(a)", "r_y(b, c)"}
		}
	}
	l.req = NewRequest("r", deps)
	l.req.Run(StartInput{Text: "Show the connection rate by signup week"})
	return l.req
}

func (l *timeoutLoop) failed() *Event {
	for i := range l.events {
		if l.events[i].Type == "failed" {
			return &l.events[i]
		}
	}
	return nil
}

func (l *timeoutLoop) hasStep(text string) bool {
	for _, ev := range l.events {
		if ev.Type == "step" && ev.Kind == "check_failed" && ev.Text == text {
			return true
		}
	}
	return false
}

const rewriting = "Query did not finish within 60s → rewriting a lighter query"

func TestPanelTimeoutGetsOneRewrite(t *testing.T) {
	l := &timeoutLoop{steps: []string{panelStep, panelStep}, panels: []PanelResult{timeoutPanel, okPanel}}
	req := l.run(t)
	if req.State() != "done" || req.Fixes != 1 || req.Timeouts != 1 {
		t.Fatalf("state %s, fixes %d, timeouts %d", req.State(), req.Fixes, req.Timeouts)
	}
	if !l.hasStep(rewriting) {
		t.Errorf("no rewrite step: %+v", l.events)
	}
	reply := l.inputs[1]
	for _, want := range []string{"Panel check failed — stage: query timeout. Fixes left: 1.", "did not finish within the time limit (60s)", "aggregate first in a WITH clause", "Do not wrap indexed columns in functions",
		"Narrow the period", "One more query over the time limit ends the request.", "Indexed columns:\n- d_x(a)"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply lacks %q:\n%s", want, reply)
		}
	}
	if strings.Contains(reply, "r_y") || len(l.indexCalls) != 1 || !l.indexCalls[0] {
		t.Errorf("panel reply must list panel tables only: calls %v\n%s", l.indexCalls, reply)
	}
}

func TestSecondPanelTimeoutStops(t *testing.T) {
	l := &timeoutLoop{steps: []string{panelStep, panelStep, panelStep}, panels: []PanelResult{timeoutPanel, timeoutPanel, okPanel}}
	req := l.run(t)
	f := l.failed()
	if req.State() != "failed" || f == nil || f.Reason != "query_timeout" {
		t.Fatalf("state %s, failed %+v", req.State(), f)
	}
	if !strings.Contains(f.Message, "within 60s") || !strings.Contains(f.Message, "last 4 weeks") {
		t.Errorf("message: %s", f.Message)
	}
	if len(l.inputs) != 2 {
		t.Errorf("agent calls: %d", len(l.inputs))
	}
}

func TestProbeTimeoutCountsTowardsTheLimit(t *testing.T) {
	l := &timeoutLoop{steps: []string{probeStep, panelStep, panelStep}, probes: []QueryResult{timeoutProbe}, panels: []PanelResult{timeoutPanel, okPanel}}
	req := l.run(t)
	if f := l.failed(); req.State() != "failed" || f == nil || f.Reason != "query_timeout" {
		t.Fatalf("state %s, failed %+v", req.State(), f)
	}
}

func TestProbeTimeoutReply(t *testing.T) {
	l := &timeoutLoop{steps: []string{probeStep, refuseStep}, probes: []QueryResult{timeoutProbe}}
	req := l.run(t)
	if req.State() != "done" || req.Probes != 1 || req.Timeouts != 1 {
		t.Fatalf("state %s, probes %d, timeouts %d", req.State(), req.Probes, req.Timeouts)
	}
	reply := l.inputs[1]
	for _, want := range []string{"Probe query failed (timeout).", "aggregate first in a WITH clause", "Indexed columns:\n- d_x(a)\n- r_y(b, c)", "Probes left: 3."} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply lacks %q:\n%s", want, reply)
		}
	}
	if len(l.indexCalls) != 1 || l.indexCalls[0] {
		t.Errorf("index calls: %v", l.indexCalls)
	}
	if !l.hasStep(rewriting) {
		t.Errorf("no rewrite step")
	}
}

// A cancel that lands while a panel times out ends the request as cancelled, without a failure.
func TestCancelDuringPanelTimeout(t *testing.T) {
	l := &timeoutLoop{steps: []string{panelStep, panelStep}, panels: []PanelResult{timeoutPanel, okPanel}}
	l.onPanel = func(ctx context.Context) {
		go l.req.Cancel()
		<-ctx.Done()
	}
	req := l.run(t)
	for i := 0; req.State() != "cancelled"; i++ {
		if i == 200 {
			t.Fatalf("state %s", req.State())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if f := l.failed(); f != nil || req.Timeouts != 0 || len(l.inputs) != 1 {
		t.Errorf("failed %+v, timeouts %d, calls %d", f, req.Timeouts, len(l.inputs))
	}
}

func TestTimeoutReplyWithoutIndexes(t *testing.T) {
	l := &timeoutLoop{steps: []string{panelStep, panelStep}, panels: []PanelResult{timeoutPanel, okPanel}, noIndexes: true}
	req := l.run(t)
	if req.State() != "done" || strings.Contains(l.inputs[1], "Indexed columns") || !strings.Contains(l.inputs[1], "query timeout") {
		t.Fatalf("state %s:\n%s", req.State(), l.inputs[1])
	}
}

func TestIndexedColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sqlite")
	db, err := sqlitec.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	sql := `CREATE TABLE r_doc (id INTEGER PRIMARY KEY, kind TEXT, member_id INTEGER);
		CREATE TABLE d_member (id INTEGER PRIMARY KEY, app TEXT, at TEXT, note TEXT);
		CREATE TABLE x_other (k TEXT); CREATE TABLE d_hidden (k TEXT);
		INSERT INTO r_doc VALUES (1, 'a', 1); INSERT INTO d_member VALUES (1, 'x', '2024-01-01', 'n');
		CREATE INDEX r_doc_kind ON r_doc(kind, member_id);
		CREATE INDEX d_member_app ON d_member(app); CREATE INDEX d_member_app2 ON d_member(app);
		CREATE INDEX d_member_note ON d_member(at, note); CREATE INDEX x_other_k ON x_other(k); CREATE INDEX d_hidden_k ON d_hidden(k);
		CREATE INDEX d_member_expr ON d_member(lower(app));`
	for i := 0; i < 70; i++ {
		sql += fmt.Sprintf("CREATE TABLE d_t%02d (c TEXT); CREATE INDEX d_t%02d_c ON d_t%02d(c);", i, i, i)
	}
	if err := db.Exec(sql + "PRAGMA analysis_limit = 0; ANALYZE"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	blocked := query.BlockedList{Columns: []string{"d_member.note"}, Tables: []string{"d_hidden"}}
	probe := IndexedColumns(path, []string{"r_", "d_", "s"}, blocked)
	if len(probe) != MaxIndexLines {
		t.Errorf("lines: %d", len(probe))
	}
	if probe[0] != "d_member(app)" || probe[len(probe)-1] != "d_t58(c)" {
		t.Errorf("order: %v", probe)
	}
	panel := IndexedColumns(path, []string{"d_"}, blocked)
	joined := strings.Join(probe, "\n") + "\n" + strings.Join(panel, "\n")
	for _, bad := range []string{"note", "d_hidden", "x_other", "sqlite_", "lower"} {
		if strings.Contains(joined, bad) {
			t.Errorf("list includes %s:\n%s", bad, joined)
		}
	}
	if strings.Count(joined, "d_member(app)") != 2 {
		t.Errorf("duplicate or missing d_member(app):\n%s", joined)
	}
	all := IndexedColumns(path, []string{"r_"}, blocked)
	if len(all) != 1 || all[0] != "r_doc(kind, member_id)" {
		t.Errorf("probe tables: %v", all)
	}
	for _, line := range panel {
		if !strings.HasPrefix(line, "d_") {
			t.Errorf("panel list has %s", line)
		}
	}
	if IndexedColumns(filepath.Join(t.TempDir(), "missing.sqlite"), []string{"d_"}, blocked) != nil {
		t.Errorf("missing file gave a list")
	}
}
