package demo

import (
	"os"
	"path/filepath"
	"testing"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/query"
	"growth-lab/internal/snapshot"
	"growth-lab/internal/workspace"
)

// The generated data goes through collect, derive and every seed panel of the demo workspace.
func TestDemoPipeline(t *testing.T) {
	src := filepath.Join("..", "..", "examples", "demo")
	dir := t.TempDir()
	for _, f := range []string{"workspace.json", "tables.json", "derived.sql", "derived-columns.json"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, f), b, 0o644)
	}
	if _, err := Seed(filepath.Join(dir, ".out", "source.sqlite"), Options{Anchor: "2024-06-03 12:00:00"}); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := snapshot.RunCollect(ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := snapshot.RunDerive(ws); err != nil || !d.Reused || d.SnapshotID != r.SnapshotID {
		t.Fatalf("derive: %+v %v", d, err)
	}
	panels, _ := filepath.Glob(filepath.Join(src, "seed-panels", "*.json"))
	if len(panels) == 0 {
		t.Fatal("no seed panels")
	}
	cur, _ := snapshot.ReadCurrent(snapshot.Dir(ws.Config.OutDir))
	for _, p := range panels {
		b, _ := os.ReadFile(p)
		v, err := jsjson.Parse(string(b))
		if err != nil {
			t.Fatal(err)
		}
		sql, _ := v.(jsjson.Object).Get("sql")
		params := jsjson.Object{{Key: "as_of", Value: "2024-06-03 12:00:00.000000"}}
		for _, m := range ws.Config.Params {
			if s, ok := m.Value.(string); ok {
				params = append(params, jsjson.Member{Key: m.Key, Value: s})
			}
		}
		for _, path := range []string{r.Files.Real, cur.AgentFile} {
			used := jsjson.Object{}
			for _, m := range params {
				if containsParam(sql.(string), m.Key) {
					used = append(used, m)
				}
			}
			out := query.Run(&query.Input{Path: path, SQL: sql.(string), ReadablePrefixes: ws.Config.PanelPrefixes, HeapLimitMb: 256, MaxRows: 5000, Overflow: "error", CellLimit: 4096, OutputLimit: 4 << 20, Params: used})
			if ok, _ := out.Get("ok"); ok != true {
				t.Errorf("%s on %s: %s", filepath.Base(p), filepath.Base(path), jsjson.MustStringify(out))
				continue
			}
			if rows, _ := out.Get("rows"); len(rows.([]any)) == 0 {
				t.Errorf("%s: no rows", filepath.Base(p))
			}
		}
	}
	t.Logf("snapshot %s, %d seed panels", r.SnapshotID, len(panels))
}

func containsParam(sql, name string) bool {
	for i := 0; i+len(name) < len(sql); i++ {
		if sql[i] == ':' && sql[i+1:i+1+len(name)] == name && (i+1+len(name) == len(sql) || !isWord(sql[i+1+len(name)])) {
			return true
		}
	}
	return false
}

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
