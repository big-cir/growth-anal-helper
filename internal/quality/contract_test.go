package quality_test

import (
	"encoding/json"
	"testing"

	"growth-lab/internal/collect"
	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/quality"
)

func TestBuiltinContract(t *testing.T) {
	var v []struct {
		Lang   string          `json:"lang"`
		Specs  json.RawMessage `json:"specs"`
		Params json.RawMessage `json:"params"`
		Out    struct {
			OK    bool            `json:"ok"`
			Value json.RawMessage `json:"value"`
			Error string          `json:"error"`
		} `json:"out"`
	}
	if err := contract.Load("exec-quality.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v {
		raw, _ := jsjson.Parse(string(c.Specs))
		specs, err := collect.ParseSpec(raw)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := jsjson.Parse(string(c.Params))
		checks, err := quality.Builtin(specs, p.(jsjson.Object), c.Lang)
		if !c.Out.OK {
			if err == nil || err.Error() != c.Out.Error {
				t.Errorf("%s: got %v want %q", c.Params, err, c.Out.Error)
			}
			continue
		}
		arr := make([]any, len(checks))
		for i, ch := range checks {
			arr[i] = ch.JS()
		}
		want, _ := jsjson.Compact(c.Out.Value)
		if err != nil || jsjson.MustStringify(arr) != want {
			t.Errorf("%s %s: got %v %s", c.Lang, c.Params, err, jsjson.MustStringify(arr))
		}
	}
	t.Logf("%d cases", len(v))
}
