package demo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"growth-lab/internal/snapshot"
	"growth-lab/internal/sqlitec"
	"growth-lab/internal/workspace"
)

func addLines(t *testing.T, path, sql string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, []byte(sql)...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setRole adds (role != "") or removes one derived column role.
func setRole(t *testing.T, path, key, role string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	entry := "\n  \"" + key + "\": \"" + role + "\","
	if role == "" {
		s = strings.Replace(s, "\n  \""+key+"\": \"ordinary\",", "", 1)
	} else {
		s = strings.Replace(s, "{", "{"+entry, 1)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A rebuild with changed derived SQL gets fresh statistics in both files, even when derived SQL drops its own table.
func TestDemoRederiveStatistics(t *testing.T) {
	dir := demoDir(t)
	derived, roles := filepath.Join(dir, "derived.sql"), filepath.Join(dir, "derived-columns.json")
	original, _ := os.ReadFile(derived)
	addLines(t, derived, "\nCREATE TABLE d_gone AS SELECT country FROM d_member;\nCREATE INDEX d_gone_country ON d_gone(country);\n")
	setRole(t, roles, "d_gone.country", "ordinary")
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := snapshot.RunCollect(ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{r.Files.Real, r.Files.Agent} {
		if count(t, path, "SELECT count(*) FROM sqlite_stat1 WHERE tbl = 'd_gone'") == 0 {
			t.Fatalf("%s: no statistics for d_gone after collect", filepath.Base(path))
		}
	}

	os.WriteFile(derived, original, 0o644)
	addLines(t, derived, "\nCREATE INDEX d_member_country ON d_member(country);\nCREATE TABLE d_tmp AS SELECT 1 AS x;\nDROP TABLE d_tmp;\n")
	setRole(t, roles, "d_gone.country", "")
	if ws, err = workspace.Load(dir); err != nil {
		t.Fatal(err)
	}
	d, err := snapshot.RunDerive(ws)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if d.Reused || d.SnapshotID == r.SnapshotID {
		t.Fatalf("derive reused the old snapshot: %+v", d)
	}
	if d.AnalyzeReal <= 0 || d.AnalyzeAgent <= 0 {
		t.Errorf("analyze times not recorded: %v %v", d.AnalyzeReal, d.AnalyzeAgent)
	}
	for _, path := range []string{d.Files.Real, d.Files.Agent} {
		for _, st := range []string{"sqlite_stat1", "sqlite_stat4"} {
			if count(t, path, "SELECT count(*) FROM "+st) == 0 {
				t.Errorf("%s: no %s rows", filepath.Base(path), st)
			}
		}
		if count(t, path, "SELECT count(*) FROM sqlite_stat1 WHERE tbl = 'd_gone'") != 0 {
			t.Errorf("%s: statistics of the dropped table remain", filepath.Base(path))
		}
	}
	if count(t, d.Files.Real, "SELECT count(*) FROM sqlite_stat1 WHERE idx = 'd_member_country'") == 0 {
		t.Errorf("no statistics for the new index")
	}
}

// A failed ANALYZE deletes the temporary files and keeps the current snapshot.
func TestFinalizeAnalyzeFailure(t *testing.T) {
	ws, err := workspace.Load(demoDir(t))
	if err != nil {
		t.Fatal(err)
	}
	r, err := snapshot.RunCollect(ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := snapshot.LoadBuildInputs(ws)
	if err != nil {
		t.Fatal(err)
	}
	snaps := snapshot.Dir(ws.Config.OutDir)
	before, _ := os.ReadDir(snaps)
	for _, stage := range []string{"analyze", "analyze-agent"} {
		tmp := filepath.Join(snaps, "tmp-"+stage+".sqlite")
		b, _ := os.ReadFile(r.Files.Real)
		os.WriteFile(tmp, b, 0o644)
		db, err := sqlitec.Open(tmp, false)
		if err != nil {
			t.Fatal(err)
		}
		names, _ := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'd\_%' ESCAPE '\'`)
		for _, n := range names {
			db.Exec(`DROP TABLE "` + n[0].Text + `"`)
		}
		db.Exec("DROP TABLE sqlite_stat1; DROP TABLE sqlite_stat4")
		db.Close()
		_, err = snapshot.Finalize(snapshot.FinalizeOptions{TmpPath: tmp, SnapshotsDir: snaps, Inputs: inputs, Meta: snapshot.Meta{Cutoff: "2024-06-03 12:00:00.000000", StartedAt: "2024-06-03 12:00:00.000000", FinishedAt: "2024-06-03 12:00:00.000000"}, LockHeld: true, FailAt: stage})
		if err == nil || !strings.Contains(err.Error(), "forced failure ("+stage+")") {
			t.Fatalf("%s: %v", stage, err)
		}
		for _, p := range []string{tmp, tmp + ".agent", tmp + ".map", tmp + "-journal", tmp + ".agent-journal"} {
			if _, err := os.Stat(p); err == nil {
				t.Errorf("%s: %s left behind", stage, filepath.Base(p))
			}
		}
		cur, _ := snapshot.ReadCurrent(snaps)
		if cur == nil || cur.SnapshotID != r.SnapshotID {
			t.Errorf("%s: current snapshot changed: %+v", stage, cur)
		}
	}
	after, _ := os.ReadDir(snaps)
	if len(after) != len(before) {
		t.Errorf("snapshot folder changed: %d -> %d entries", len(before), len(after))
	}
}
