package jsjson_test

import (
	"math"
	"strconv"
	"testing"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
)

type jsonVectors struct {
	Roundtrip []struct{ Input, Output string } `json:"roundtrip"`
	Numbers   []struct{ Bits, Output string }  `json:"numbers"`
	Int64     []struct{ Input, Output string } `json:"int64"`
}

func TestContract(t *testing.T) {
	var v jsonVectors
	if err := contract.Load("json.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Roundtrip {
		got, err := jsjson.Compact([]byte(c.Input))
		if err != nil || got != c.Output {
			t.Errorf("roundtrip %s: got %q (%v), want %q", c.Input, got, err, c.Output)
		}
	}
	for _, c := range v.Numbers {
		b, _ := strconv.ParseUint(c.Bits, 16, 64)
		if got := jsjson.MustStringify(math.Float64frombits(b)); got != c.Output {
			t.Errorf("number %s: got %q, want %q", c.Bits, got, c.Output)
		}
	}
	for _, c := range v.Int64 {
		n, _ := strconv.ParseInt(c.Input, 10, 64)
		if got := jsjson.MustStringify(n); got != c.Output {
			t.Errorf("int64 %s: got %q, want %q", c.Input, got, c.Output)
		}
	}
	t.Logf("%d roundtrip, %d numbers, %d int64", len(v.Roundtrip), len(v.Numbers), len(v.Int64))
}

func TestUndefined(t *testing.T) {
	got := jsjson.MustStringify(jsjson.Object{{"a", jsjson.Undefined}, {"b", []any{jsjson.Undefined}}})
	if got != `{"b":[null]}` {
		t.Fatal(got)
	}
}
