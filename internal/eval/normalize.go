package eval

import (
	"fmt"
	"growth-lab/internal/jsstr"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"

	"growth-lab/internal/civiltime"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/verify"
)

// Cell is a normalized value; nil is NULL.
type Cell = *string

// RoleRow is one result row: its key cells and measure values.
type RoleRow struct {
	Key    []Cell
	Values []NamedCell
}

// NamedCell is one measure value or label.
type NamedCell struct {
	Name string
	Cell Cell
}

// LabelSet maps a JSON row key to its label, in insertion order.
type LabelSet struct {
	Role   string
	Labels []NamedCell
}

// RoleRows is a panel result as role rows (keys, measures, labels).
type RoleRows struct {
	Pattern string
	// number only: "rate" (numerator/denominator) or "value"; nil otherwise
	Representation *string
	Keys           []string
	Measures       []string
	Labels         []LabelSet
	Rows           []RoleRow
}

func (r RoleRow) value(m string) Cell {
	for _, v := range r.Values {
		if v.Name == m {
			return v.Cell
		}
	}
	return nil
}

func (r RoleRows) labels(role string) (*LabelSet, bool) {
	for i := range r.Labels {
		if r.Labels[i].Role == role {
			return &r.Labels[i], true
		}
	}
	return nil, false
}

func (l *LabelSet) get(k string) (Cell, bool) {
	for _, x := range l.Labels {
		if x.Name == k {
			return x.Cell, true
		}
	}
	return nil, false
}

// NormalizeFailure: the result can't be put into role rows (normalization_collision, key_unparseable).
type NormalizeFailure struct{ Code, Msg string }

func (e *NormalizeFailure) Error() string { return e.Msg }

func cellJS(c Cell) any {
	if c == nil {
		return nil
	}
	return *c
}

func cellsJS(cs []Cell) []any {
	out := make([]any, len(cs))
	for i, c := range cs {
		out[i] = cellJS(c)
	}
	return out
}

// keyJSON is JSON.stringify(key).
func keyJSON(key []Cell) string { return jsjson.MustStringify(cellsJS(key)) }

// JS returns the role rows as the report and fixture files store them.
func (r RoleRows) JS() jsjson.Object {
	var rep any
	if r.Representation != nil {
		rep = *r.Representation
	}
	labels := jsjson.Object{}
	for _, l := range r.Labels {
		o := jsjson.Object{}
		for _, x := range l.Labels {
			o = append(o, jsjson.Member{Key: x.Name, Value: cellJS(x.Cell)})
		}
		labels = append(labels, jsjson.Member{Key: l.Role, Value: o})
	}
	rows := make([]any, len(r.Rows))
	for i, row := range r.Rows {
		vals := jsjson.Object{}
		for _, v := range row.Values {
			vals = append(vals, jsjson.Member{Key: v.Name, Value: cellJS(v.Cell)})
		}
		rows[i] = jsjson.Object{{Key: "key", Value: cellsJS(row.Key)}, {Key: "values", Value: vals}}
	}
	return jsjson.Object{{Key: "pattern", Value: r.Pattern}, {Key: "representation", Value: rep}, {Key: "keys", Value: r.Keys}, {Key: "measures", Value: r.Measures}, {Key: "labels", Value: labels}, {Key: "rows", Value: rows}}
}

func toCell(v any) Cell {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return &x
	case float64:
		s := jsjson.Number(x)
		return &s
	}
	s := jsString(v)
	return &s
}

func strs(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// roleRowsFrom reads role rows stored in a fixture.
func roleRowsFrom(o jsjson.Object) RoleRows {
	var r RoleRows
	p, _ := o.Get("pattern")
	r.Pattern, _ = p.(string)
	if rep, _ := o.Get("representation"); rep != nil {
		s, _ := rep.(string)
		r.Representation = &s
	}
	k, _ := o.Get("keys")
	r.Keys = strs(k)
	m, _ := o.Get("measures")
	r.Measures = strs(m)
	if lv, _ := o.Get("labels"); lv != nil {
		lo, _ := lv.(jsjson.Object)
		for _, role := range lo {
			set := LabelSet{Role: role.Key}
			inner, _ := role.Value.(jsjson.Object)
			for _, x := range inner {
				set.Labels = append(set.Labels, NamedCell{x.Key, toCell(x.Value)})
			}
			r.Labels = append(r.Labels, set)
		}
	}
	rv, _ := o.Get("rows")
	arr, _ := rv.([]any)
	for _, x := range arr {
		ro, _ := x.(jsjson.Object)
		var row RoleRow
		kv, _ := ro.Get("key")
		ka, _ := kv.([]any)
		for _, c := range ka {
			row.Key = append(row.Key, toCell(c))
		}
		vv, _ := ro.Get("values")
		vo, _ := vv.(jsjson.Object)
		for _, c := range vo {
			row.Values = append(row.Values, NamedCell{c.Key, toCell(c.Value)})
		}
		r.Rows = append(r.Rows, row)
	}
	return r
}

var lineBar = map[string]bool{"line": true, "bar": true}

// samePatternFamily: line and bar share roles, so either answers the other.
func samePatternFamily(a, b string) bool { return a == b || (lineBar[a] && lineBar[b]) }

type layout struct {
	representation *string
	keys, measures []string
	labels         []string
}

func ptr(s string) *string { return &s }

func roleLayout(spec panels.Spec) (layout, error) {
	used := func(r string) bool { return spec.Display.Col(r) != "" }
	opt := func(cond bool, s ...string) []string {
		if cond {
			return s
		}
		return nil
	}
	switch spec.Display.Type {
	case "number":
		if used("value") {
			return layout{ptr("value"), []string{}, []string{"value"}, opt(used("label"), "label")}, nil
		}
		return layout{ptr("rate"), []string{}, []string{"numerator", "denominator"}, opt(used("label"), "label")}, nil
	case "line", "bar":
		return layout{nil, append([]string{"x"}, opt(used("series"), "series")...), []string{"numerator", "denominator"}, nil}, nil
	case "funnel":
		return layout{nil, append(opt(used("cohort"), "cohort"), "step_no"), []string{"reached", "eligible", "unknown"}, []string{"step_name"}}, nil
	case "cohort":
		return layout{nil, append([]string{"cohort", "period"}, opt(used("series"), "series")...), append([]string{"numerator", "denominator"}, opt(used("deleted_n"), "deleted_n")...), nil}, nil
	}
	return layout{}, fmt.Errorf("table panels are not graded")
}

func timeKey(v Cell, grain, role string) (Cell, error) {
	if v == nil {
		return nil, nil
	}
	ts, err := civiltime.Normalize(*v)
	if err != nil {
		return nil, &NormalizeFailure{"key_unparseable", role + " value is not a date: " + jsjson.QuoteString(*v)}
	}
	switch grain {
	case "week":
		w, _ := civiltime.WeekStart(ts)
		return ptr(w[:10]), nil
	case "month":
		return ptr(ts[:7]), nil
	}
	return ptr(ts[:10]), nil
}

var intKeyRE = regexp.MustCompile(`^-?\d+(\.0+)?$`)

// intKey writes integer-looking keys as plain integers ("3.0" → "3"); other values as they are.
func intKey(v Cell) Cell {
	if v == nil || !intKeyRE.MatchString(*v) {
		return v
	}
	n, _ := new(big.Int).SetString(strings.Split(*v, ".")[0], 10)
	return ptr(n.String())
}

// utf16Less compares strings like JavaScript's < (UTF-16 code units).
func utf16Less(a, b string) bool {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

// NormalizeResult puts a panel result into role rows; xGrain ("" none) buckets the time key.
func NormalizeResult(spec panels.Spec, columns []panels.Column, rows []panels.Row, xGrain string) (RoleRows, error) {
	l, err := roleLayout(spec)
	if err != nil {
		return RoleRows{}, err
	}
	idx := map[string]int{}
	for i, c := range columns {
		idx[c.Name] = i
	}
	col := func(role string) (int, error) {
		name := spec.Display.Col(role)
		i, ok := idx[name]
		if name == "" || !ok {
			return 0, fmt.Errorf("result has no column for role %s", role)
		}
		return i, nil
	}
	timeRole := "x"
	if spec.Display.Type == "cohort" {
		timeRole = "cohort"
	}
	out := RoleRows{Pattern: spec.Display.Type, Representation: l.representation, Keys: l.keys, Measures: l.measures}
	for _, lr := range l.labels {
		out.Labels = append(out.Labels, LabelSet{Role: lr})
	}
	seen := map[string]bool{}
	for _, row := range rows {
		key := make([]Cell, len(l.keys))
		for i, role := range l.keys {
			ci, err := col(role)
			if err != nil {
				return RoleRows{}, err
			}
			v := toCell(row[ci])
			if xGrain != "" && role == timeRole {
				if v, err = timeKey(v, xGrain, role); err != nil {
					return RoleRows{}, err
				}
			}
			if role == "step_no" || role == "period" {
				v = intKey(v)
			}
			key[i] = v
		}
		k := keyJSON(key)
		if seen[k] {
			return RoleRows{}, &NormalizeFailure{"normalization_collision", "rows share the key " + k + " after normalization"}
		}
		seen[k] = true
		rr := RoleRow{Key: key}
		for _, m := range l.measures {
			ci, err := col(m)
			if err != nil {
				return RoleRows{}, err
			}
			rr.Values = append(rr.Values, NamedCell{m, toCell(row[ci])})
		}
		out.Rows = append(out.Rows, rr)
		for li, lr := range l.labels {
			ci, err := col(lr)
			if err != nil {
				return RoleRows{}, err
			}
			out.Labels[li].Labels = append(out.Labels[li].Labels, NamedCell{k, toCell(row[ci])})
		}
	}
	if out.Rows == nil {
		out.Rows = []RoleRow{}
	}
	sort.SliceStable(out.Rows, func(i, j int) bool { return utf16Less(keyJSON(out.Rows[i].Key), keyJSON(out.Rows[j].Key)) })
	return out, nil
}

// cohortAsLineBar reshapes a single-period cohort answer into line/bar rows keyed by cohort; ok false when it can't.
func cohortAsLineBar(r RoleRows) (RoleRows, bool) {
	if r.Pattern != "cohort" {
		return RoleRows{}, false
	}
	pi := indexOf(r.Keys, "period")
	periods := map[string]bool{}
	for _, row := range r.Rows {
		periods[jsjson.MustStringify(cellJS(row.Key[pi]))] = true
	}
	if len(periods) > 1 {
		return RoleRows{}, false
	}
	keys := []string{"x"}
	for _, k := range r.Keys {
		if k == "series" {
			keys = append(keys, k)
		}
	}
	// deleted_n is a supplementary count that line/bar has no role for
	rows := make([]RoleRow, len(r.Rows))
	for i, row := range r.Rows {
		var key []Cell
		for j, c := range row.Key {
			if j != pi {
				key = append(key, c)
			}
		}
		rows[i] = RoleRow{Key: key, Values: []NamedCell{{"numerator", row.value("numerator")}, {"denominator", row.value("denominator")}}}
	}
	return RoleRows{Pattern: "bar", Representation: r.Representation, Keys: keys, Measures: []string{"numerator", "denominator"}, Rows: rows}, true
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

var spacesRE = regexp.MustCompile(`[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`)

func labelNorm(c Cell) Cell {
	if c == nil {
		return nil
	}
	return ptr(strings.ToLower(jsstr.Trim(spacesRE.ReplaceAllString(*c, " "))))
}

// Diff is one differing measure value.
type Diff struct {
	Key              []Cell
	Role             string
	Expected, Actual Cell
}

// LabelReview is a label that differs from the fixture (shown to a person, never a failure).
type LabelReview struct {
	Role, Key        string
	Expected, Actual Cell
}

// Comparison is the result of comparing actual role rows with a fixture.
type Comparison struct {
	Codes       []string
	Diffs       []Diff
	LabelReview []LabelReview
}

func sameCell(a, b Cell) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// CompareRoleRows compares actual with the fixture exactly. Labels never fail; mismatches are flagged for review.
func CompareRoleRows(expected, actual RoleRows, aliases LabelAliases) Comparison {
	var cmp Comparison
	if !sameCell(expected.Representation, actual.Representation) {
		cmp.Codes = []string{"representation"}
		return cmp
	}
	if strings.Join(expected.Keys, ",") != strings.Join(actual.Keys, ",") || strings.Join(expected.Measures, ",") != strings.Join(actual.Measures, ",") {
		cmp.Codes = []string{"roles_mismatch"}
		return cmp
	}
	got := map[string]RoleRow{}
	for _, r := range actual.Rows {
		got[keyJSON(r.Key)] = r
	}
	want := map[string]bool{}
	missing := false
	for _, r := range expected.Rows {
		k := keyJSON(r.Key)
		want[k] = true
		if _, ok := got[k]; !ok {
			missing = true
		}
	}
	if missing {
		cmp.Codes = append(cmp.Codes, "rows_missing")
	}
	for k := range got {
		if !want[k] {
			cmp.Codes = append(cmp.Codes, "rows_extra")
			break
		}
	}
	differs := false
	for _, w := range expected.Rows {
		g, ok := got[keyJSON(w.Key)]
		if !ok {
			continue
		}
		for _, m := range expected.Measures {
			if verify.SameValue(w.value(m), g.value(m)) {
				continue
			}
			differs = true
			if len(cmp.Diffs) < 5 {
				cmp.Diffs = append(cmp.Diffs, Diff{w.Key, m, w.value(m), g.value(m)})
			}
		}
	}
	if differs {
		cmp.Codes = append(cmp.Codes, "value_diff")
	}
	for _, set := range expected.Labels {
		// a number card's label is free wording; it is only checked when the case lists accepted labels
		if set.Role == "label" && len(aliases.Label) == 0 {
			continue
		}
		act, _ := actual.labels(set.Role)
		for _, l := range set.Labels {
			if _, ok := got[l.Name]; !ok {
				continue
			}
			var a Cell
			if act != nil {
				a, _ = act.get(l.Name)
			}
			accepted := []Cell{l.Cell}
			if set.Role == "label" {
				for _, x := range aliases.Label {
					accepted = append(accepted, ptr(x))
				}
			} else if set.Role == "step_name" {
				parsed, _ := jsjson.Parse(l.Name)
				arr, _ := parsed.([]any)
				step := "undefined"
				if len(arr) > 0 {
					step = jsString(arr[len(arr)-1])
				}
				for _, x := range aliases.stepNames(step) {
					accepted = append(accepted, ptr(x))
				}
			}
			ok := false
			for _, c := range accepted {
				if sameCell(labelNorm(c), labelNorm(a)) {
					ok = true
				}
			}
			if !ok {
				cmp.LabelReview = append(cmp.LabelReview, LabelReview{set.Role, l.Name, l.Cell, a})
			}
		}
	}
	return cmp
}
