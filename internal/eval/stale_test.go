package eval_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"growth-lab/internal/eval"
)

// The demo data ends on the anchor day; a source clock five days later makes a snapshot whose events end early.
const lateNow = "2024-06-08 12:00:00.000000"

func (e *env) setEvents() {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, "coverage.json"), []byte(`{"events": [{"table": "r_post", "time": "created_at"}, {"table": "r_reaction", "time": "created_at"}]}`), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) freeze() ([]eval.FreezeResult, error) {
	e.t.Helper()
	a, err := eval.NewApp(e.ws, "")
	if err != nil {
		e.t.Fatal(err)
	}
	ids, _ := eval.MetricIDs(a)
	cases, problems, err := eval.LoadCases(e.dir, ids)
	if err != nil || len(problems) > 0 {
		e.t.Fatal(err, problems)
	}
	return eval.FreezeCases(a, cases)
}

func TestFreezeStopsOnStaleSnapshot(t *testing.T) {
	e := newEnvAt(t, lateNow)
	e.setEvents()
	file := e.fixtureFile("board_join_12w")
	_ = os.Remove(file)
	e.ws.Config.Agent.CoverageCheck = false
	r, err := e.freeze()
	var stale *eval.StaleSnapshotError
	if !errors.As(err, &stale) || len(r) != 0 {
		t.Fatalf("freeze on a stale snapshot: %+v %v", r, err)
	}
	for _, want := range []string{"event records end at 2024-06-03 ", "last complete day 2024-06-02", "day before the cutoff 2024-06-07", "run collect"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("reason lacks %q: %s", want, err)
		}
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("fixture written on a stale snapshot")
	}
	e.ws.Config.Agent.CoverageCheck = true
	cmd := exec.Command(engine, "eval", "freeze")
	cmd.Env = append(os.Environ(), "GROWTH_LAB_WORKSPACE="+e.dir)
	out, _ := cmd.CombinedOutput()
	if code := cmd.ProcessState.ExitCode(); code != 1 || !strings.Contains(string(out), "snapshot data is not complete through the day before the cutoff") {
		t.Fatalf("eval freeze (%d): %s", code, out)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("fixture written by eval freeze on a stale snapshot")
	}
}

func TestFreezeOnCompleteSnapshot(t *testing.T) {
	e := newEnv(t)
	e.setEvents()
	file := e.fixtureFile("board_join_12w")
	_ = os.Remove(file)
	if r, err := e.freeze(); err != nil || len(r) != 1 || !r[0].OK {
		t.Fatalf("freeze: %+v %v", r, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("no fixture")
	}
}

func TestRunOnStaleSnapshotSkipsCasesWithoutFixture(t *testing.T) {
	// board_join_12w was frozen before coverage.json named the event tables; the copy has no fixture
	e := newEnvAt(t, lateNow)
	e.setEvents()
	b, err := os.ReadFile(filepath.Join(e.dir, "eval", "cases", "board_join_12w.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(e.dir, "eval", "cases", "board_join_copy.json"), []byte(strings.Replace(string(b), `"id": "board_join_12w"`, `"id": "board_join_copy"`, 1)), 0o644)
	sql, _ := reference(t)["sql"].(string)
	e.script(iso, panelStep(t, sql, "line", nil))
	r := e.run(eval.Options{Tag: "core", Runs: f(1)})
	if len(r.Meta.Stale) != 1 || r.Meta.Stale[0].ID != "board_join_copy" || !strings.HasPrefix(r.Meta.Stale[0].Reason, "the automatic freeze stopped: snapshot data is not complete") || len(r.Meta.AutoFrozen) != 0 {
		t.Fatalf("stale %+v, auto %+v", r.Meta.Stale, r.Meta.AutoFrozen)
	}
	if len(r.Cases) != 1 || r.Cases[0].Case.ID != "board_join_12w" || r.Cases[0].Runs[0].Verdict != "verified" {
		t.Fatalf("cases %+v", r.Cases)
	}
	if e.calls() != 2 {
		t.Fatalf("%d agent calls", e.calls())
	}
	if _, err := os.Stat(e.fixtureFile("board_join_copy")); err == nil {
		t.Fatal("fixture made on a stale snapshot")
	}
}
