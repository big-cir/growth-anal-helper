package eval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"growth-lab/internal/demo"
	"growth-lab/internal/eval"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/query"
	"growth-lab/internal/snapshot"
	"growth-lab/internal/workspace"
)

const (
	anchor = "2024-06-03 12:00:00"
	cutoff = "2024-06-03 12:00:00.000000"
)

var root, engine, fakeBin string

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	root = filepath.Join(filepath.Dir(file), "..", "..")
	binDir, err := os.MkdirTemp("", "gl-eval-bin-")
	if err != nil {
		panic(err)
	}
	engine = filepath.Join(binDir, "growth-lab")
	fakeBin = filepath.Join(binDir, "fakeclaude")
	for _, c := range [][]string{
		{filepath.Join(root, "scripts", "go"), "build", "-o", engine, "./cmd/growth-lab"},
		{"go", "build", "-o", fakeBin, "./test/e2e/fakeclaude"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
			os.Exit(1)
		}
	}
	query.WorkerCommand = func() (string, []string, error) { return engine, []string{"query-worker"}, nil }
	code := m.Run()
	os.RemoveAll(binDir)
	os.Exit(code)
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	_ = os.MkdirAll(to, 0o755)
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			copyDir(t, filepath.Join(from, e.Name()), filepath.Join(to, e.Name()))
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type env struct {
	t   *testing.T
	dir string
	ws  *workspace.Workspace
	// report file of the last run
	report string
}

// newEnv builds the demo workspace with its eval cases, a snapshot and frozen fixtures.
func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	demoDir := filepath.Join(root, "examples", "demo")
	for _, f := range []string{"tables.json", "derived.sql", "derived-columns.json", "metrics.json", "guide.md"} {
		b, _ := os.ReadFile(filepath.Join(demoDir, f))
		_ = os.WriteFile(filepath.Join(dir, f), b, 0o644)
	}
	copyDir(t, filepath.Join(demoDir, "seed-panels"), filepath.Join(dir, "seed-panels"))
	copyDir(t, filepath.Join(demoDir, "eval"), filepath.Join(dir, "eval"))
	fakeDir := filepath.Join(dir, "fake")
	_ = os.MkdirAll(fakeDir, 0o755)
	wrapper := filepath.Join(fakeDir, "claude")
	_ = os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nFAKE_CLAUDE_SCRIPT=%q FAKE_CLAUDE_DIR=%q exec %q \"$@\"\n", filepath.Join(fakeDir, "argv.txt"), filepath.Join(fakeDir, "script.json"), fakeDir, fakeBin)), 0o755)
	cfg := map[string]any{
		"name": "demo-board", "outDir": ".out",
		"datasource": map[string]any{"host": "sqlite://.out/source.sqlite"},
		"policy":     map[string]any{"readablePrefixes": []string{"r_", "d_", "snapshot_"}},
		"params":     map[string]any{"cohort_start": "2024-01-01 00:00:00.000000", "calendar_start": "2024-01-01 00:00:00.000000", "quality_min_ts": "2020-01-01 00:00:00.000000"},
		"agent":      map[string]any{"bin": wrapper, "callTimeoutMs": 10000},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "workspace.json"), b, 0o644)
	_ = os.MkdirAll(filepath.Join(dir, ".out"), 0o755)
	if _, err := demo.Seed(filepath.Join(dir, ".out", "source.sqlite"), demo.Options{Anchor: anchor}); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROWTH_LAB_TEST_SOURCE_NOW", cutoff)
	if _, err := snapshot.RunCollect(ws, nil, nil); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, ws: ws}
	e.script(iso)
	a, err := eval.NewApp(ws, "")
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := eval.MetricIDs(a)
	cases, problems, err := eval.LoadCases(dir, ids)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	r, err := eval.FreezeCases(a, cases)
	if err != nil || len(r) != 1 || !r[0].OK {
		t.Fatalf("freeze: %+v %v", r, err)
	}
	return e
}

var iso = map[string]any{"structured": map[string]any{"action": "refuse", "reason": "ok", "alternatives": []string{}}}

func (e *env) script(steps ...map[string]any) {
	b, _ := json.Marshal(steps)
	fake := filepath.Join(e.dir, "fake")
	_ = os.WriteFile(filepath.Join(fake, "script.json"), b, 0o644)
	_ = os.WriteFile(filepath.Join(fake, "state.json"), []byte(`{"calls":0}`), 0o644)
	_ = os.WriteFile(filepath.Join(fake, "argv.txt"), nil, 0o644)
}

func (e *env) calls() int {
	b, _ := os.ReadFile(filepath.Join(e.dir, "fake", "state.json"))
	var s struct{ Calls int }
	_ = json.Unmarshal(b, &s)
	return s.Calls
}

func reference(t *testing.T) map[string]any {
	b, err := os.ReadFile(filepath.Join(root, "examples", "demo", "eval", "cases", "reference", "board_join_12w.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func panelStep(t *testing.T, sql, display string, extra map[string]any) map[string]any {
	p := reference(t)
	p["sql"] = sql
	p["display"] = map[string]any{"type": display, "x": "x", "numerator": "numerator", "denominator": "denominator", "series": nil, "headline": nil}
	for k, v := range extra {
		p[k] = v
	}
	return map[string]any{"structured": map[string]any{"action": "panel", "plan": "Weekly join rate", "panel": p}}
}

var askStep = map[string]any{"structured": map[string]any{"action": "ask", "questions": []any{map[string]any{"id": "win", "text": "Which window?", "options": []any{map[string]any{"label": "7 days", "is_default": true}, map[string]any{"label": "14 days", "is_default": false}}, "allow_free_text": false}}}}

func f(x float64) *float64 { return &x }

func (e *env) run(o eval.Options) eval.Report {
	e.t.Helper()
	rep, path, err := eval.Run(e.ws, o)
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(b), "{\n  \"format\": 1,") {
		e.t.Fatalf("report file: %v", err)
	}
	e.report = string(b)
	return rep
}

func TestRunner(t *testing.T) {
	e := newEnv(t)
	sql, _ := reference(t)["sql"].(string)

	e.script(iso, panelStep(t, sql, "line", nil))
	r := e.run(eval.Options{CaseID: "board_join_12w", Runs: f(2), Seed: f(1)})
	if c := r.Cases[0]; c.Successes != 2 || c.State != "stable_success" {
		t.Fatalf("correct SQL: %+v", c.Runs[0].Codes)
	}
	if seeds, _ := filepath.Glob(filepath.Join(e.dir, "seed-panels", "*.json")); r.Meta.SeedsInPrompt != len(seeds) || r.Meta.SystemPromptChars == 0 {
		t.Fatalf("prompt meta: %d seeds of %d, %d chars", r.Meta.SeedsInPrompt, len(seeds), r.Meta.SystemPromptChars)
	}
	e.script(iso, panelStep(t, strings.Replace(sql, "sum(board_state = 'reached')", "max(sum(board_state = 'reached') - 1, 0)", 1), "line", nil))
	if c := e.run(eval.Options{CaseID: "board_join_12w", Runs: f(1)}).Cases[0]; strings.Join(c.Runs[0].Codes, ",") != "value_diff" {
		t.Fatalf("changed numbers: %v", c.Runs[0].Codes)
	}
	e.script(iso, panelStep(t, strings.Replace(sql, "'-77 days'", "'-84 days'", 1), "line", nil))
	if c := e.run(eval.Options{CaseID: "board_join_12w", Runs: f(1)}).Cases[0]; strings.Join(c.Runs[0].Codes, ",") != "rows_extra" {
		t.Fatalf("one more week: %v", c.Runs[0].Codes)
	}
	e.script(iso, panelStep(t, sql, "bar", nil))
	if c := e.run(eval.Options{CaseID: "board_join_12w", Runs: f(1)}).Cases[0]; c.Runs[0].Verdict != "verified" {
		t.Fatalf("bar answers line: %+v", c.Runs[0].Codes)
	}

	e.script(iso, askStep, panelStep(t, sql, "line", nil))
	g := e.run(eval.Options{CaseID: "board_join_12w", Runs: f(1)}).Cases[0].Runs[0]
	if g.Verdict != "workaround" || !contains(g.Diag, "asked") {
		t.Fatalf("answered question: %s %v", g.Verdict, g.Diag)
	}
	e.script(iso, askStep)
	if g := e.run(eval.Options{CaseID: "board_join_12w", Runs: f(1)}).Cases[0].Runs[0]; strings.Join(g.Codes, ",") != "ask_loop" {
		t.Fatalf("ask loop: %v", g.Codes)
	}
	e.script(iso, askStep)
	r = e.run(eval.Options{CaseID: "ambiguous_retention", Runs: f(1)})
	if r.Cases[0].Runs[0].Verdict != "unreviewed" || len(r.Summary.ReviewNeeded) != 1 || r.Summary.ReviewNeeded[0].What != "asked: Which window?" {
		t.Fatalf("ask case: %+v", r.Summary.ReviewNeeded)
	}
	e.script(iso)
	r = e.run(eval.Options{CaseID: "refuse_member_emails", Runs: f(2)})
	if r.Cases[0].Successes != 2 || e.calls() != 1 {
		t.Fatalf("preflight refuse: %d successes, %d calls", r.Cases[0].Successes, e.calls())
	}

	e.script(iso, panelStep(t, sql, "line", map[string]any{"metric": nil}), panelStep(t, sql, "line", nil))
	g = e.run(eval.Options{CaseID: "board_join_12w", Runs: f(1)}).Cases[0].Runs[0]
	if g.Verdict != "workaround" || !contains(g.Diag, "offdict_attempt") {
		t.Fatalf("off-dictionary: %s %v", g.Verdict, g.Diag)
	}
	e.script(iso, panelStep(t, "SELECT 'token=abc123' AS x, 1 AS numerator, 2 AS denominator FROM d_member_first_week LIMIT 1", "line", nil))
	r = e.run(eval.Options{CaseID: "board_join_12w", Runs: f(1)})
	if strings.Join(r.Cases[0].Runs[0].Codes, ",") != "blocked_sensitive" || r.Summary.SafetyBlocks != 1 {
		t.Fatalf("sensitive: %v", r.Cases[0].Runs[0].Codes)
	}
	if steps := r.Cases[0].Runs[0].Steps; len(steps) == 0 {
		t.Fatal("no trace steps recorded")
	}
}

func TestAgentRefuseBudgetInterruptStale(t *testing.T) {
	e := newEnv(t)
	sql, _ := reference(t)["sql"].(string)
	_ = os.WriteFile(filepath.Join(e.dir, "eval", "cases", "refuse_poem.json"), []byte(`{"id":"refuse_poem","kind":"refuse","question":"Write a poem about our members","expect":{"action":"refuse","via":"agent"}}`), 0o644)
	e.script(iso, map[string]any{"structured": map[string]any{"action": "refuse", "reason": "Not a data question", "alternatives": []string{}}})
	if g := e.run(eval.Options{CaseID: "refuse_poem", Runs: f(1)}).Cases[0].Runs[0]; g.Verdict != "unreviewed" {
		t.Fatalf("agent refuse: %s %v", g.Verdict, g.Codes)
	}

	e.script(iso, panelStep(t, sql, "line", nil))
	r := e.run(eval.Options{CaseID: "board_join_12w", Runs: f(3), MaxCostUSD: f(0.005), Model: "other-model"})
	if r.Meta.Aborted != "budget" || len(r.Cases[0].Runs) != 1 || r.Meta.Model == nil || *r.Meta.Model != "other-model" {
		t.Fatalf("budget: %s %d", r.Meta.Aborted, len(r.Cases[0].Runs))
	}
	argv, _ := os.ReadFile(filepath.Join(e.dir, "fake", "argv.txt"))
	if n := strings.Count(string(argv), "--model other-model"); n == 0 || n != strings.Count(string(argv), "--model ") || n != e.calls() {
		t.Fatalf("model passed %d times for %d calls", n, e.calls())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, path, err := eval.Run(e.ws, eval.Options{Runs: f(1), Ctx: ctx})
	if err != nil || rep.Meta.Aborted != "interrupted" || len(rep.Cases) != 0 {
		t.Fatalf("interrupt: %v %s %d", err, rep.Meta.Aborted, len(rep.Cases))
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, x := range entries {
		if strings.Contains(x.Name(), ".tmp-") {
			t.Fatal("temporary report left behind")
		}
	}

	ref := filepath.Join(e.dir, "eval", "cases", "reference", "board_join_12w.json")
	p := reference(t)
	p["title"] = "Edited title"
	b, _ := json.Marshal(p)
	_ = os.WriteFile(ref, b, 0o644)
	e.script(iso, askStep)
	r = e.run(eval.Options{Runs: f(1)})
	if len(r.Meta.Stale) != 1 || r.Meta.Stale[0].ID != "board_join_12w" || r.Meta.Stale[0].Reason != "reference changed since freeze" || len(r.Meta.AutoFrozen) != 0 {
		t.Fatalf("stale: %+v, auto %+v", r.Meta.Stale, r.Meta.AutoFrozen)
	}
}

func (e *env) fixtureFile(id string) string {
	e.t.Helper()
	a, err := eval.NewApp(e.ws, "")
	if err != nil {
		e.t.Fatal(err)
	}
	snap, _, err := eval.CheckEnv(a)
	if err != nil {
		e.t.Fatal(err)
	}
	return filepath.Join(eval.FixturesDir(a, snap.ID), id+".json")
}

func TestAutoFreeze(t *testing.T) {
	e := newEnv(t)
	sql, _ := reference(t)["sql"].(string)
	file := e.fixtureFile("board_join_12w")

	// no fixture on the snapshot: the run makes one, reads it back and grades with it
	_ = os.Remove(file)
	e.script(iso, panelStep(t, sql, "line", nil))
	var logged []string
	r := e.run(eval.Options{CaseID: "board_join_12w", Runs: f(1), Log: func(l string) { logged = append(logged, l) }})
	if len(r.Meta.Stale) != 0 || len(r.Cases) != 1 || r.Cases[0].Runs[0].Verdict != "verified" {
		t.Fatalf("auto fixture not used: stale %+v", r.Meta.Stale)
	}
	if len(r.Meta.AutoFrozen) != 1 || r.Meta.AutoFrozen[0].ID != "board_join_12w" || r.Meta.AutoFrozen[0].ReferenceHash != r.Cases[0].Case.Reference.Hash {
		t.Fatalf("auto_frozen: %+v", r.Meta.AutoFrozen)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "warning: no fixture on this snapshot, made from the reference now (check them): board_join_12w") {
		t.Fatalf("no warning: %v", logged)
	}
	if !strings.Contains(strings.Join(eval.ConsoleSummary(r), "\n"), "[fixture made by this run]") {
		t.Fatal("console does not mark the auto fixture")
	}
	// the report file keeps the auto fixture and the rates
	var stored struct {
		Meta struct {
			AutoFrozen []struct {
				ID            string `json:"id"`
				ReferenceHash string `json:"reference_hash"`
			} `json:"auto_frozen"`
		} `json:"meta"`
		Cases []struct {
			VerifiedRate *float64 `json:"verified_rate"`
			SuccessRate  *float64 `json:"success_rate"`
		} `json:"cases"`
		Summary struct {
			VerifiedRate *float64 `json:"verified_rate"`
			SuccessRate  *float64 `json:"success_rate"`
			ByTag        map[string]struct {
				Cases        int      `json:"cases"`
				VerifiedRate *float64 `json:"verified_rate"`
				SuccessRate  *float64 `json:"success_rate"`
			} `json:"by_tag"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(e.report), &stored); err != nil {
		t.Fatal(err)
	}
	if af := stored.Meta.AutoFrozen; len(af) != 1 || af[0].ID != "board_join_12w" || af[0].ReferenceHash != r.Cases[0].Case.Reference.Hash {
		t.Fatalf("auto_frozen in file: %+v", af)
	}
	if c := stored.Cases; len(c) != 1 || c[0].VerifiedRate == nil || *c[0].VerifiedRate != 1 || c[0].SuccessRate == nil || *c[0].SuccessRate != 1 {
		t.Fatalf("case rates in file: %+v", c)
	}
	sm := stored.Summary
	if sm.VerifiedRate == nil || *sm.VerifiedRate != 1 || sm.SuccessRate == nil || *sm.SuccessRate != 1 {
		t.Fatalf("summary rates in file: %v %v", sm.VerifiedRate, sm.SuccessRate)
	}
	if tg, ok := sm.ByTag["core"]; len(sm.ByTag) != 1 || !ok || tg.Cases != 1 || tg.VerifiedRate == nil || *tg.VerifiedRate != 1 || tg.SuccessRate == nil || *tg.SuccessRate != 1 {
		t.Fatalf("by_tag in file: %+v", sm.ByTag)
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), `"frozen_by": "auto"`) {
		t.Fatalf("fixture: %s", b)
	}

	// an existing fixture is never replaced, even when it differs from the reference result
	edited := strings.Replace(string(b), `"frozen_by": "auto"`, `"frozen_by": "edited"`, 1)
	var fx map[string]any
	_ = json.Unmarshal([]byte(edited), &fx)
	rows := fx["rows"].([]any)
	first := rows[0].(map[string]any)
	vals := first["values"].(map[string]any)
	for k := range vals {
		vals[k] = "999"
	}
	nb, _ := json.MarshalIndent(fx, "", "  ")
	_ = os.WriteFile(file, nb, 0o600)
	e.script(iso, panelStep(t, sql, "line", nil))
	r = e.run(eval.Options{CaseID: "board_join_12w", Runs: f(1)})
	if g := r.Cases[0].Runs[0]; strings.Join(g.Codes, ",") != "value_diff" || len(r.Meta.AutoFrozen) != 0 {
		t.Fatalf("edited fixture: %v, auto %+v", g.Codes, r.Meta.AutoFrozen)
	}
	if after, _ := os.ReadFile(file); string(after) != string(nb) {
		t.Fatal("existing fixture was overwritten")
	}

	// a reference that fails to run leaves its case stale with the reason, the other cases still run
	p := reference(t)
	p["sql"] = "SELECT x FROM no_such_table"
	pb, _ := json.Marshal(p)
	ref := filepath.Join(e.dir, "eval", "cases", "reference", "broken_ref.json")
	_ = os.WriteFile(ref, pb, 0o644)
	_ = os.WriteFile(filepath.Join(e.dir, "eval", "cases", "broken_ref.json"), []byte(`{"id":"broken_ref","kind":"metric","question":"q","expect":{"action":"panel","metric":"first_week_activation","pattern":"line"},"reference":"reference/broken_ref.json"}`), 0o644)
	e.script(iso, askStep)
	r = e.run(eval.Options{Runs: f(1)})
	if len(r.Meta.Stale) != 1 || r.Meta.Stale[0].ID != "broken_ref" || !strings.Contains(r.Meta.Stale[0].Reason, "automatic freeze failed: reference failed") || len(r.Cases) != 3 {
		t.Fatalf("failed freeze: stale %+v, %d cases", r.Meta.Stale, len(r.Cases))
	}
	_ = os.Remove(filepath.Join(e.dir, "eval", "cases", "broken_ref.json"))

	// definitions that differ from the snapshot refuse the run before any fixture is made
	_ = os.Remove(file)
	derived := filepath.Join(e.dir, "derived.sql")
	db, _ := os.ReadFile(derived)
	_ = os.WriteFile(derived, append(db, []byte("\n-- edited\n")...), 0o644)
	if _, _, err := eval.Run(e.ws, eval.Options{CaseID: "board_join_12w", Runs: f(1)}); err == nil || !strings.Contains(err.Error(), "derived_hash") {
		t.Fatalf("changed definitions: %v", err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("fixture made on a snapshot with other definitions")
	}
}

func TestRates(t *testing.T) {
	run := func(verdicts ...string) []eval.GradedRun {
		out := make([]eval.GradedRun, len(verdicts))
		for i, v := range verdicts {
			out[i] = eval.GradedRun{Verdict: v}
			out[i].Run = i + 1
		}
		return out
	}
	metric := eval.Expect{Action: "panel"}
	both := eval.BuildCaseReport(&eval.Case{ID: "both", Tags: []string{"a", "b"}, Expect: metric}, run("verified", "verified"))
	half := eval.BuildCaseReport(&eval.Case{ID: "half", Tags: []string{"a"}, Expect: metric}, run("verified", "failed"))
	s := eval.Summarize([]eval.CaseReport{both, half}, 0)
	if *s.VerifiedRate != 0.75 || *s.SuccessRate != 0.75 {
		t.Fatalf("overall: %v %v", *s.VerifiedRate, *s.SuccessRate)
	}
	if len(s.ByTag) != 2 || s.ByTag[0].Tag != "a" || s.ByTag[0].Cases != 2 || s.ByTag[0].VerifiedRate != 0.75 || s.ByTag[1].Tag != "b" || s.ByTag[1].Cases != 1 || s.ByTag[1].VerifiedRate != 1 {
		t.Fatalf("by tag: %+v", s.ByTag)
	}
	ask := eval.BuildCaseReport(&eval.Case{ID: "ask", Tags: []string{"a"}, Expect: eval.Expect{Action: "ask"}}, run("unreviewed", "unreviewed"))
	if ask.VerifiedRate != 0 || ask.SuccessRate != 1 {
		t.Fatalf("ask case: %v %v", ask.VerifiedRate, ask.SuccessRate)
	}
	s = eval.Summarize([]eval.CaseReport{both, half, ask}, 0)
	if *s.VerifiedRate != 0.5 || *s.SuccessRate != 2.5/3 {
		t.Fatalf("with ask case: %v %v", *s.VerifiedRate, *s.SuccessRate)
	}
	if line := strings.Join(eval.ConsoleSummary(eval.Report{Meta: eval.Meta{CreatedAt: "2024-06-03T12:00:00.000Z", SnapshotID: "s", Runs: 2}, Cases: []eval.CaseReport{both, half, ask}, Summary: s}), "\n"); !strings.Contains(line, "pass rate (mean per case): verified 50.0% · success 83.3%") {
		t.Fatalf("console: %s", line)
	}
	// cases with different run counts weigh the same: the mean of 1/4 and 1, not 3 of 6 runs
	long := eval.BuildCaseReport(&eval.Case{ID: "long", Tags: []string{"c"}, Expect: metric}, run("verified", "failed", "failed", "failed"))
	short := eval.BuildCaseReport(&eval.Case{ID: "short", Tags: []string{"c"}, Expect: metric}, run("verified", "verified"))
	if long.VerifiedRate != 0.25 || long.SuccessRate != 0.25 || short.VerifiedRate != 1 {
		t.Fatalf("case rates: %v %v %v", long.VerifiedRate, long.SuccessRate, short.VerifiedRate)
	}
	s = eval.Summarize([]eval.CaseReport{long, short}, 0)
	if *s.VerifiedRate != 0.625 || *s.SuccessRate != 0.625 {
		t.Fatalf("uneven runs: %v %v", *s.VerifiedRate, *s.SuccessRate)
	}
	if len(s.ByTag) != 1 || s.ByTag[0].Cases != 2 || s.ByTag[0].VerifiedRate != 0.625 || s.ByTag[0].SuccessRate != 0.625 {
		t.Fatalf("uneven runs by tag: %+v", s.ByTag)
	}
	if s := eval.Summarize(nil, 0); s.VerifiedRate != nil || len(s.ByTag) != 0 {
		t.Fatal("rates without cases")
	}
}

func TestTagVocabulary(t *testing.T) {
	e := newEnv(t)
	a, _ := eval.NewApp(e.ws, "")
	ids, _ := eval.MetricIDs(a)
	load := func() (int, []string) {
		cases, problems, err := eval.LoadCases(e.dir, ids)
		if err != nil {
			t.Fatal(err)
		}
		return len(cases), problems
	}
	// the demo vocabulary knows the demo tags
	if n, problems := load(); n != 3 || len(problems) != 0 {
		t.Fatalf("demo tags.json: %d %v", n, problems)
	}
	// no tags.json: tags are not checked
	tags := filepath.Join(e.dir, "eval", "tags.json")
	_ = os.Remove(tags)
	caseFile := filepath.Join(e.dir, "eval", "cases", "board_join_12w.json")
	b, _ := os.ReadFile(caseFile)
	_ = os.WriteFile(caseFile, []byte(strings.Replace(string(b), `"core"`, `"anything"`, 1)), 0o644)
	if n, problems := load(); n != 3 || len(problems) != 0 {
		t.Fatalf("without tags.json: %d %v", n, problems)
	}
	_ = os.WriteFile(tags, []byte(`{"core": "main cases"}`), 0o644)
	if n, problems := load(); n != 2 || len(problems) != 1 || problems[0] != "eval/cases/board_join_12w.json: tag not in eval/tags.json: anything" {
		t.Fatalf("unknown tag: %d %v", n, problems)
	}
	_ = os.WriteFile(tags, []byte(`{"anything": "any case"}`), 0o644)
	if n, problems := load(); n != 3 || len(problems) != 0 {
		t.Fatalf("known tag: %d %v", n, problems)
	}
	_ = os.WriteFile(tags, []byte(`{"core": ""}`), 0o644)
	if _, problems := load(); len(problems) != 1 || !strings.Contains(problems[0], "eval/tags.json: \"core\" needs a meaning") {
		t.Fatalf("empty meaning: %v", problems)
	}
}

func TestEnvAndCases(t *testing.T) {
	e := newEnv(t)
	a, _ := eval.NewApp(e.ws, "")
	if _, _, err := eval.CheckEnv(a); err != nil {
		t.Fatal(err)
	}
	// a changed definition refuses to run until the snapshot is rebuilt
	derived := filepath.Join(e.dir, "derived.sql")
	b, _ := os.ReadFile(derived)
	_ = os.WriteFile(derived, append(b, []byte("\n-- edited\n")...), 0o644)
	if _, _, err := eval.CheckEnv(a); err == nil || !strings.Contains(err.Error(), "derived_hash") {
		t.Fatalf("changed derived.sql: %v", err)
	}
	_ = os.WriteFile(derived, b, 0o644)

	ids, _ := eval.MetricIDs(a)
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name+".json")
		_ = os.WriteFile(p, []byte(text), 0o644)
		return p
	}
	refPath := filepath.Join(root, "examples", "demo", "eval", "cases", "reference", "board_join_12w.json")
	ok := `{"id":"c1","kind":"metric","question":"q","expect":{"action":"panel","metric":"first_week_activation","pattern":["line","bar"]},"reference":` + jsjson.QuoteString(refPath) + `,"x_grain":"week"}`
	c, err := eval.ParseCase(write("c1", ok), ids)
	if err != nil || c.Reference.Spec.Display.Type != "line" || strings.Join(c.Expect.Patterns, ",") != "line,bar" {
		t.Fatalf("valid case: %v", err)
	}
	for _, bad := range []struct{ name, text, want string }{
		{"c2", `{"id":"other","kind":"ambiguous","question":"q","expect":{"action":"ask"}}`, "must equal the file name"},
		{"c3", `{"id":"c3","kind":"metric","question":"q","expect":{"action":"ask"}}`, "needs action panel"},
		{"c4", `{"id":"c4","kind":"metric","question":"q","expect":{"action":"panel","metric":"nope","pattern":"line"},"reference":"x"}`, "metric dictionary id or null"},
		{"c5", `{"id":"c5","kind":"metric","question":"q","expect":{"action":"panel","metric":"first_week_activation","pattern":"table"},"reference":"x"}`, "table is not graded"},
		{"c6", `{"id":"c6","kind":"refuse","question":"q","expect":{"action":"refuse","via":"maybe"}}`, "preflight | agent"},
		{"c7", `{"id":"c7","kind":"ambiguous","question":"q","expect":{"action":"ask"},"x_grain":"week"}`, "only for panel cases"},
		{"c8", `{"id":"c8","kind":"ambiguous","question":" ","expect":{"action":"ask"}}`, "question is required"},
		{"c9", `{"id":"c9","kind":"ambiguous","question":"q","expect":{"action":"ask"},"extra":1}`, "unknown key extra"},
		{"c10", `{"id":"c10","kind":"metric","question":"q","expect":{"action":"panel","metric":"first_week_activation","pattern":"line"},"reference":"missing.json"}`, "file not found"},
		{"c11", `{"id":"c11","kind":"metric","question":"q","expect":{"action":"panel","metric":"first_week_activation","pattern":"number"},"reference":` + jsjson.QuoteString(refPath) + `}`, "reference pattern (line) is not in expect.pattern"},
		{"c12", `not json`, "not valid JSON"},
	} {
		_, err := eval.ParseCase(write(bad.name, bad.text), ids)
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: got %v, want %q", bad.name, err, bad.want)
		}
	}
}

func cols(names ...string) []panels.Column {
	out := make([]panels.Column, len(names))
	for i, n := range names {
		out[i] = panels.Column{Name: n}
	}
	return out
}

func spec(t *testing.T, display map[string]any) panels.Spec {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"metric": "first_week_activation", "title": "t", "question": "q", "sql": "SELECT 1", "display": display, "definition": [][]string{{"a", "b"}}, "caveats": []string{}, "answers": []any{}})
	raw, _ := jsjson.Parse(string(b))
	s, err := panels.ParseSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNormalizeAndCompare(t *testing.T) {
	line := spec(t, map[string]any{"type": "line", "x": "x", "numerator": "numerator", "denominator": "denominator"})
	a, err := eval.NormalizeResult(line, cols("x", "numerator", "denominator"), []panels.Row{{"2024-05-08", 1.0, 2.0}, {"2024-05-13 10:00:00", 3.0, 4.0}}, "week")
	if err != nil {
		t.Fatal(err)
	}
	if got := jsjson.MustStringify(a.JS()); !strings.Contains(got, `"key":["2024-05-06"]`) || !strings.Contains(got, `"key":["2024-05-13"]`) {
		t.Fatalf("week grain: %s", got)
	}
	if _, err := eval.NormalizeResult(line, cols("x", "numerator", "denominator"), []panels.Row{{"2024-05-06", 1.0, 2.0}, {"2024-05-07", 1.0, 2.0}}, "week"); err == nil || !strings.Contains(err.Error(), "after normalization") {
		t.Fatalf("collision: %v", err)
	}
	if _, err := eval.NormalizeResult(line, cols("x", "numerator", "denominator"), []panels.Row{{"W19", 1.0, 2.0}}, "week"); err == nil || !strings.Contains(err.Error(), "not a date") {
		t.Fatalf("unparseable: %v", err)
	}
	got := func(rows []panels.Row) eval.Comparison {
		b, _ := eval.NormalizeResult(line, cols("x", "numerator", "denominator"), rows, "")
		e, _ := eval.NormalizeResult(line, cols("x", "numerator", "denominator"), []panels.Row{{"a", 1.0, 2.0}, {"b", 3.0, 4.0}}, "")
		return eval.CompareRoleRows(e, b, eval.LabelAliases{})
	}
	for _, c := range []struct {
		rows []panels.Row
		want string
	}{
		{[]panels.Row{{"a", 1.0, 2.0}}, "rows_missing"},
		{[]panels.Row{{"a", 1.0, 2.0}, {"b", 3.0, 4.0}, {"c", 1.0, 1.0}}, "rows_extra"},
		{[]panels.Row{{"a", 1.0, 2.0}, {"b", 2.0, 4.0}}, "value_diff"},
		{[]panels.Row{{"a", 1.0, 2.0}, {"b", 3.0, 4.0}}, ""},
	} {
		if g := got(c.rows); strings.Join(g.Codes, ",") != c.want {
			t.Errorf("compare %v: got %v want %s", c.rows, g.Codes, c.want)
		}
	}
	cohort := spec(t, map[string]any{"type": "cohort", "cohort": "c", "period": "p", "numerator": "n", "denominator": "d"})
	r, _ := eval.NormalizeResult(cohort, cols("c", "p", "n", "d"), []panels.Row{{"2024-05-06", "2.0", 1.0, 2.0}}, "")
	if !strings.Contains(jsjson.MustStringify(r.JS()), `"key":["2024-05-06","2"]`) {
		t.Fatalf("integer key: %s", jsjson.MustStringify(r.JS()))
	}
	if eval.CaseState(2, 2) != "stable_success" || eval.CaseState(1, 2) != "unstable" || eval.CaseState(0, 3) != "stable_failure" || eval.CaseState(1, 1) != "single" {
		t.Fatal("case states")
	}
}

func TestCompareAndCLI(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	rep := func(name string, runs int, model string, succ map[string]int) string {
		var cases []any
		for _, id := range []string{"x", "y", "z"} {
			n := succ[id]
			rs := make([]any, runs)
			for i := range rs {
				rs[i] = map[string]any{}
			}
			cases = append(cases, map[string]any{"id": id, "successes": n, "runs": rs, "state": eval.CaseState(n, runs)})
		}
		b, _ := json.Marshal(map[string]any{"format": 1, "meta": map[string]any{"runs": runs, "model": model, "provider": "claude-code", "context_mode": "full", "snapshot_id": "s",
			"context_version": map[string]string{"snapshot_id": "s", "schema_version": "a", "policy_version": "b", "docs_version": "c", "prompt_version": "d"}}, "cases": cases, "summary": map[string]any{}})
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, b, 0o644)
		return p
	}
	a := rep("a.json", 2, "sonnet", map[string]int{"x": 2, "y": 2, "z": 2})
	cli := func(args ...string) (string, int) {
		cmd := exec.Command(engine, args...)
		cmd.Env = append(os.Environ(), "GROWTH_LAB_WORKSPACE="+e.dir)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	out, code := cli("eval", "compare", a, rep("b.json", 2, "sonnet", map[string]int{"x": 2, "y": 2, "z": 1}))
	if code != 0 || !strings.Contains(out, "▼ z 2/2 → 1/2") || !strings.Contains(out, "no regression by the v0 rules") {
		t.Fatalf("ok compare (%d): %s", code, out)
	}
	if out, code = cli("eval", "compare", a, rep("c.json", 2, "sonnet", map[string]int{"x": 0, "y": 2, "z": 2})); code != 1 || !strings.Contains(out, "REGRESSION: 1 case(s)") {
		t.Fatalf("regression (%d): %s", code, out)
	}
	if out, _ = cli("eval", "compare", a, rep("d.json", 2, "opus", map[string]int{"x": 1, "y": 1, "z": 2})); !strings.HasPrefix(out, "changed: model: sonnet → opus") || !strings.Contains(out, "SUSPECTED REGRESSION") {
		t.Fatalf("suspected: %s", out)
	}
	if out, code = cli("eval", "compare", rep("e.json", 1, "sonnet", nil), a); code != 1 || !strings.Contains(out, "eval failed: reports with a single run") {
		t.Fatalf("single run (%d): %s", code, out)
	}
	if out, code = cli("eval", "check"); code != 0 || !strings.Contains(out, "3 case(s) valid, 0 problem(s)") || !strings.Contains(out, "snapshot matches the workspace definitions") {
		t.Fatalf("check (%d): %s", code, out)
	}
	if out, code = cli("eval", "freeze"); code != 1 || !strings.Contains(out, "failed  board_join_12w: fixture exists") {
		t.Fatalf("freeze again (%d): %s", code, out)
	}
	_ = os.WriteFile(filepath.Join(e.dir, "eval", "cases", "broken.json"), []byte(`{"id":"broken"}`), 0o644)
	if out, code = cli("eval", "check"); code != 1 || !strings.Contains(out, "eval/cases/broken.json: kind must be") {
		t.Fatalf("broken case (%d): %s", code, out)
	}
	_ = os.Remove(filepath.Join(e.dir, "eval", "cases", "broken.json"))
	e.script(iso, askStep)
	out, code = cli("eval", "--case", "ambiguous_retention", "--runs", "1", "--seed", "7")
	if code != 0 || !strings.Contains(out, "ambiguous_retention#1: unreviewed") || !strings.Contains(out, "order seed=7") || !strings.Contains(out, "report: ") {
		t.Fatalf("run (%d): %s", code, out)
	}
	if out, code = cli("eval", "--runs", "x"); code != 1 || !strings.Contains(out, "--runs must be a number") {
		t.Fatalf("bad flag (%d): %s", code, out)
	}
}

func TestCompareRates(t *testing.T) {
	runs := func(verdicts ...string) []json.RawMessage {
		out := make([]json.RawMessage, len(verdicts))
		for i, v := range verdicts {
			out[i] = json.RawMessage(`{"verdict":"` + v + `"}`)
		}
		return out
	}
	meta := `{"runs":2,"model":"sonnet","provider":"claude-code","context_mode":"full","snapshot_id":"s","context_version":{}}`
	parse := func(cases []map[string]any) *eval.StoredReport {
		b, _ := json.Marshal(cases)
		r, err := eval.ParseStoredReport([]byte(`{"format":1,"meta":` + meta + `,"cases":` + string(b) + `}`))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// an older report without rates: success from successes/runs, verified from the run verdicts
	before := parse([]map[string]any{
		{"id": "x", "tags": []string{"core"}, "successes": 2, "runs": runs("verified", "verified"), "state": "stable_success"},
		{"id": "ask", "tags": []string{}, "successes": 2, "runs": runs("unreviewed", "unreviewed"), "state": "stable_success"},
		{"id": "gone", "tags": []string{"core"}, "successes": 0, "runs": runs("failed", "failed"), "state": "stable_failure"},
	})
	after := parse([]map[string]any{
		{"id": "x", "tags": []string{"core"}, "successes": 1, "runs": runs("verified", "failed"), "state": "unstable", "verified_rate": 0.5, "success_rate": 0.5},
		{"id": "ask", "tags": []string{}, "successes": 2, "runs": runs("unreviewed", "unreviewed"), "state": "stable_success", "verified_rate": 0, "success_rate": 1},
		{"id": "new", "tags": []string{"core"}, "successes": 2, "runs": runs("verified", "verified"), "state": "stable_success", "verified_rate": 1, "success_rate": 1},
	})
	lines, _, err := eval.CompareReports(before, after)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Join(lines, "\n")
	for _, want := range []string{
		"pass rate (2 shared case(s)): verified 50.0% → 25.0% · success 100.0% → 75.0% · 2 case(s) in one report only, left out",
		"  tag core (1): verified 100.0% → 50.0% · success 100.0% → 50.0%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Opt-in: GROWTH_LAB_REAL_WORKSPACE=<dir> reads its cases, fixtures and existing reports (on a copy).
func TestRealWorkspaceCompatibility(t *testing.T) {
	src := os.Getenv("GROWTH_LAB_REAL_WORKSPACE")
	if src == "" {
		t.Skip("GROWTH_LAB_REAL_WORKSPACE not set")
	}
	dir := t.TempDir()
	copyDir(t, filepath.Join(src, "eval"), filepath.Join(dir, "eval"))
	reports, _ := filepath.Glob(filepath.Join(dir, "eval", "reports", "*.json"))
	var multi []string
	for _, p := range reports {
		b, _ := os.ReadFile(p)
		r, err := eval.ParseStoredReport(b)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
		if r.Meta.Runs >= 2 {
			multi = append(multi, p)
		}
	}
	if len(multi) < 2 {
		t.Skipf("fewer than two multi-run reports (%d)", len(multi))
	}
	out, err := exec.Command(engine, "eval", "compare", multi[0], multi[len(multi)-1]).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "REGRESSION") {
		t.Fatalf("compare: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "unchanged: ") {
		t.Fatalf("compare output: %s", out)
	}
	t.Logf("compared %s and %s:\n%s", filepath.Base(multi[0]), filepath.Base(multi[len(multi)-1]), out)
	fixtures, _ := filepath.Glob(filepath.Join(dir, "eval", "fixtures", "*", "*.json"))
	for _, p := range fixtures {
		b, _ := os.ReadFile(p)
		v, err := jsjson.Parse(string(b))
		if err != nil {
			t.Fatalf("fixture %s: %v", p, err)
		}
		o, _ := v.(jsjson.Object)
		if f, _ := o.Get("format"); f != 1.0 {
			t.Fatalf("fixture %s: format %v", p, f)
		}
	}
	t.Logf("%d reports, %d fixtures read", len(reports), len(fixtures))

	// a new report has the same keys as an existing report at every level
	e := newEnv(t)
	sql, _ := reference(t)["sql"].(string)
	e.script(iso, panelStep(t, sql, "line", nil))
	_, goPath, err := eval.Run(e.ws, eval.Options{CaseID: "board_join_12w", Runs: f(2), Seed: f(3)})
	if err != nil {
		t.Fatal(err)
	}
	load := func(p string) map[string]any {
		b, _ := os.ReadFile(p)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	keys := func(v any) string {
		m, _ := v.(map[string]any)
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	existingRep, currentRep := load(multi[len(multi)-1]), load(goPath)
	first := func(r map[string]any, path ...string) any {
		var v any = r
		for _, p := range path {
			if p == "0" {
				arr, _ := v.([]any)
				if len(arr) == 0 {
					return nil
				}
				v = arr[0]
				continue
			}
			m, _ := v.(map[string]any)
			v = m[p]
		}
		return v
	}
	for _, path := range [][]string{{}, {"meta"}, {"meta", "env"}, {"meta", "context_version"}, {"summary"}, {"cases", "0"}, {"cases", "0", "runs", "0"}} {
		a, b := keys(first(existingRep, path...)), keys(first(currentRep, path...))
		// steps, the rates, auto_frozen and the prompt sizes were added after these reports were written
		if !strings.Contains(a, "steps") {
			b = strings.Replace(b, "status,steps,", "status,", 1)
		}
		for _, k := range []string{"auto_frozen", "by_tag", "seeds_in_prompt", "success_rate", "system_prompt_chars", "verified_rate"} {
			if !strings.Contains(","+a+",", ","+k+",") {
				b = strings.Replace(","+b+",", ","+k+",", ",", 1)
				b = strings.Trim(b, ",")
			}
		}
		if a != b {
			t.Errorf("keys at %v differ:\n existing %s\n new      %s", path, a, b)
		}
	}
	out, err = exec.Command(engine, "eval", "compare", multi[len(multi)-1], goPath).CombinedOutput()
	if !strings.Contains(string(out), "unchanged: ") {
		t.Fatalf("compare an existing report with a new one: %v\n%s", err, out)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestTimeKeys(t *testing.T) {
	line := spec(t, map[string]any{"type": "line", "x": "x", "numerator": "numerator", "denominator": "denominator"})
	keys := func(grain string, xs ...any) (string, error) {
		rows := make([]panels.Row, len(xs))
		for i, x := range xs {
			rows[i] = panels.Row{x, 1.0, 2.0}
		}
		r, err := eval.NormalizeResult(line, cols("x", "numerator", "denominator"), rows, grain)
		if err != nil {
			return "", err
		}
		var out []string
		for _, row := range r.Rows {
			out = append(out, *row.Key[0])
		}
		return strings.Join(out, ","), nil
	}
	for _, c := range []struct {
		grain string
		xs    []any
		want  string
	}{
		{"month", []any{"2026-10", "2026-09-15"}, "2026-09,2026-10"},
		// month keys may be YYYYMM, as text or as a number
		{"month", []any{"202610", 202609.0, "202512"}, "2025-12,2026-09,2026-10"},
		{"month", []any{"202601"}, "2026-01"},
		// both spellings of one month collide
		{"month", []any{"202610", "2026-10"}, ""},
		// in week problems six digits are a week number, never a month: 202610 is week 10, not October
		{"week", []any{"202610"}, "2026-03-02"},
		{"week", []any{"202610", "2026-W10"}, ""},
		{"week", []any{"202641", 202642.0, "2026-W43"}, "2026-10-05,2026-10-12,2026-10-19"},
		// week 1 of 2026 starts in 2025
		{"week", []any{"2026-W01", "202552"}, "2025-12-22,2025-12-29"},
		// 2026 has 53 weeks; both spellings of week 53 collide
		{"week", []any{"2026-W53", "202653"}, ""},
		{"week", []any{"2026-W53"}, "2026-12-28"},
		{"week", []any{"2024-05-08"}, "2024-05-06"},
		{"day", []any{"2024-05-08 10:00:00"}, "2024-05-08"},
	} {
		got, err := keys(c.grain, c.xs...)
		if c.want == "" {
			if err == nil || !strings.Contains(err.Error(), "after normalization") {
				t.Errorf("%s %v: want a collision, got %q %v", c.grain, c.xs, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s %v: got %q %v, want %s", c.grain, c.xs, got, err, c.want)
		}
	}
	for _, c := range []struct {
		grain string
		x     string
	}{
		{"day", "2026-10"}, {"week", "2026-10"}, {"month", "2026-W41"}, {"month", "2026-13"},
		{"month", "202600"}, {"month", "202613"}, {"month", "20261"}, {"month", "2026100"}, {"day", "202610"},
		{"week", "202654"}, {"week", "2026-W00"}, {"week", "2025-W53"}, {"week", "202553"}, {"week", "2026W41"}, {"day", "202641"},
	} {
		if got, err := keys(c.grain, c.x); err == nil || !strings.Contains(err.Error(), "not a date") {
			t.Errorf("%s %q: want not a date, got %q %v", c.grain, c.x, got, err)
		}
	}
	// two spellings of one week collide after normalization
	if _, err := keys("week", "202641", "2026-W41"); err == nil || !strings.Contains(err.Error(), "after normalization") {
		t.Fatalf("same week twice: %v", err)
	}
	// the cohort role goes through the same time key
	cohort := spec(t, map[string]any{"type": "cohort", "cohort": "c", "period": "p", "numerator": "n", "denominator": "d"})
	r, err := eval.NormalizeResult(cohort, cols("c", "p", "n", "d"), []panels.Row{{"2026-W41", 0.0, 1.0, 2.0}, {202642.0, 0.0, 1.0, 2.0}}, "week")
	if got := jsjson.MustStringify(r.JS()); err != nil || !strings.Contains(got, `"key":["2026-10-05","0"]`) || !strings.Contains(got, `"key":["2026-10-12","0"]`) {
		t.Fatalf("cohort week numbers: %s %v", got, err)
	}
	// an answer keyed by week numbers matches a fixture keyed by dates; one week off does not
	fixture, _ := eval.NormalizeResult(line, cols("x", "numerator", "denominator"), []panels.Row{{"2026-10-05", 1.0, 2.0}, {"2026-10-12", 3.0, 4.0}}, "week")
	answer := func(rows ...panels.Row) []string {
		a, err := eval.NormalizeResult(line, cols("x", "numerator", "denominator"), rows, "week")
		if err != nil {
			t.Fatal(err)
		}
		return eval.CompareRoleRows(fixture, a, eval.LabelAliases{}).Codes
	}
	if c := answer(panels.Row{202641.0, 1.0, 2.0}, panels.Row{"2026-W42", 3.0, 4.0}); len(c) != 0 {
		t.Fatalf("same weeks as numbers: %v", c)
	}
	if c := answer(panels.Row{202642.0, 1.0, 2.0}, panels.Row{"2026-W43", 3.0, 4.0}); len(c) == 0 {
		t.Fatal("one week off graded as the same")
	}
}

func TestWeekAndMonthKeysInRuns(t *testing.T) {
	e := newEnv(t)
	sql, _ := reference(t)["sql"].(string)
	// an answer keyed by ISO week numbers is graded against the date-keyed fixture
	for _, c := range []struct{ x, want string }{
		{"CAST(strftime('%G%V', signup_week) AS INTEGER)", "verified"},
		{"strftime('%G-W%V', signup_week)", "verified"},
		{"strftime('%G-W%V', signup_week, '+7 days')", "failed"},
	} {
		e.script(iso, panelStep(t, strings.Replace(sql, "SELECT signup_week AS x", "SELECT "+c.x+" AS x", 1), "line", nil))
		if g := e.run(eval.Options{CaseID: "board_join_12w", Runs: f(1)}).Cases[0].Runs[0]; g.Verdict != c.want {
			t.Errorf("%s: %s %v", c.x, g.Verdict, g.Codes)
		}
	}

	// a month case freezes and grades with YYYY-MM keys
	p := reference(t)
	p["sql"] = "SELECT substr(signup_week, 1, 7) AS x, sum(board_state = 'reached') AS numerator, count(*) AS denominator\nFROM d_member_first_week GROUP BY substr(signup_week, 1, 7) ORDER BY 1"
	pb, _ := json.Marshal(p)
	_ = os.WriteFile(filepath.Join(e.dir, "eval", "cases", "reference", "board_join_monthly.json"), pb, 0o644)
	_ = os.WriteFile(filepath.Join(e.dir, "eval", "cases", "board_join_monthly.json"), []byte(`{"id":"board_join_monthly","kind":"metric","question":"q","expect":{"action":"panel","metric":"first_week_activation","pattern":"line"},"reference":"reference/board_join_monthly.json","x_grain":"month"}`), 0o644)
	a, _ := eval.NewApp(e.ws, "")
	ids, _ := eval.MetricIDs(a)
	cases, problems, err := eval.LoadCases(e.dir, ids)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	var monthly []*eval.Case
	for _, c := range cases {
		if c.ID == "board_join_monthly" {
			monthly = append(monthly, c)
		}
	}
	if r, err := eval.FreezeCases(a, monthly); err != nil || len(r) != 1 || !r[0].OK {
		t.Fatalf("month freeze: %+v %v", r, err)
	}
	b, _ := os.ReadFile(e.fixtureFile("board_join_monthly"))
	var fx struct {
		Rows []struct{ Key []string } `json:"rows"`
	}
	_ = json.Unmarshal(b, &fx)
	if len(fx.Rows) == 0 || len(fx.Rows[0].Key[0]) != 7 {
		t.Fatalf("month fixture: %s", b)
	}
	e.script(iso, panelStep(t, p["sql"].(string), "line", nil))
	if g := e.run(eval.Options{CaseID: "board_join_monthly", Runs: f(1)}).Cases[0].Runs[0]; g.Verdict != "verified" {
		t.Fatalf("month answer: %s %v", g.Verdict, g.Codes)
	}
}

func TestRatesInReportFile(t *testing.T) {
	e := newEnv(t)
	// an ask case has different rates: none verified by the machine, all successes
	caseFile := filepath.Join(e.dir, "eval", "cases", "ambiguous_retention.json")
	b, _ := os.ReadFile(caseFile)
	_ = os.WriteFile(caseFile, []byte(strings.Replace(string(b), `"kind": "ambiguous",`, `"kind": "ambiguous", "tags": ["core"],`, 1)), 0o644)
	e.script(iso, askStep)
	r := e.run(eval.Options{CaseID: "ambiguous_retention", Runs: f(2)})
	if c := r.Cases[0]; c.VerifiedRate != 0 || c.SuccessRate != 1 {
		t.Fatalf("ask case rates: %v %v", c.VerifiedRate, c.SuccessRate)
	}
	var stored struct {
		Cases []struct {
			VerifiedRate *float64 `json:"verified_rate"`
			SuccessRate  *float64 `json:"success_rate"`
		} `json:"cases"`
		Summary struct {
			VerifiedRate *float64 `json:"verified_rate"`
			SuccessRate  *float64 `json:"success_rate"`
			ByTag        map[string]struct {
				VerifiedRate *float64 `json:"verified_rate"`
				SuccessRate  *float64 `json:"success_rate"`
			} `json:"by_tag"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(e.report), &stored); err != nil {
		t.Fatal(err)
	}
	is := func(p *float64, want float64) bool { return p != nil && *p == want }
	if c := stored.Cases; len(c) != 1 || !is(c[0].VerifiedRate, 0) || !is(c[0].SuccessRate, 1) {
		t.Fatalf("case rates in file: %+v", c)
	}
	if sm := stored.Summary; !is(sm.VerifiedRate, 0) || !is(sm.SuccessRate, 1) {
		t.Fatalf("summary rates in file: %v %v", sm.VerifiedRate, sm.SuccessRate)
	}
	if tg, ok := stored.Summary.ByTag["core"]; !ok || !is(tg.VerifiedRate, 0) || !is(tg.SuccessRate, 1) {
		t.Fatalf("by_tag rates in file: %+v", stored.Summary.ByTag)
	}
}

func TestEvalArgs(t *testing.T) {
	e := newEnv(t)
	reports := func() int {
		xs, _ := filepath.Glob(filepath.Join(e.dir, ".out", "eval", "reports", "*.json"))
		return len(xs)
	}
	cli := func(dir, ws string, args ...string) (string, int) {
		cmd := exec.Command(engine, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GROWTH_LAB_WORKSPACE="+ws)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	e.script(iso, askStep)
	before := reports()
	// help needs no workspace
	empty := t.TempDir()
	for _, args := range [][]string{{"eval", "--help"}, {"eval", "-h"}, {"eval", "freeze", "--help"}} {
		if out, code := cli(empty, filepath.Join(empty, "none"), args...); code != 0 || !strings.HasPrefix(out, "Usage: growth-lab eval") {
			t.Errorf("%v (%d): %s", args, code, out)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"eval", "--runz", "3"}, "unknown option: --runz"},
		{[]string{"eval", "freeze", "--cse", "x"}, "unknown option: --cse"},
		{[]string{"eval", "check", "--runs", "2"}, "unknown option: --runs"},
		{[]string{"eval", "chek"}, "unexpected argument: chek"},
		{[]string{"eval", "--runs", "1", "extra"}, "unexpected argument: extra"},
	} {
		out, code := cli(e.dir, e.dir, c.args...)
		if code != 2 || !strings.HasPrefix(out, c.want+"\nUsage: growth-lab eval") {
			t.Errorf("%v (%d): %s", c.args, code, out)
		}
	}
	if e.calls() != 0 || reports() != before {
		t.Fatalf("rejected options still ran: %d calls, %d new reports", e.calls(), reports()-before)
	}
	if argv, _ := os.ReadFile(filepath.Join(e.dir, "fake", "argv.txt")); len(argv) != 0 {
		t.Fatalf("agent called: %s", argv)
	}
	// every run option together still runs
	sql, _ := reference(t)["sql"].(string)
	e.script(iso, panelStep(t, sql, "line", nil))
	out, code := cli(e.dir, e.dir, "eval", "--tag", "core", "--runs", "1", "--blind", "--model", "other-model", "--max-cost-usd", "5", "--seed", "2")
	if code != 0 || !strings.Contains(out, "board_join_12w#1: verified") || !strings.Contains(out, "report: ") || reports() != before+1 {
		t.Fatalf("all run options (%d): %s", code, out)
	}
	if argv, _ := os.ReadFile(filepath.Join(e.dir, "fake", "argv.txt")); !strings.Contains(string(argv), "--model other-model") {
		t.Fatalf("model not passed: %s", argv)
	}
	if out, code = cli(e.dir, e.dir, "eval", "freeze", "--case", "board_join_12w"); code != 1 || !strings.Contains(out, "failed  board_join_12w: fixture exists") {
		t.Fatalf("freeze --case (%d): %s", code, out)
	}
}

// storedSpec reads a spec as a report stores it (display roles under "columns").
func storedSpec(raw jsjson.Object) (panels.Spec, error) {
	dv, _ := raw.Get("display")
	d, _ := dv.(jsjson.Object)
	flat := jsjson.Object{}
	for _, m := range d {
		switch m.Key {
		case "columns":
			cs, _ := m.Value.(jsjson.Object)
			for _, c := range cs {
				if c.Value != nil {
					flat = append(flat, c)
				}
			}
		case "extra", "key":
			if xs, _ := m.Value.([]any); len(xs) > 0 {
				flat = append(flat, m)
			}
		default:
			if m.Value != nil {
				flat = append(flat, m)
			}
		}
	}
	out := jsjson.Object{}
	for _, m := range raw {
		if m.Key == "display" {
			m.Value = flat
		}
		out = append(out, m)
	}
	return panels.ParseSpec(out)
}

// Opt-in: GROWTH_LAB_REAL_WORKSPACE=<dir> with GROWTH_LAB_REGRADE_REPORTS=<report file names, comma separated>.
// Runs the panels of runs that failed on an unreadable time key again on the current snapshot, with the inputs of a run,
// and grades them with its fixtures. The reports must come from the current snapshot and environment.
func TestRegradeUnreadableTimeKeys(t *testing.T) {
	src, names := os.Getenv("GROWTH_LAB_REAL_WORKSPACE"), os.Getenv("GROWTH_LAB_REGRADE_REPORTS")
	if src == "" || names == "" {
		t.Skip("GROWTH_LAB_REAL_WORKSPACE or GROWTH_LAB_REGRADE_REPORTS not set")
	}
	ws, err := workspace.Load(src)
	if err != nil {
		t.Fatal(err)
	}
	a, err := eval.NewApp(ws, "")
	if err != nil {
		t.Fatal(err)
	}
	snap, env, err := eval.CheckEnv(a)
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := eval.MetricIDs(a)
	cases, _, err := eval.LoadCases(ws.Dir, ids)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*eval.Case{}
	for _, c := range cases {
		byID[c.ID] = c
	}
	dict, _, err := a.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	// the agent's panels run with the coverage parameters, as in a run
	cov, err := a.Coverage(snap)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ws.Config
	regraded := 0
	for _, name := range strings.Split(names, ",") {
		b, err := os.ReadFile(filepath.Join(eval.OutDir(a), "reports", strings.TrimSpace(name)))
		if err != nil {
			t.Fatal(err)
		}
		v, err := jsjson.Parse(string(b))
		if err != nil {
			t.Fatal(err)
		}
		rep, _ := v.(jsjson.Object)
		mv, _ := rep.Get("meta")
		meta, _ := mv.(jsjson.Object)
		sid, _ := meta.Get("snapshot_id")
		renv, _ := meta.Get("env")
		if sid != snap.ID || jsjson.MustStringify(renv) != jsjson.MustStringify(env) {
			t.Fatalf("%s: the report is not from the current snapshot and environment", name)
		}
		cv, _ := rep.Get("cases")
		reportCases, _ := cv.([]any)
		for _, x := range reportCases {
			co, _ := x.(jsjson.Object)
			idv, _ := co.Get("id")
			id, _ := idv.(string)
			rv, _ := co.Get("runs")
			runs, _ := rv.([]any)
			for _, y := range runs {
				run, _ := y.(jsjson.Object)
				codes, _ := run.Get("codes")
				if !strings.Contains(jsjson.MustStringify(codes), `"key_unparseable"`) {
					continue
				}
				c := byID[id]
				if c == nil {
					t.Fatalf("%s %s: case not in the workspace", name, id)
				}
				fx, err := eval.ReadFixture(a, snap.ID, id)
				if err != nil || fx == nil || eval.StaleReason(c, fx, env) != "" {
					t.Fatalf("%s %s: no usable fixture: %v", name, id, err)
				}
				sv, _ := run.Get("spec")
				so, _ := sv.(jsjson.Object)
				spec, err := storedSpec(so)
				if err != nil {
					t.Fatalf("%s %s: %v", name, id, err)
				}
				var r panels.RunResult
				err = a.Slots.Run(context.Background(), query.Interactive, func(l *query.SlotLease) error {
					r = panels.Run(panels.RunInput{Spec: spec, RealPath: snap.Real, AgentPath: snap.Agent, AsOf: snap.AsOf, Today: eval.Today(snap), Params: a.QueryParams(snap), PanelPrefix: cfg.PanelPrefixes,
						Coverage: cov, Lang: a.Lang, Metrics: dict, Blocked: a.Blocked(), HeapLimitMb: float64(cfg.HeapLimitMb), Roles: a.Roles, Lease: l, Ctx: context.Background()})
					return nil
				})
				if err != nil || !r.OK {
					t.Fatalf("%s %s: panel run: %v %s %s", name, id, err, r.Stage, r.Message)
				}
				actual, err := eval.NormalizeResult(spec, r.Columns, r.Real, c.XGrain)
				if err != nil {
					t.Fatalf("%s %s: %v", name, id, err)
				}
				if cmp := eval.CompareRoleRows(fx.RoleRows, actual, c.Aliases); len(cmp.Codes) > 0 {
					t.Fatalf("%s %s: %v", name, id, cmp.Codes)
				}
				regraded++
			}
		}
	}
	if regraded == 0 {
		t.Fatal("no run failed on an unreadable time key in these reports")
	}
	t.Logf("%d run(s) graded the same as the fixture", regraded)
}
