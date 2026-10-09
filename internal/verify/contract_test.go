package verify

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
)

type out struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
}

func TestContract(t *testing.T) {
	var v struct {
		Files []struct {
			Hex string `json:"hex"`
			Out out    `json:"out"`
		} `json:"files"`
		Weeks []struct {
			AsOf string  `json:"asOf"`
			Week *string `json:"week"`
			Out  out     `json:"out"`
		} `json:"weeks"`
		Values []struct {
			A, B *string
			Same bool `json:"same"`
		} `json:"values"`
		Bind []struct {
			Hex string `json:"hex"`
			Out struct {
				OK      bool   `json:"ok"`
				SQL     string `json:"sql"`
				Message string `json:"message"`
			} `json:"out"`
		} `json:"bind"`
	}
	if err := contract.Load("exec-verify.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Files {
		text, _ := hex.DecodeString(c.Hex)
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "verify.json"), text, 0o644)
		checks, err := LoadChecks(dir)
		if !c.Out.OK {
			if err == nil || err.Error() != c.Out.Error {
				t.Errorf("%s: got %v want %q", text, err, c.Out.Error)
			}
			continue
		}
		arr := make([]any, len(checks))
		for i, ch := range checks {
			arr[i] = jsjson.Object{{Key: "id", Value: ch.ID}, {Key: "title", Value: ch.Title}, {Key: "source_sql", Value: ch.SourceSQL}, {Key: "snapshot_sql", Value: ch.SnapshotSQL}}
		}
		if want, _ := jsjson.Compact(c.Out.Value); err != nil || jsjson.MustStringify(arr) != want {
			t.Errorf("%s: got %v", text, err)
		}
	}
	for _, c := range v.Weeks {
		start, end, err := Week(c.AsOf, c.Week)
		if !c.Out.OK {
			if err == nil || err.Error() != c.Out.Error {
				t.Errorf("week %v: got %v want %q", c.Week, err, c.Out.Error)
			}
			continue
		}
		got := jsjson.MustStringify(jsjson.Object{{Key: "start", Value: start}, {Key: "end", Value: end}})
		if want, _ := jsjson.Compact(c.Out.Value); err != nil || got != want {
			t.Errorf("week %v: got %s %v", c.Week, got, err)
		}
	}
	for _, c := range v.Values {
		if SameValue(c.A, c.B) != c.Same {
			t.Errorf("sameValue %v %v", c.A, c.B)
		}
	}
	values := map[string]string{"week_start": "2024-04-29 00:00:00.000000", "week_end": "2024-05-06 00:00:00.000000", "as_of": "2024-06-03 12:00:00.000000"}
	for _, c := range v.Bind {
		sql, _ := hex.DecodeString(c.Hex)
		got, err := BindSourceSQL(string(sql), values)
		if c.Out.OK != (err == nil) || (err == nil && got != c.Out.SQL) || (err != nil && err.Error() != c.Out.Message) {
			t.Errorf("bind %s: got %q %v, want %+v", sql, got, err, c.Out)
		}
	}
	t.Logf("%d files, %d weeks, %d values, %d bind", len(v.Files), len(v.Weeks), len(v.Values), len(v.Bind))
}
