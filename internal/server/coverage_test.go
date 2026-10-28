package server

import (
	"os"
	"path/filepath"
	"testing"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/sqlitec"
	"growth-lab/internal/workspace"
)

func TestWithParamsReplacesSameName(t *testing.T) {
	got := withParams(jsjson.Object{{Key: "data_end", Value: "2001-01-01"}, {Key: "reopen_date", Value: "x"}}, jsjson.Object{{Key: "data_end", Value: "2018-09-21"}})
	if v, _ := got.Get("data_end"); v != "2018-09-21" || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func coverageApp(t *testing.T, on bool) (*App, *Snapshot) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.sqlite")
	db, err := sqlitec.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RunOnce("CREATE TABLE e_events (kind TEXT, at TEXT)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.WriteFile(filepath.Join(dir, "coverage.json"), []byte(`{"events": [{"table": "e_events", "time": "at"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := workspace.Config{Params: jsjson.Object{{Key: "quality_gap_ranges", Value: []any{"2014-03-01 00:00:00.000000~2016-06-01 00:00:00.000000"}}}}
	cfg.Agent.CoverageCheck = on
	return &App{WS: &workspace.Workspace{Dir: dir, Config: cfg}}, &Snapshot{ID: "s1", Real: path, AsOf: "2018-09-22 10:00:00.000000"}
}

func TestCoverageOffLeavesContextAlone(t *testing.T) {
	a, snap := coverageApp(t, false)
	if r, err := a.Coverage(snap); r != nil || err != nil || a.CoverageKey() != "" {
		t.Fatalf("%v %v %q", r, err, a.CoverageKey())
	}
	if _, ok := a.QueryParams(snap).Get("data_end"); ok {
		t.Fatal("data_end added while off")
	}
	b, _ := coverageApp(t, true)
	if b.CoverageKey() == "" {
		t.Fatal("no key while on")
	}
}

func TestCoverageRetriesAfterGapCopyFailure(t *testing.T) {
	a, snap := coverageApp(t, true)
	dir := filepath.Dir(snap.Real)
	blocker := filepath.Join(dir, "snap.gap1.sqlite")
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := a.Coverage(snap)
	if err != nil || r.GapError == "" || len(r.GapPaths) != 0 {
		t.Fatalf("failure not shown: %v %+v", err, r)
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	r, err = a.Coverage(snap)
	if err != nil || r.GapError != "" || len(r.GapPaths) != 1 {
		t.Fatalf("no retry after recovery: %v %+v", err, r)
	}
	if v, _ := a.QueryParams(snap).Get("data_end"); v != "2018-09-21" {
		t.Fatalf("data_end %v", v)
	}
}

func TestCoverageSwitchChangesContextVersion(t *testing.T) {
	off, snap := coverageApp(t, false)
	on := &App{WS: &workspace.Workspace{Dir: off.WS.Dir, Config: off.WS.Config}}
	on.WS.Config.Agent.CoverageCheck = true
	if off.ContextVersion(snap).Key() == on.ContextVersion(snap).Key() {
		t.Fatal("turning the coverage check on keeps the context version, so old sessions and kept previews would be reused")
	}
	again := &App{WS: &workspace.Workspace{Dir: off.WS.Dir, Config: off.WS.Config}}
	if off.ContextVersion(snap).Key() != again.ContextVersion(snap).Key() {
		t.Fatal("context version is not stable while off")
	}
}

func TestCoverageConfigChangeChangesKey(t *testing.T) {
	a, _ := coverageApp(t, true)
	before := a.CoverageKey()
	if err := os.WriteFile(filepath.Join(a.WS.Dir, "coverage.json"), []byte(`{"events": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if a.CoverageKey() == before {
		t.Fatal("coverage.json change keeps the key")
	}
}

func TestBrokenCoverageConfigFails(t *testing.T) {
	a, snap := coverageApp(t, true)
	if err := os.WriteFile(filepath.Join(a.WS.Dir, "coverage.json"), []byte(`{"events": [{"table": "e_events", "time": "missing"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Coverage(snap); err == nil {
		t.Fatal("a reference to a missing column is accepted")
	}
}
