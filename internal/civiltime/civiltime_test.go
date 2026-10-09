package civiltime_test

import (
	"encoding/json"
	"testing"

	"growth-lab/internal/civiltime"
	"growth-lab/internal/contract"
)

type timeVectors struct {
	Normalize []struct {
		Input string
		Out   contract.Out
	} `json:"normalize"`
	IsNormalized []struct {
		Input string
		Out   contract.Out
	} `json:"isNormalized"`
	WeekStart []struct {
		Input string
		Out   contract.Out
	} `json:"weekStart"`
	AddDays []struct {
		Input string
		N     int
		Out   contract.Out
	} `json:"addDays"`
}

func check(t *testing.T, name, input string, want contract.Out, got any, err error) {
	t.Helper()
	if !want.OK {
		if err == nil || err.Error() != want.Error {
			t.Errorf("%s(%q): got %v / %v, want error %q", name, input, got, err, want.Error)
		}
		return
	}
	b, _ := json.Marshal(got)
	if err != nil || string(b) != string(want.Value) {
		t.Errorf("%s(%q): got %s (%v), want %s", name, input, b, err, want.Value)
	}
}

func TestContract(t *testing.T) {
	var v timeVectors
	if err := contract.Load("time.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Normalize {
		got, err := civiltime.Normalize(c.Input)
		check(t, "Normalize", c.Input, c.Out, got, err)
	}
	for _, c := range v.IsNormalized {
		check(t, "IsNormalized", c.Input, c.Out, civiltime.IsNormalized(c.Input), nil)
	}
	for _, c := range v.WeekStart {
		got, err := civiltime.WeekStart(c.Input)
		check(t, "WeekStart", c.Input, c.Out, got, err)
	}
	for _, c := range v.AddDays {
		got, err := civiltime.AddDays(c.Input, c.N)
		check(t, "AddDays", c.Input, c.Out, got, err)
	}
	t.Logf("%d cases", len(v.Normalize)+len(v.IsNormalized)+len(v.WeekStart)+len(v.AddDays))
}
