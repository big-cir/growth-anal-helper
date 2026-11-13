package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
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

// addEvents writes event rows into a snapshot file.
func addEvents(t *testing.T, path string, times ...string) {
	t.Helper()
	db, err := sqlitec.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, at := range times {
		if _, err := db.RunOnce("INSERT INTO e_events VALUES ('a', ?)", at); err != nil {
			t.Fatal(err)
		}
	}
}

func viewDataEnd(s *PanelService, snap *Snapshot, resultSnapshot string) any {
	p := &panels.SavedPanel{Fields: jsjson.Object{{Key: "id", Value: "p00000000001"}, {Key: "last_result", Value: jsjson.Object{{Key: "snapshot_id", Value: resultSnapshot}, {Key: "as_of", Value: "2018-09-22 10:00:00.000000"}}}}}
	lr, _ := s.View(p, snap, false).Get("last_result")
	v, _ := lr.(jsjson.Object).Get("data_end")
	return v
}

func TestDataEndDoesNotChangeVersions(t *testing.T) {
	a, fresh := coverageApp(t, true)
	stale := &Snapshot{ID: "s2", Real: filepath.Join(a.WS.Dir, "snap2.sqlite"), AsOf: fresh.AsOf}
	b, err := os.ReadFile(fresh.Real)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale.Real, b, 0o644); err != nil {
		t.Fatal(err)
	}
	addEvents(t, fresh.Real, "2018-09-22 08:00:00.000000")
	addEvents(t, stale.Real, "2018-09-19 10:00:00.000000")
	before := a.PanelVersions(fresh)
	rf, err := a.Coverage(fresh)
	if err != nil || rf.AppEnd != "2018-09-21" {
		t.Fatalf("fresh %v %+v", err, rf)
	}
	rs, err := a.Coverage(stale)
	if err != nil || rs.AppEnd != "2018-09-18" || !strings.Contains(rs.Guide(), "records end at 2018-09-19 10:00") {
		t.Fatalf("stale %v %+v", err, rs)
	}
	after := a.PanelVersions(stale)
	if changed := panels.ChangedRules(before, after); len(changed) != 0 {
		t.Fatalf("rule versions changed: %v", changed)
	}
	p := panels.SavedPanel{Fields: jsjson.Object{{Key: "status", Value: "ok"}, {Key: "versions", Value: before}, {Key: "last_result", Value: jsjson.Object{{Key: "snapshot_id", Value: fresh.ID}}}}}
	if d := panels.Decide(p, after); d != "auto" {
		t.Fatalf("decision %s", d)
	}
}

func TestPanelViewDataEnd(t *testing.T) {
	a, snap := coverageApp(t, true)
	addEvents(t, snap.Real, "2018-09-19 10:00:00.000000")
	s := &PanelService{app: a}
	if v := viewDataEnd(s, snap, snap.ID); v != "2018-09-18" {
		t.Fatalf("current snapshot result: %v", v)
	}
	if v := viewDataEnd(s, snap, "s0"); v != nil {
		t.Fatalf("other snapshot result: %v", v)
	}
	if v := viewDataEnd(s, nil, snap.ID); v != nil {
		t.Fatalf("no snapshot: %v", v)
	}
	// the view makes no gap copies even though the workspace has a gap
	if gaps, _ := filepath.Glob(filepath.Join(a.WS.Dir, "*.gap*")); len(gaps) != 0 {
		t.Fatalf("gap copies made by the view: %v", gaps)
	}
	off, offSnap := coverageApp(t, false)
	addEvents(t, offSnap.Real, "2018-09-19 10:00:00.000000")
	if v := viewDataEnd(&PanelService{app: off}, offSnap, offSnap.ID); v != nil {
		t.Fatalf("coverage check off: %v", v)
	}
}

func TestDataEndRetriesAfterFailure(t *testing.T) {
	a, snap := coverageApp(t, true)
	addEvents(t, snap.Real, "2018-09-19 10:00:00.000000")
	moved := snap.Real + ".moved"
	if err := os.Rename(snap.Real, moved); err != nil {
		t.Fatal(err)
	}
	s := &PanelService{app: a}
	if v := viewDataEnd(s, snap, snap.ID); v != nil {
		t.Fatalf("unreadable snapshot: %v", v)
	}
	if err := os.Rename(moved, snap.Real); err != nil {
		t.Fatal(err)
	}
	if v := viewDataEnd(s, snap, snap.ID); v != "2018-09-18" {
		t.Fatalf("no retry after recovery: %v", v)
	}
}
