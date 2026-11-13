package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"growth-lab/internal/coverage"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/snapshot"
	"growth-lab/internal/sqlitec"
	"growth-lab/internal/workspace"
)

// Opt-in: GROWTH_LAB_REAL_WORKSPACE=<dir> prints the data end of each real snapshot and the saved panel decisions.
// Read only: no gap copies, no panel writes.
func TestRealWorkspaceDataEnd(t *testing.T) {
	dir := os.Getenv("GROWTH_LAB_REAL_WORKSPACE")
	if dir == "" {
		t.Skip("GROWTH_LAB_REAL_WORKSPACE not set")
	}
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := coverage.LoadConfig(ws.Dir)
	if err != nil {
		t.Fatal(err)
	}
	snapDir := snapshot.Dir(ws.Config.OutDir)
	files, _ := filepath.Glob(filepath.Join(snapDir, "*.sqlite"))
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasSuffix(base, ".agent.sqlite") || strings.HasSuffix(base, ".pseudo-map.sqlite") || strings.Contains(base, ".gap") || strings.HasPrefix(base, "tmp-") {
			continue
		}
		asOf := realAsOf(t, f)
		if len(asOf) < 10 {
			t.Errorf("%s: no as_of", base)
			continue
		}
		day, _ := time.Parse("2006-01-02", asOf[:10])
		old := day.AddDate(0, 0, -1).Format("2006-01-02")
		fr, err := coverage.ReadFreshness(cfg, f, asOf)
		if err != nil {
			t.Errorf("%s: %v", base, err)
			continue
		}
		t.Logf("snapshot %s: old data_end %s, new data_end %s, changed %v", strings.TrimSuffix(base, ".sqlite"), old, fr.End, old != fr.End)
	}

	a, err := NewApp(ws)
	if err != nil {
		t.Fatal(err)
	}
	snap := a.Snapshot()
	if snap == nil {
		t.Fatal("no current snapshot")
	}
	cur := a.PanelVersions(snap)
	for _, k := range panels.RuleKeys {
		v, _ := cur.Get(k)
		s := jsjson.MustStringify(v)
		if len(s) > 14 {
			s = s[:14]
		}
		t.Logf("version %s: %s", k, s)
	}
	store := &panels.Store{Dir: filepath.Join(ws.Config.OutDir, "panels")}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, p := range list {
		count[panels.Decide(*p, cur)]++
	}
	t.Logf("saved panels %d: review %d, auto %d, none %d", len(list), count["review"], count["auto"], count["none"])
}

func realAsOf(t *testing.T, path string) string {
	t.Helper()
	db, err := sqlitec.Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT source_cutoff_at FROM snapshot_meta")
	if err != nil || len(rows) != 1 || rows[0][0].Type != sqlitec.Text {
		return ""
	}
	return rows[0][0].Text
}
