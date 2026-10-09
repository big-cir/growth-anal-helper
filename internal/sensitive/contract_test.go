package sensitive_test

import (
	"encoding/hex"
	"testing"

	"growth-lab/internal/contract"
	"growth-lab/internal/sensitive"
)

func TestContract(t *testing.T) {
	var v []struct {
		Hex        string  `json:"hex"`
		Normalized string  `json:"normalized"`
		Topic      *string `json:"topic"`
		Shape      *string `json:"shape"`
		Assignment bool    `json:"assignment"`
	}
	if err := contract.Load("sensitive.json", &v); err != nil {
		t.Fatal(err)
	}
	s := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	for _, c := range v {
		b, _ := hex.DecodeString(c.Hex)
		text := string(b)
		if got := sensitive.NormalizeText(text); got != c.Normalized {
			t.Errorf("normalize %q: got %q want %q", text, got, c.Normalized)
		}
		if got := sensitive.Topic(text); got != s(c.Topic) {
			t.Errorf("topic %q: got %q want %q", text, got, s(c.Topic))
		}
		if got := sensitive.SecretShape(text); got != s(c.Shape) {
			t.Errorf("shape %q: got %q want %q", text, got, s(c.Shape))
		}
		if got := sensitive.SecretAssignment(text); got != c.Assignment {
			t.Errorf("assignment %q: got %v want %v", text, got, c.Assignment)
		}
	}
	t.Logf("%d texts", len(v))
}
