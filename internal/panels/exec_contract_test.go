package panels_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
)

func TestStoreContract(t *testing.T) {
	var v struct {
		Panels []struct {
			Input json.RawMessage `json:"input"`
			Get   json.RawMessage `json:"get"`
			File  string          `json:"file"`
		} `json:"panels"`
		List []string `json:"list"`
	}
	if err := contract.Load("exec-store.json", &v); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := panels.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Panels {
		raw, _ := jsjson.Parse(string(c.Input))
		id, _ := raw.(jsjson.Object).Get("id")
		if err := os.WriteFile(filepath.Join(dir, id.(string)+".json"), []byte(jsjson.MustStringify(raw)), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := st.Get(id.(string))
		if err != nil || p == nil {
			t.Fatalf("get %v: %v", id, err)
		}
		want, _ := jsjson.Compact(c.Get)
		if got := jsjson.MustStringify(p.JS()); got != want {
			t.Errorf("get %v:\n got %s\nwant %s", id, got, want)
		}
		if err := st.Write(*p); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, id.(string)+".json"))
		if string(b) != c.File {
			t.Errorf("file %v:\n got %s\nwant %s", id, b, c.File)
		}
	}
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range list {
		ids = append(ids, p.Str("id"))
	}
	if jsjson.MustStringify(toAny(ids)) != jsjson.MustStringify(toAny(v.List)) {
		t.Errorf("list %v want %v", ids, v.List)
	}
	if p, _ := st.Get("NOT-AN-ID"); p != nil {
		t.Error("invalid id")
	}
	t.Logf("%d panels", len(v.Panels))
}

func toAny(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func TestRecomputeContract(t *testing.T) {
	var v []struct {
		Status   string          `json:"status"`
		Saved    json.RawMessage `json:"saved"`
		Current  json.RawMessage `json:"current"`
		Last     string          `json:"last"`
		Decision string          `json:"decision"`
		Changed  []string        `json:"changed"`
	}
	if err := contract.Load("exec-recompute.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v {
		saved, _ := jsjson.Parse(string(c.Saved))
		cur, _ := jsjson.Parse(string(c.Current))
		p := panels.SavedPanel{Fields: jsjson.Object{{Key: "status", Value: c.Status}, {Key: "versions", Value: saved}, {Key: "last_result", Value: jsjson.Object{{Key: "snapshot_id", Value: c.Last}}}}}
		if got := panels.Decide(p, cur.(jsjson.Object)); got != c.Decision {
			t.Errorf("%s %s: got %s want %s", c.Status, c.Current, got, c.Decision)
		}
		if got, want := jsjson.MustStringify(toAny(panels.ChangedRules(saved.(jsjson.Object), cur.(jsjson.Object)))), jsjson.MustStringify(toAny(c.Changed)); got != want {
			t.Errorf("changed %s: got %s want %s", c.Current, got, want)
		}
	}
	t.Logf("%d cases", len(v))
}
