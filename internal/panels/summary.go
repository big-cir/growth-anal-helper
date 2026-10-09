package panels

import (
	"fmt"
	"growth-lab/internal/jsstr"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"

	"growth-lab/internal/jsjson"
)

// Claim operations.
var claimOps = []string{"value", "rate", "diff", "ratio", "sum"}

// ClaimRef points at one result cell by row key and column.
type ClaimRef struct {
	Row    [][2]string // column, value
	Column string
}

// Claim is a number the engine recomputes and writes into the text.
type Claim struct {
	ID      string
	Op      string
	Refs    []ClaimRef
	Display string
}

// SummaryDraft is the agent's summary reply.
type SummaryDraft struct {
	Prose  string
	Claims []Claim
}

// SummaryError is a summary problem.
type SummaryError struct{ Msg string }

func (e *SummaryError) Error() string { return e.Msg }

func sumErr(format string, a ...any) { throw(&SummaryError{fmt.Sprintf(format, a...)}) }

func refCountOK(op string, n int) bool {
	switch op {
	case "value":
		return n == 1
	case "rate":
		return n == 2
	case "diff", "ratio":
		return n == 4
	}
	return n >= 1
}

// ClaimUnits are the units the engine writes, by language.
func ClaimUnits(lang string) map[string]string {
	if lang == "ko" {
		return map[string]string{"value": "", "sum": "", "rate": "%", "diff": "%p", "ratio": "배"}
	}
	return map[string]string{"value": "", "sum": "", "rate": "%", "diff": "pp", "ratio": "x"}
}

// display units accepted in either language
var displayUnits = map[string][]string{"value": {""}, "sum": {""}, "rate": {"%"}, "diff": {"%p", "pp"}, "ratio": {"배", "x"}}

var claimIDRE = regexp.MustCompile(`^c[0-9]{1,2}$`)

// ParseSummary validates the agent's summary reply (decoded by jsjson.Parse).
func ParseSummary(raw any) (draft SummaryDraft, err error) {
	defer catch(&err)
	o, ok := raw.(jsjson.Object)
	if !ok {
		sumErr("reply is not an object")
	}
	pv, _ := o.Get("prose")
	prose, ok := pv.(string)
	if !ok || jsstr.Trim(prose) == "" {
		sumErr("prose is empty")
	}
	if jsstr.CPLen(prose) > 800 {
		sumErr("prose must be 800 characters or fewer")
	}
	cv, _ := o.Get("claims")
	arr, ok := cv.([]any)
	if !ok || len(arr) > 12 {
		sumErr("claims must be an array of 12 or fewer")
	}
	ids := map[string]bool{}
	draft = SummaryDraft{Prose: prose, Claims: []Claim{}}
	for i, c := range arr {
		p := fmt.Sprintf("claims[%d]", i)
		co, ok := c.(jsjson.Object)
		if !ok {
			sumErr("%s: not an object", p)
		}
		idv, _ := co.Get("id")
		id, ok := idv.(string)
		if !ok || !claimIDRE.MatchString(id) {
			sumErr("%s.id: must look like c1", p)
		}
		if ids[id] {
			sumErr("%s.id: duplicate %s", p, id)
		}
		ids[id] = true
		opv, _ := co.Get("op")
		op, _ := opv.(string)
		if !contains(claimOps, op) {
			sumErr("%s.op: %s", p, strings.Join(claimOps, "|"))
		}
		rv, _ := co.Get("refs")
		refsRaw, ok := rv.([]any)
		if !ok || !refCountOK(op, len(refsRaw)) {
			sumErr("%s.refs: wrong count for %s", p, op)
		}
		refs := make([]ClaimRef, len(refsRaw))
		for j, r := range refsRaw {
			ro, ok := r.(jsjson.Object)
			colv, _ := ro.Get("column")
			col, colOK := colv.(string)
			rowv, _ := ro.Get("row")
			rowArr, rowOK := rowv.([]any)
			if !ok || !colOK || !rowOK {
				sumErr("%s.refs[%d]: must be { row, column }", p, j)
			}
			keys := make([][2]string, len(rowArr))
			for k, kv := range rowArr {
				ko, ok := kv.(jsjson.Object)
				kc, _ := ko.Get("column")
				kval, _ := ko.Get("value")
				kcs, cOK := kc.(string)
				kvs, vOK := kval.(string)
				if !ok || !cOK || !vOK {
					sumErr("%s.refs[%d].row: must be a list of { column, value }", p, j)
				}
				keys[k] = [2]string{kcs, kvs}
			}
			refs[j] = ClaimRef{Row: keys, Column: col}
		}
		dv, _ := co.Get("display")
		disp, ok := dv.(string)
		if !ok || jsstr.Trim(disp) == "" {
			sumErr("%s.display: empty", p)
		}
		draft.Claims = append(draft.Claims, Claim{ID: id, Op: op, Refs: refs, Display: disp})
	}
	return draft, nil
}

func refCell(ref ClaimRef, columns []Column, rows []Row) float64 {
	idx := map[string]int{}
	for i, c := range columns {
		idx[c.Name] = i
	}
	for _, k := range ref.Row {
		if _, ok := idx[k[0]]; !ok {
			sumErr("row key column not in result: %s", k[0])
		}
	}
	if _, ok := idx[ref.Column]; !ok {
		sumErr("column not in result: %s", ref.Column)
	}
	var hits []Row
	for _, r := range rows {
		match := true
		for _, k := range ref.Row {
			if show(cellAt(r, idx, k[0])) != k[1] {
				match = false
				break
			}
		}
		if match {
			hits = append(hits, r)
		}
	}
	parts := make([]string, len(ref.Row))
	for i, k := range ref.Row {
		parts[i] = k[0] + "=" + k[1]
	}
	key := strings.Join(parts, ", ")
	if key == "" {
		key = "(no key)"
	}
	if len(hits) != 1 {
		sumErr("row key %s matches %d rows (must be exactly 1)", key, len(hits))
	}
	v, ok := cellAt(hits[0], idx, ref.Column).(float64)
	if !ok {
		sumErr("%s at %s is not a number", ref.Column, key)
	}
	return v
}

func compute(c Claim, columns []Column, rows []Row) float64 {
	v := make([]float64, len(c.Refs))
	for i, r := range c.Refs {
		v[i] = refCell(r, columns, rows)
	}
	rate := func(n, d float64) float64 {
		if d == 0 {
			sumErr("%s: denominator is 0", c.ID)
		}
		return n / d
	}
	switch c.Op {
	case "value":
		return v[0]
	case "sum":
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s
	case "rate":
		return rate(v[0], v[1]) * 100
	case "diff":
		return (rate(v[0], v[1]) - rate(v[2], v[3])) * 100
	}
	b := rate(v[2], v[3])
	if b == 0 {
		sumErr("%s: the rate compared against is 0", c.ID)
	}
	return rate(v[0], v[1]) / b
}

func round(x float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return jsRound(x*p) / p
}

// FormatClaim is the value the engine inserts into the text.
func FormatClaim(op string, x float64, lang string) string {
	if (op == "value" || op == "sum") && isInteger(x) {
		return localeInt(x)
	}
	return toFixed1(round(x, 1)) + ClaimUnits(lang)[op]
}

var displayRE = regexp.MustCompile(`^([+\-−]?)` + jsstr.Space + `*([0-9][0-9,]*(?:\.[0-9]+)?)` + jsstr.Space + `*(%p|pp|%|배|x)?$`)

// matchDisplay checks display against the computed value under the rounding rule; returns the value to insert.
func matchDisplay(c Claim, x float64, lang string) (string, bool) {
	m := displayRE.FindStringSubmatch(jsstr.Trim(c.Display))
	if m == nil {
		return "", false
	}
	if !contains(displayUnits[c.Op], m[3]) {
		return "", false
	}
	decimals := 0
	if _, frac, ok := strings.Cut(m[2], "."); ok {
		decimals = len(frac)
	}
	shown, _ := strconv.ParseFloat(strings.ReplaceAll(m[2], ",", ""), 64)
	if m[1] != "" && m[1] != "+" {
		shown = -shown
	}
	if (c.Op == "value" || c.Op == "sum") && isInteger(x) {
		if shown == x {
			return FormatClaim(c.Op, x, lang), true
		}
		return "", false
	}
	if decimals > 1 {
		return "", false
	}
	if round(x, decimals) == shown {
		return FormatClaim(c.Op, x, lang), true
	}
	// difference written without a sign (the text says "lower")
	if m[1] == "" && c.Op == "diff" && round(math.Abs(x), decimals) == shown {
		return FormatClaim(c.Op, math.Abs(x), lang), true
	}
	return "", false
}

var (
	labelTokenRE = regexp.MustCompile(`[0-9][0-9.,:\-/]*(?:주차|개월|시간|번째|단계|일|주|개|년|월|분|초|명|건|회|차)?`)
	rateUnitRE   = regexp.MustCompile(`(?i)^` + jsstr.Space + `*(?:%|％|퍼센트|배|x\b|pp\b|percent)`)
	asciiDigitRE = regexp.MustCompile(`[0-9]`)
	strayRE      = regexp.MustCompile(`\p{Nd}[\p{Nd}.,:\-/]*`)
	placeholder  = regexp.MustCompile(`\{(c[0-9]{1,2})\}`)
)

// AllowedLabels are the number-bearing labels allowed in the text: row key values of the result, numbers in the definition.
func AllowedLabels(spec Spec, columns []Column, rows []Row) []string {
	d := spec.Display
	idx := map[string]int{}
	for i, c := range columns {
		idx[c.Name] = i
	}
	var keyCols []string
	for _, c := range append([]string{d.Col("x"), d.Col("series"), d.Col("cohort"), d.Col("step_name"), d.Col("label")}, d.Key...) {
		if _, ok := idx[c]; c != "" && ok {
			keyCols = append(keyCols, c)
		}
	}
	var out []string
	addOut := func(s string) {
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	for _, r := range rows {
		for _, c := range keyCols {
			switch v := cellAt(r, idx, c).(type) {
			case string:
				if asciiDigitRE.MatchString(v) {
					addOut(v)
				}
			case float64:
				if math.Abs(v) >= 1000 {
					addOut(numStr(v))
				}
			}
		}
		if p := d.Col("period"); d.Type == "cohort" && p != "" {
			if _, ok := idx[p]; ok {
				if pv, ok := cellAt(r, idx, p).(float64); ok {
					s := numStr(pv)
					for _, l := range []string{"W" + s, s + "주차", s + "주 차", "week " + s, "Week " + s} {
						addOut(l)
					}
				}
			}
		}
	}
	texts := []string{spec.Title, spec.Question}
	for _, p := range spec.Definition {
		texts = append(texts, p[0], p[1])
	}
	for _, a := range spec.Answers {
		texts = append(texts, a.Question, a.Answer)
	}
	for _, raw := range texts {
		t := norm.NFKC.String(raw)
		for _, loc := range labelTokenRE.FindAllStringIndex(t, -1) {
			if !rateUnitRE.MatchString(t[loc[1]:]) {
				addOut(t[loc[0]:loc[1]])
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return jsstr.U16Len(out[a]) > jsstr.U16Len(out[b]) })
	if out == nil {
		out = []string{}
	}
	return out
}

// StrayNumbers are numbers in text not covered by a label.
func StrayNumbers(text string, labels []string) []string {
	u := utf16.Encode([]rune(text))
	covered := make([]bool, len(u))
	for _, l := range labels {
		lu := utf16.Encode([]rune(l))
		for i := indexU16(u, lu, 0); i >= 0; i = indexU16(u, lu, i+1) {
			for k := i; k < i+len(lu); k++ {
				covered[k] = true
			}
		}
	}
	out := []string{}
	for _, loc := range strayRE.FindAllStringIndex(text, -1) {
		m := text[loc[0]:loc[1]]
		at := len(utf16.Encode([]rune(text[:loc[0]])))
		rate := rateUnitRE.MatchString(text[loc[1]:])
		uncovered := false
		k := 0
		for _, ch := range m {
			if unicode.Is(unicode.Nd, ch) && (at+k >= len(covered) || !covered[at+k]) {
				uncovered = true
			}
			k++
		}
		if rate || uncovered {
			out = append(out, m)
		}
	}
	return out
}

// indexU16 is String.prototype.indexOf on code units.
func indexU16(s, sub []uint16, from int) int {
	if from > len(s) {
		return -1
	}
	for i := from; i+len(sub) <= len(s); i++ {
		match := true
		for j := range sub {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// SummaryCheck is ok with the final text, or the problems.
type SummaryCheck struct {
	OK       bool
	Text     string
	Problems []string
}

// CheckSummary recomputes the claims and checks the numbers in the text.
func CheckSummary(draft SummaryDraft, spec Spec, columns []Column, rows []Row, lang string) SummaryCheck {
	problems := []string{}
	filled := map[string]string{}
	for _, c := range draft.Claims {
		func() {
			var err error
			x := 0.0
			func() {
				defer catch(&err)
				x = compute(c, columns, rows)
			}()
			if err != nil {
				problems = append(problems, c.ID+": "+err.Error())
				return
			}
			if v, ok := matchDisplay(c, x, lang); ok {
				filled[c.ID] = v
			} else {
				problems = append(problems, fmt.Sprintf("%s: display %s does not match the computed value %s", c.ID, c.Display, FormatClaim(c.Op, x, lang)))
			}
		}()
	}
	var usedIDs []string
	for _, m := range placeholder.FindAllStringSubmatch(draft.Prose, -1) {
		if !contains(usedIDs, m[1]) {
			usedIDs = append(usedIDs, m[1])
		}
	}
	for _, id := range usedIDs {
		found := false
		for _, c := range draft.Claims {
			if c.ID == id {
				found = true
			}
		}
		if !found {
			problems = append(problems, "no claim for {"+id+"} in the text")
		}
	}
	bare := placeholder.ReplaceAllString(norm.NFKC.String(draft.Prose), " ")
	stray := StrayNumbers(bare, AllowedLabels(spec, columns, rows))
	if len(stray) > 0 {
		var uniq []string
		for _, s := range stray {
			if !contains(uniq, s) {
				uniq = append(uniq, s)
			}
		}
		if len(uniq) > 8 {
			uniq = uniq[:8]
		}
		problems = append(problems, "the text has numbers outside claims, or labels not in the result or definition: "+strings.Join(uniq, ", "))
	}
	if len(problems) > 0 {
		return SummaryCheck{Problems: problems}
	}
	text := placeholder.ReplaceAllStringFunc(draft.Prose, func(s string) string { return filled[s[1:len(s)-1]] })
	return SummaryCheck{OK: true, Text: text}
}

// JS returns the check in its stored JSON form.
func (c SummaryCheck) JS() jsjson.Object {
	if c.OK {
		return jsjson.Object{{Key: "ok", Value: true}, {Key: "text", Value: c.Text}}
	}
	return jsjson.Object{{Key: "ok", Value: false}, {Key: "problems", Value: c.Problems}}
}

// SummaryResult is the result sent to the agent: all rows up to 200, otherwise a deterministic summary.
func SummaryResult(columns []Column, rows []Row) jsjson.Object {
	names := make([]string, len(columns))
	for i, c := range columns {
		names[i] = c.Name
	}
	rowsJS := func(rs []Row) []any {
		out := make([]any, len(rs))
		for i, r := range rs {
			out[i] = []any(r)
		}
		return out
	}
	if len(rows) <= 200 {
		return jsjson.Object{{Key: "columns", Value: names}, {Key: "rows", Value: rowsJS(rows)}}
	}
	stats := make([]any, len(names))
	for i, name := range names {
		var nums []float64
		for _, r := range rows {
			if i < len(r) {
				if f, ok := r[i].(float64); ok {
					nums = append(nums, f)
				}
			}
		}
		if len(nums) == 0 {
			stats[i] = jsjson.Object{{Key: "name", Value: name}}
			continue
		}
		mn, mx, sum := math.Inf(1), math.Inf(-1), 0.0
		for _, f := range nums {
			mn = math.Min(mn, f)
			mx = math.Max(mx, f)
			sum += f
		}
		stats[i] = jsjson.Object{{Key: "name", Value: name}, {Key: "min", Value: mn}, {Key: "max", Value: mx}, {Key: "sum", Value: sum}}
	}
	last := rows
	if len(rows) > 20 {
		last = rows[len(rows)-20:]
	}
	return jsjson.Object{{Key: "columns", Value: names}, {Key: "row_count", Value: len(rows)}, {Key: "stats", Value: stats}, {Key: "first_rows", Value: rowsJS(rows[:20])}, {Key: "last_rows", Value: rowsJS(last)}}
}
