package snapshot_test

import (
	"os"
	"path/filepath"
	"testing"

	"growth-lab/internal/collect"
	"growth-lab/internal/snapshot"
	"growth-lab/internal/sqlitec"
	"growth-lab/internal/workspace"
)

// Opt-in: GROWTH_LAB_REAL_WORKSPACE=<dir> checks the Go hashes against the current snapshot's meta.
func TestRealWorkspaceHashes(t *testing.T) {
	dir := os.Getenv("GROWTH_LAB_REAL_WORKSPACE")
	if dir == "" {
		t.Skip("GROWTH_LAB_REAL_WORKSPACE not set")
	}
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	specs, err := collect.LoadSpec(filepath.Join(dir, "tables.json"))
	if err != nil {
		t.Fatal(err)
	}
	cur, err := snapshot.ReadCurrent(snapshot.Dir(ws.Config.OutDir))
	if err != nil || cur == nil {
		t.Fatal("no current snapshot", err)
	}
	db, err := sqlitec.Open(cur.File, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, _, err := db.QueryJS("SELECT derived_hash, roles_hash, params_hash, spec_hash, ga4_spec_hash FROM snapshot_meta")
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	inputs, err := snapshot.LoadBuildInputs(ws)
	if err != nil {
		t.Fatal(err)
	}
	h, err := snapshot.HashInputs(inputs)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{h.Derived, h.Roles, h.Params, h.Spec, h.GA4Spec}
	for i, name := range []string{"derived_hash", "roles_hash", "params_hash", "spec_hash", "ga4_spec_hash"} {
		if got[i] != rows[0][i] {
			t.Errorf("%s: go %s, snapshot %v", name, got[i], rows[0][i])
		}
	}
	_ = specs
}
