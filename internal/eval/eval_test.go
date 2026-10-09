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
	e := &env{t, dir, ws}
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
	if b, err := os.ReadFile(path); err != nil || !strings.HasPrefix(string(b), "{\n  \"format\": 1,") {
		e.t.Fatalf("report file: %v", err)
	}
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
	if len(r.Meta.Stale) != 1 || r.Meta.Stale[0].ID != "board_join_12w" || r.Meta.Stale[0].Reason != "reference changed since freeze" {
		t.Fatalf("stale: %+v", r.Meta.Stale)
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
		// steps was added after these reports were written
		if !strings.Contains(a, "steps") {
			b = strings.Replace(b, "status,steps,", "status,", 1)
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
