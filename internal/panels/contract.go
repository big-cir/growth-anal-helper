package panels

import (
	"fmt"
	"growth-lab/internal/jsstr"
	"math"
	"sort"
	"strings"

	"growth-lab/internal/jsjson"
)

// PatternContractVersion is bumped when contract, invariant or headline rules change.
const PatternContractVersion = 1

// Column is a result column.
type Column struct {
	Name   string
	Table  *string
	Column *string
}

// Row is a result row.
type Row = []Value

// Violation is an invariant violation. Index is the row position (nil for whole-result violations).
type Violation struct {
	Row     string
	Problem string
	Index   *int
	Rule    string
	Column  *string
}

// ContractError is a broken result contract.
type ContractError struct{ Msg string }

func (e *ContractError) Error() string { return e.Msg }

// ColumnIndex maps names to positions; duplicate names are an error.
func ColumnIndex(columns []Column) (map[string]int, error) {
	m := map[string]int{}
	for i, c := range columns {
		if _, ok := m[c.Name]; ok {
			return nil, &ContractError{"duplicate result column name: " + c.Name}
		}
		m[c.Name] = i
	}
	return m, nil
}

// used returns the role → column pairs that are set, in role order.
func used(spec Spec) []Member {
	var out []Member
	for _, m := range spec.Display.Columns {
		if m.Column != nil && *m.Column != "" {
			out = append(out, m)
		}
	}
	return out
}

func usedCol(u []Member, role string) string {
	for _, m := range u {
		if m.Role == role {
			return *m.Column
		}
	}
	return ""
}

// CheckContract checks that the result has the columns the pattern needs.
func CheckContract(spec Spec, columns []Column) error {
	idx, err := ColumnIndex(columns)
	if err != nil {
		return err
	}
	var need []string
	for _, m := range used(spec) {
		need = append(need, *m.Column)
	}
	need = append(append(need, spec.Display.Extra...), spec.Display.Key...)
	var missing []string
	for _, c := range need {
		if _, ok := idx[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		names := make([]string, len(columns))
		for i, c := range columns {
			names[i] = c.Name
		}
		return &ContractError{fmt.Sprintf("result is missing columns the %s pattern needs: %s (result columns: %s)", spec.Display.Type, strings.Join(missing, ", "), strings.Join(names, ", "))}
	}
	return nil
}

var keyRoles = map[string][]string{"line": {"x", "series"}, "bar": {"x", "series"}, "funnel": {"cohort", "step_no"}, "cohort": {"cohort", "series", "period"}, "number": {}}

var countRoles = map[string][]string{"number": {"numerator", "denominator"}, "line": {"numerator", "denominator"}, "bar": {"numerator", "denominator"}, "funnel": {"reached", "eligible", "unknown"}, "cohort": {"numerator", "denominator", "deleted_n"}, "table": {}}

// cellAt reads a cell; a missing column reads as undefined.
func cellAt(row Row, idx map[string]int, col string) Value {
	i, ok := idx[col]
	if !ok || i >= len(row) {
		return jsjson.Undefined
	}
	return row[i]
}

func typeOf(v Value) string {
	switch v.(type) {
	case nil:
		return "object"
	case float64:
		return "number"
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	return "undefined"
}

type entry struct {
	row Row
	i   int
}

// orderedGroups is a Map of string → entries keeping insertion order.
type orderedGroups struct {
	keys []string
	m    map[string][]entry
}

func (g *orderedGroups) add(k string, e entry) {
	if g.m == nil {
		g.m = map[string][]entry{}
	}
	if _, ok := g.m[k]; !ok {
		g.keys = append(g.keys, k)
	}
	g.m[k] = append(g.m[k], e)
}

// CheckInvariants checks the result rows against the pattern rules.
func CheckInvariants(spec Spec, columns []Column, rows []Row) ([]Violation, error) {
	idx, err := ColumnIndex(columns)
	if err != nil {
		return nil, err
	}
	t := spec.Display.Type
	u := used(spec)
	get := func(row Row, role string) Value { return cellAt(row, idx, usedCol(u, role)) }
	var keyCols []string
	if t == "table" {
		keyCols = spec.Display.Key
	} else {
		for _, r := range keyRoles[t] {
			if c := usedCol(u, r); c != "" {
				keyCols = append(keyCols, c)
			}
		}
	}
	rowKey := func(row Row, i int) string {
		if len(keyCols) == 0 {
			return fmt.Sprintf("row %d", i+1)
		}
		parts := make([]string, len(keyCols))
		for k, c := range keyCols {
			parts[k] = c + "=" + show(cellAt(row, idx, c))
		}
		return strings.Join(parts, ", ")
	}
	out := []Violation{}
	add := func(row Row, i int, rule, column, problem string) {
		ii := i
		var col *string
		if column != "" {
			c := column
			col = &c
		}
		out = append(out, Violation{Row: rowKey(row, i), Problem: problem, Index: &ii, Rule: rule, Column: col})
	}
	whole := func(rule, column, problem string) {
		var col *string
		if column != "" {
			c := column
			col = &c
		}
		out = append(out, Violation{Row: "-", Problem: problem, Rule: rule, Column: col})
	}

	if len(rows) == 0 {
		return []Violation{{Row: "-", Problem: "result has no rows", Rule: "rows_nonempty"}}, nil
	}
	if t == "number" && len(rows) != 1 {
		return []Violation{{Row: "-", Problem: fmt.Sprintf("the number pattern needs exactly 1 row (got %d)", len(rows)), Rule: "number_one_row"}}, nil
	}

	var countCols []string
	for _, r := range countRoles[t] {
		if c := usedCol(u, r); c != "" {
			countCols = append(countCols, c)
		}
	}
	countCols = append(countCols, spec.Display.Extra...)
	for i, row := range rows {
		for _, c := range countCols {
			v := cellAt(row, idx, c)
			if !isCount(v) {
				add(row, i, "count_nonneg_int", c, fmt.Sprintf("%s must be an integer ≥ 0 (%s)", c, show(v)))
			}
		}
		num, den := "numerator", "denominator"
		if t == "funnel" {
			num, den = "reached", "eligible"
		}
		if usedCol(u, num) != "" && usedCol(u, den) != "" {
			a, b := get(row, num), get(row, den)
			if isCount(a) && isCount(b) && a.(float64) > b.(float64) {
				add(row, i, "numerator_le_denominator", usedCol(u, num), fmt.Sprintf("%s(%s) > %s(%s)", usedCol(u, num), show(a), usedCol(u, den), show(b)))
			}
		}
		if t == "number" && usedCol(u, "value") != "" {
			if v := get(row, "value"); !isNum(v) {
				add(row, i, "value_number", usedCol(u, "value"), fmt.Sprintf("value must be a number (%s)", show(v)))
			}
		}
	}

	if len(keyCols) > 0 {
		seen := map[string]int{}
		for i, row := range rows {
			vals := make([]any, len(keyCols))
			for k, c := range keyCols {
				vals[k] = cellAt(row, idx, c)
			}
			key := jsonOf(vals)
			if first, ok := seen[key]; ok {
				add(row, i, "unique_row_key", "", fmt.Sprintf("duplicate row key (same as row %d)", first+1))
			} else {
				seen[key] = i
			}
		}
	}

	if t == "line" || t == "bar" {
		hasNull := false
		types := map[string]bool{}
		for _, r := range rows {
			x := get(r, "x")
			if x == nil {
				hasNull = true
			}
			types[typeOf(x)] = true
		}
		if hasNull {
			whole("x_not_null", usedCol(u, "x"), "x has NULL")
		} else if len(types) > 1 {
			whole("x_sortable", usedCol(u, "x"), "x mixes numbers and strings and cannot be sorted")
		}
		if usedCol(u, "series") != "" {
			series := map[string]bool{}
			for _, r := range rows {
				series[show(get(r, "series"))] = true
			}
			if len(series) > 6 {
				whole("series_max_6", usedCol(u, "series"), fmt.Sprintf("%d series (6 or fewer)", len(series)))
			}
		}
	}

	if t == "funnel" {
		var groups orderedGroups
		for i, row := range rows {
			g := ""
			if usedCol(u, "cohort") != "" {
				g = show(get(row, "cohort"))
			}
			groups.add(g, entry{row, i})
		}
		for _, k := range groups.keys {
			list := append([]entry(nil), groups.m[k]...)
			var bad *entry
			for j := range list {
				if !isInteger(get(list[j].row, "step_no")) {
					bad = &list[j]
					break
				}
			}
			if bad != nil {
				add(bad.row, bad.i, "step_no_int", usedCol(u, "step_no"), "step_no must be an integer")
				continue
			}
			sort.SliceStable(list, func(a, b int) bool {
				return get(list[a].row, "step_no").(float64) < get(list[b].row, "step_no").(float64)
			})
			for n, e := range list {
				if get(e.row, "step_no").(float64) != float64(n+1) {
					add(e.row, e.i, "steps_consecutive", usedCol(u, "step_no"), fmt.Sprintf("step numbers are not consecutive from 1 (expected %d)", n+1))
				}
				if n == 0 {
					continue
				}
				prev := list[n-1].row
				r, pr, el, un := get(e.row, "reached"), get(prev, "reached"), get(e.row, "eligible"), get(e.row, "unknown")
				if !isCount(r) || !isCount(pr) || !isCount(el) || !isCount(un) {
					continue
				}
				if r.(float64) > pr.(float64) {
					add(e.row, e.i, "reached_nonincreasing", usedCol(u, "reached"), fmt.Sprintf("reached (%s) is more than the previous step's reached (%s)", show(r), show(pr)))
				}
				if el.(float64) != pr.(float64)-un.(float64) {
					add(e.row, e.i, "eligible_is_prev_minus_unknown", usedCol(u, "eligible"), fmt.Sprintf("eligible (%s) ≠ previous step's reached (%s) − unknown (%s)", show(el), show(pr), show(un)))
				}
			}
		}
	}

	if t == "cohort" {
		var groups orderedGroups
		for i, row := range rows {
			p := get(row, "period")
			if !isInteger(p) || p.(float64) < 0 {
				add(row, i, "period_nonneg_int", usedCol(u, "period"), fmt.Sprintf("period must be an integer ≥ 0 (%s)", show(p)))
			}
			var series Value
			if usedCol(u, "series") != "" {
				series = get(row, "series")
			}
			groups.add(jsonOf([]any{get(row, "cohort"), series}), entry{row, i})
		}
		for _, k := range groups.keys {
			var ok []entry
			for _, e := range groups.m[k] {
				if isInteger(get(e.row, "period")) && isCount(get(e.row, "denominator")) {
					ok = append(ok, e)
				}
			}
			sort.SliceStable(ok, func(a, b int) bool {
				return get(ok[a].row, "period").(float64) < get(ok[b].row, "period").(float64)
			})
			for n := 1; n < len(ok); n++ {
				d, pd := get(ok[n].row, "denominator").(float64), get(ok[n-1].row, "denominator").(float64)
				if d > pd {
					add(ok[n].row, ok[n].i, "denominator_nonincreasing", usedCol(u, "denominator"), fmt.Sprintf("denominator grew as period grew (%s → %s, breaks the observability condition)", numStr(pd), numStr(d)))
				}
			}
		}
	}
	return out, nil
}

// FormatViolations lists up to max violations.
func FormatViolations(vs []Violation, max int) string {
	n := len(vs)
	if n > max {
		n = max
	}
	lines := make([]string, n)
	for i := 0; i < n; i++ {
		lines[i] = "- " + vs[i].Row + ": " + vs[i].Problem
	}
	head := strings.Join(lines, "\n")
	if len(vs) > max {
		return fmt.Sprintf("%s\n- … and %d more", head, len(vs)-max)
	}
	return head
}

// EffectiveCaveats prepends engine caveats: off-dictionary, then observational comparison (2+ series).
func EffectiveCaveats(spec Spec, columns []Column, rows []Row, lang string) ([]string, error) {
	u := used(spec)
	t := spec.Display.Type
	comparison := false
	if (t == "line" || t == "bar" || t == "cohort") && usedCol(u, "series") != "" {
		idx, err := ColumnIndex(columns)
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, r := range rows {
			set[show(cellAt(r, idx, usedCol(u, "series")))] = true
		}
		comparison = len(set) >= 2
	}
	var auto []string
	offdict, compare := OffdictCaveat(lang), ComparisonCaveat(lang)
	anyHas := func(s string) bool {
		for _, c := range spec.Caveats {
			if strings.Contains(c, s) {
				return true
			}
		}
		return false
	}
	if spec.Metric == nil && !anyHas(offdict) {
		auto = append(auto, offdict)
	}
	if comparison && !anyHas(compare) {
		auto = append(auto, compare)
	}
	return append(append([]string{}, auto...), spec.Caveats...), nil
}

// HeadlineValue is the headline number of a panel.
type HeadlineValue struct {
	Value       Value // float64 (NaN for a zero denominator) or the raw cell
	Numerator   Value
	Denominator Value
	LowN        bool
	Label       string
}

func rate(num, den Value, label string) *HeadlineValue {
	var v float64
	if d, ok := den.(float64); ok && d == 0 {
		v = math.NaN()
	} else {
		v = toNumber(num) / toNumber(den)
	}
	return &HeadlineValue{Value: v, Numerator: num, Denominator: den, LowN: lessThan(den, 30), Label: label}
}

// lessThan is `v < n` with JavaScript coercion.
func lessThan(v Value, n float64) bool {
	if v == jsjson.Undefined {
		return false
	}
	f := toNumber(v)
	return !math.IsNaN(f) && f < n
}

// ComputeHeadline picks the headline number: the pattern's default rule unless display.headline is set. Denominator under 30 is lowN.
func ComputeHeadline(spec Spec, columns []Column, rows []Row) (*HeadlineValue, error) {
	t := spec.Display.Type
	if t == "table" || t == "cohort" || len(rows) == 0 {
		return nil, nil
	}
	idx, err := ColumnIndex(columns)
	if err != nil {
		return nil, err
	}
	u := used(spec)
	get := func(row Row, role string) Value { return cellAt(row, idx, usedCol(u, role)) }
	h := spec.Display.Headline

	if t == "number" {
		row := rows[0]
		label := ""
		if usedCol(u, "label") != "" {
			label = show(get(row, "label"))
		}
		if usedCol(u, "value") != "" {
			return &HeadlineValue{Value: get(row, "value"), Numerator: nil, Denominator: nil, LowN: false, Label: label}, nil
		}
		return rate(get(row, "numerator"), get(row, "denominator"), label), nil
	}

	if t == "line" || t == "bar" {
		seriesUsed := usedCol(u, "series") != ""
		order := []string{""}
		if seriesUsed {
			order = nil
			for _, r := range rows {
				s := show(get(r, "series"))
				if !contains(order, s) {
					order = append(order, s)
				}
			}
		}
		want := order[0]
		if h != nil && h.HasSeries {
			want = "NULL"
			if h.Series != nil {
				want = *h.Series
			}
		}
		if seriesUsed && !contains(order, want) {
			return nil, &ContractError{"headline series not in the result: " + want}
		}
		var in []Row
		for _, r := range rows {
			if !seriesUsed || show(get(r, "series")) == want {
				in = append(in, r)
			}
		}
		var row Row
		if h != nil && h.X != nil {
			found := false
			for _, r := range in {
				if show(get(r, "x")) == *h.X {
					row, found = r, true
					break
				}
			}
			if !found {
				return nil, &ContractError{"headline x not in the result: " + *h.X}
			}
		} else {
			sorted := append([]Row(nil), in...)
			sort.SliceStable(sorted, func(a, b int) bool { return CompareValues(get(sorted[a], "x"), get(sorted[b], "x")) < 0 })
			if len(sorted) == 0 {
				return nil, &ContractError{"no rows in the headline series"}
			}
			row = sorted[len(sorted)-1]
		}
		label := show(get(row, "x"))
		if seriesUsed {
			label += " · " + want
		}
		return rate(get(row, "numerator"), get(row, "denominator"), label), nil
	}

	// funnel: last step as a share of the first (the 'ALL' row when there are cohorts)
	group := rows
	if usedCol(u, "cohort") != "" {
		want := "ALL"
		if h != nil && h.X != nil {
			want = *h.X
		}
		group = nil
		for _, r := range rows {
			if show(get(r, "cohort")) == want {
				group = append(group, r)
			}
		}
		if len(group) == 0 {
			if h != nil && h.X != nil {
				return nil, &ContractError{"headline cohort not in the result: " + *h.X}
			}
			return nil, nil
		}
	}
	sorted := append([]Row(nil), group...)
	sort.SliceStable(sorted, func(a, b int) bool { return toNumber(get(sorted[a], "step_no")) < toNumber(get(sorted[b], "step_no")) })
	first, last := sorted[0], sorted[len(sorted)-1]
	return rate(get(last, "reached"), get(first, "reached"), show(get(last, "step_name"))+" / "+show(get(first, "step_name"))), nil
}

// CompareValues orders NULL < number < string.
func CompareValues(a, b Value) float64 {
	rank := func(v Value) float64 {
		switch v.(type) {
		case nil:
			return 0
		case float64:
			return 1
		}
		return 2
	}
	if rank(a) != rank(b) {
		return rank(a) - rank(b)
	}
	if x, ok := a.(float64); ok {
		if y, ok := b.(float64); ok {
			return x - y
		}
	}
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			switch {
			case jsstr.Less(x, y):
				return -1
			case jsstr.Less(y, x):
				return 1
			}
		}
	}
	return 0
}

// JS returns the headline in its stored JSON form (NaN becomes null).
func (h *HeadlineValue) JS() any {
	if h == nil {
		return nil
	}
	return jsjson.Object{{Key: "value", Value: h.Value}, {Key: "numerator", Value: h.Numerator}, {Key: "denominator", Value: h.Denominator}, {Key: "lowN", Value: h.LowN}, {Key: "label", Value: h.Label}}
}

// JS returns a violation in its stored JSON form.
func (v Violation) JS() jsjson.Object {
	var idx any
	if v.Index != nil {
		idx = *v.Index
	}
	return jsjson.Object{{Key: "row", Value: v.Row}, {Key: "problem", Value: v.Problem}, {Key: "index", Value: idx}, {Key: "rule", Value: v.Rule}, {Key: "column", Value: strPtr(v.Column)}}
}
