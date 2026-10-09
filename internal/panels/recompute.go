package panels

import "growth-lab/internal/jsjson"

// RuleKeys are versions that send a panel to review instead of recomputing automatically when they change.
var RuleKeys = []string{"schema_version", "policy_version", "docs_version", "prompt_version", "pattern_contract_version"}

// ChangedRules lists the rule versions that differ.
func ChangedRules(saved, current jsjson.Object) []string {
	var out []string
	for _, k := range RuleKeys {
		a, _ := saved.Get(k)
		b, _ := current.Get(k)
		if !sameScalar(a, b) {
			out = append(out, k)
		}
	}
	return out
}

func sameScalar(a, b any) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	}
	return false
}

// Decide is none, auto (recompute) or review for a saved panel when the current versions are given.
func Decide(p SavedPanel, current jsjson.Object) string {
	if len(ChangedRules(p.Obj("versions"), current)) > 0 {
		if p.Str("status") == "review" {
			return "none"
		}
		return "review"
	}
	if p.Str("status") == "review" {
		return "none"
	}
	lr := p.Obj("last_result")
	sid, _ := lr.Get("snapshot_id")
	cur, _ := current.Get("snapshot_id")
	if !sameScalar(sid, cur) {
		return "auto"
	}
	return "none"
}
