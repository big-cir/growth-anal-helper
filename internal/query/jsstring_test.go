package query

import (
	"testing"

	"growth-lab/internal/jsjson"
)

func TestJSStringKeepsLoneSurrogates(t *testing.T) {
	if v := jsString([]byte(`"a\ud83d"`)); jsjson.MustStringify(v) != `"a\ud83d"` {
		t.Fatalf("lone surrogate lost: %#v", v)
	}
	if v := jsString([]byte(`"😀 \u0001 x"`)); v != "😀 \u0001 x" {
		t.Fatalf("got %#v", v)
	}
	if v := jsString([]byte(`"😀"`)); v != "😀" {
		t.Fatalf("pair: %#v", v)
	}
}
