package collect_test

import (
	"encoding/json"
	"testing"

	"growth-lab/internal/collect"
	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
)

type outJS struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
}

// canon re-encodes JSON through jsjson so both sides compare as JSON.stringify bytes.
func canon(t *testing.T, raw []byte) string {
	t.Helper()
	s, err := jsjson.Compact(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func parse(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	v, err := jsjson.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSpecContract(t *testing.T) {
	var v struct {
		Specs []struct {
			Input  json.RawMessage `json:"input"`
			Out    outJS           `json:"out"`
			Hash   string          `json:"hash"`
			DDL    []string        `json:"ddl"`
			Select []string        `json:"select"`
		} `json:"specs"`
		Sensitive []struct {
			Name string  `json:"name"`
			Out  *string `json:"out"`
		} `json:"sensitive"`
	}
	if err := contract.Load("collect-spec.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Specs {
		specs, err := collect.ParseSpec(parse(t, c.Input))
		if !c.Out.OK {
			if err == nil || err.Error() != c.Out.Error {
				t.Errorf("%s: got %v, want error %q", c.Input, err, c.Out.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.Input, err)
			continue
		}
		arr := make([]any, len(specs))
		var ddl, sel []string
		for i, s := range specs {
			arr[i] = s.JS(true)
			ddl = append(ddl, collect.RawDDL(s))
			for _, d := range []collect.Dialect{"mysql", "postgres", "sqlite"} {
				sel = append(sel, collect.SelectSQL(s, d))
			}
		}
		if got, want := jsjson.MustStringify(arr), canon(t, c.Out.Value); got != want {
			t.Errorf("parse %s:\n got %s\nwant %s", c.Input, got, want)
		}
		if got := collect.SpecHash(specs); got != c.Hash {
			t.Errorf("hash %s: got %s want %s", c.Input, got, c.Hash)
		}
		if jsjson.MustStringify(ddl) != jsjson.MustStringify(c.DDL) || jsjson.MustStringify(sel) != jsjson.MustStringify(c.Select) {
			t.Errorf("ddl/select %s", c.Input)
		}
	}
	for _, c := range v.Sensitive {
		want := ""
		if c.Out != nil {
			want = *c.Out
		}
		if got := collect.SensitiveColumnName(c.Name); got != want {
			t.Errorf("sensitive %q: got %q want %q", c.Name, got, want)
		}
	}
	t.Logf("%d specs, %d names", len(v.Specs), len(v.Sensitive))
}

func TestRolesContract(t *testing.T) {
	var v struct {
		Derived []struct {
			Input json.RawMessage `json:"input"`
			Out   outJS           `json:"out"`
			Roles *outJS          `json:"roles"`
		} `json:"derived"`
		Params []struct {
			Input json.RawMessage `json:"input"`
			Hash  string          `json:"hash"`
		} `json:"params"`
	}
	if err := contract.Load("collect-roles.json", &v); err != nil {
		t.Fatal(err)
	}
	base, err := collect.ParseSpec(parse(t, json.RawMessage(`[{"source":"member","target":"r_member","key":["id"],"cutoffColumn":"created_at","columns":[{"expr":"id","as":"id","kind":"int","role":{"identifier":"user"}},{"expr":"created_at","as":"created_at","kind":"ts","role":"ordinary"}]}]`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Derived {
		roles, err := collect.ParseDerivedRoles(parse(t, c.Input))
		if !c.Out.OK {
			if err == nil || err.Error() != c.Out.Error {
				t.Errorf("%s: got %v, want error %q", c.Input, err, c.Out.Error)
			}
			continue
		}
		if err != nil || jsjson.MustStringify(roles.JS()) != canon(t, c.Out.Value) {
			t.Errorf("%s: got %v / %v", c.Input, roles, err)
			continue
		}
		all, err := collect.AllRoles(base, roles)
		if !c.Roles.OK {
			if err == nil || err.Error() != c.Roles.Error {
				t.Errorf("all roles %s: got %v want %q", c.Input, err, c.Roles.Error)
			}
			continue
		}
		var want struct {
			All  json.RawMessage `json:"all"`
			Hash string          `json:"hash"`
		}
		_ = json.Unmarshal(c.Roles.Value, &want)
		if err != nil || jsjson.MustStringify(all.JS()) != canon(t, want.All) || collect.RolesHash(all) != want.Hash {
			t.Errorf("all roles %s: mismatch (%v)", c.Input, err)
		}
	}
	for _, c := range v.Params {
		o, _ := parse(t, c.Input).(jsjson.Object)
		if got := collect.ParamsHash(o); got != c.Hash {
			t.Errorf("params %s: got %s want %s", c.Input, got, c.Hash)
		}
	}
}
