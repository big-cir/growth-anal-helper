package demo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/sqlitec"
)

// digest hashes every table with storage classes (quote()).
func digest(t *testing.T, path string) map[string]string {
	db, err := sqlitec.Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := map[string]string{}
	rows, names, err := db.QueryJS("SELECT name, sql FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	objs := make([]any, len(rows))
	for i, r := range rows {
		objs[i] = jsjson.Object{{Key: names[0], Value: r[0]}, {Key: names[1], Value: r[1]}}
	}
	sum := sha256.Sum256([]byte(jsjson.MustStringify(objs)))
	out["(schema)"] = hex.EncodeToString(sum[:])
	for _, r := range rows {
		name := r[0].(string)
		info, err := db.Query(`PRAGMA table_info("` + name + `")`)
		if err != nil {
			t.Fatal(err)
		}
		cols := ""
		for i, c := range info {
			if i > 0 {
				cols += ", "
			}
			cols += `quote("` + c[1].Text + `")`
		}
		data, _, err := db.QueryJS(`SELECT ` + cols + ` FROM "` + name + `" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		for _, row := range data {
			h.Write([]byte(jsjson.MustStringify(row) + "\n"))
		}
		out[name] = fmt.Sprintf("%d %s", len(data), hex.EncodeToString(h.Sum(nil)))
	}
	return out
}

func TestSeedContract(t *testing.T) {
	var v []struct {
		Options struct {
			Anchor  string   `json:"anchor"`
			Seed    *float64 `json:"seed"`
			Weeks   *int     `json:"weeks"`
			Members *int     `json:"members"`
		} `json:"options"`
		Summary string            `json:"summary"`
		Tables  map[string]string `json:"tables"`
	}
	if err := contract.Load("misc-demo.json", &v); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for i, c := range v {
		p := filepath.Join(dir, fmt.Sprintf("sub/s%d.sqlite", i))
		s, err := Seed(p, Options{Anchor: c.Options.Anchor, Seed: c.Options.Seed, Weeks: c.Options.Weeks, Members: c.Options.Members})
		if err != nil {
			t.Fatal(err)
		}
		if s.JSON() != c.Summary {
			t.Errorf("%s: summary %s, want %s", c.Options.Anchor, s.JSON(), c.Summary)
		}
		got := digest(t, p)
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(c.Tables)
		if string(a) != string(b) {
			t.Errorf("%s: tables\n got %s\nwant %s", c.Options.Anchor, a, b)
		}
		// seeding again replaces the file
		if s2, err := Seed(p, Options{Anchor: c.Options.Anchor, Seed: c.Options.Seed, Weeks: c.Options.Weeks, Members: c.Options.Members}); err != nil || s2 != s {
			t.Errorf("reseed: %v", err)
		}
	}
	if _, err := Seed(filepath.Join(dir, "x.sqlite"), Options{Anchor: "2024-1-1"}); err == nil || err.Error() != "invalid anchor: 2024-1-1" {
		t.Errorf("bad anchor: %v", err)
	}
	t.Logf("%d seeds", len(v))
}
