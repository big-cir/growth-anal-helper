package workspace_test

import (
	"encoding/json"
	"testing"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/workspace"
)

func TestContract(t *testing.T) {
	var v struct {
		Workspaces []struct {
			Input json.RawMessage `json:"input"`
			Out   struct {
				OK    bool            `json:"ok"`
				Value json.RawMessage `json:"value"`
				Error string          `json:"error"`
			} `json:"out"`
		} `json:"workspaces"`
		Raw []struct {
			Text string `json:"text"`
			Out  struct {
				OK    bool            `json:"ok"`
				Value json.RawMessage `json:"value"`
				Error string          `json:"error"`
			} `json:"out"`
		} `json:"raw"`
		Expand []struct{ Input, Out string } `json:"expand"`
	}
	if err := contract.Load("collect-workspace.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Workspaces {
		raw, err := jsjson.Parse(string(c.Input))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := workspace.ParseConfig(raw, "/ws/demo", "/hh/u")
		if !c.Out.OK {
			if err == nil || err.Error() != c.Out.Error {
				t.Errorf("%s:\n got %v\nwant error %q", c.Input, err, c.Out.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.Input, err)
			continue
		}
		want, _ := jsjson.Compact(c.Out.Value)
		if got := jsjson.MustStringify(cfg.JS()); got != want {
			t.Errorf("%s:\n got %s\nwant %s", c.Input, got, want)
		}
	}
	for _, c := range v.Raw {
		raw, _ := jsjson.Parse(c.Text)
		cfg, err := workspace.ParseConfig(raw, "/ws/demo", "/hh/u")
		if !c.Out.OK {
			if err == nil || err.Error() != c.Out.Error {
				t.Errorf("raw %s: got %v want %q", c.Text, err, c.Out.Error)
			}
		} else if want, _ := jsjson.Compact(c.Out.Value); err != nil || jsjson.MustStringify(cfg.JS()) != want {
			t.Errorf("raw %s: got %v", c.Text, err)
		}
	}
	for _, c := range v.Expand {
		if got := workspace.ExpandHome(c.Input, "/hh/u"); got != c.Out {
			t.Errorf("expand %q: got %q want %q", c.Input, got, c.Out)
		}
	}
	t.Logf("%d configs", len(v.Workspaces))
}

// A user in the host is rejected (built at run time: the string looks like a connection string).
func TestUserInHost(t *testing.T) {
	raw, _ := jsjson.Parse(`{"name":"x","datasource":{"host":"mysql://u` + "@" + `h","user":"u","database":"d"},"policy":{"readablePrefixes":["r_"]}}`)
	_, err := workspace.ParseConfig(raw, "/ws", "/hh/u")
	if err == nil || err.Error() != "workspace.json .datasource.host: host[:port] only (user and database go in their own keys)" {
		t.Fatal(err)
	}
}
