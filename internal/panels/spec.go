package panels

import (
	"fmt"
	"growth-lab/internal/jsstr"
	"regexp"
	"strings"

	"growth-lab/internal/jsjson"
)

// Patterns.
var Types = []string{"number", "table", "line", "bar", "funnel", "cohort"}

type roleSet struct{ required, optional []string }

// Roles per pattern. An omitted required role uses the column of the same name.
var Roles = map[string]roleSet{
	"number": {nil, []string{"numerator", "denominator", "value", "label"}},
	"table":  {nil, nil},
	"line":   {[]string{"x", "numerator", "denominator"}, []string{"series"}},
	"bar":    {[]string{"x", "numerator", "denominator"}, []string{"series"}},
	"funnel": {[]string{"step_no", "step_name", "reached", "eligible", "unknown"}, []string{"cohort"}},
	"cohort": {[]string{"cohort", "period", "numerator", "denominator"}, []string{"series", "deleted_n"}},
}

// Headline picks the headline row; nil fields were not given.
type Headline struct {
	X         *string
	HasSeries bool
	Series    *string
}

// Display is the chart mapping. Columns keep role order; a nil column is unused.
type Display struct {
	Type     string
	Columns  []Member
	Extra    []string
	Key      []string
	Headline *Headline
}

// Member is one role and its column (nil when unused).
type Member struct {
	Role   string
	Column *string
}

// Col returns the column of a role, or "".
func (d Display) Col(role string) string {
	for _, m := range d.Columns {
		if m.Role == role && m.Column != nil {
			return *m.Column
		}
	}
	return ""
}

func (d *Display) set(role string, col *string) {
	for i := range d.Columns {
		if d.Columns[i].Role == role {
			d.Columns[i].Column = col
			return
		}
	}
	d.Columns = append(d.Columns, Member{role, col})
}

// Answer is one answered clarifying question.
type Answer struct {
	Question, Answer string
	Defaulted        bool
}

// Spec is a panel spec. Metric nil means outside the dictionary.
type Spec struct {
	Metric     *string
	Title      string
	Question   string
	SQL        string
	Display    Display
	Definition [][2]string
	Caveats    []string
	Answers    []Answer
}

// SpecError is a panel spec validation error.
type SpecError struct{ Msg string }

func (e *SpecError) Error() string { return e.Msg }

type failure struct{ err error }

func throw(err error) { panic(failure{err}) }

func catch(err *error) {
	if r := recover(); r != nil {
		f, ok := r.(failure)
		if !ok {
			panic(r)
		}
		*err = f.err
	}
}

func specFail(path, msg string) { throw(&SpecError{"panel" + path + ": " + msg}) }

var (
	colRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	metricRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
)

// Limits of a panel spec.
var Limits = struct{ Title, Question, SQL, Definition, DefinitionText, Caveats, CaveatText, Answers int }{60, 300, 12000, 10, 300, 5, 200, 8}

func specStr(v any, path string, max int) string {
	s, ok := v.(string)
	if !ok || jsstr.Trim(s) == "" {
		specFail(path, "must be a non-empty string")
	}
	if jsstr.CPLen(s) > max {
		specFail(path, fmt.Sprintf("at most %d characters", max))
	}
	return s
}

func specArr(v any, path string, min, max int) []any {
	a, ok := v.([]any)
	if !ok {
		specFail(path, "must be an array")
	}
	if len(a) < min || len(a) > max {
		specFail(path, fmt.Sprintf("must have %d–%d items", min, max))
	}
	return a
}

func colName(v any, path string) string {
	s, ok := v.(string)
	if !ok || !colRE.MatchString(s) {
		specFail(path, "must be a result column name (identifier)")
	}
	return s
}

func lookup(o jsjson.Object, k string) (any, bool) { return o.Get(k) }

func given(o jsjson.Object, k string) bool {
	_, ok := o.Get(k)
	return ok
}

func givenNonNull(o jsjson.Object, k string) bool {
	v, ok := o.Get(k)
	return ok && v != nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func parseDisplay(raw any) Display {
	d, ok := raw.(jsjson.Object)
	if !ok {
		specFail(".display", "must be an object")
	}
	tv, _ := lookup(d, "type")
	t, _ := tv.(string)
	if !contains(Types, t) {
		specFail(".display.type", strings.Join(Types, "|"))
	}
	roles := Roles[t]
	allowed := append(append([]string{"type", "extra", "key", "headline"}, roles.required...), roles.optional...)
	for _, m := range d {
		if !contains(allowed, m.Key) {
			specFail(".display."+m.Key, "not a key of the "+t+" pattern")
		}
	}
	out := Display{Type: t, Columns: []Member{}}
	for _, r := range roles.required {
		c := r
		if v, ok := lookup(d, r); ok {
			c = colName(v, ".display."+r)
		}
		out.set(r, &c)
	}
	for _, r := range roles.optional {
		if givenNonNull(d, r) {
			v, _ := lookup(d, r)
			c := colName(v, ".display."+r)
			out.set(r, &c)
		} else {
			out.set(r, nil)
		}
	}
	if t == "number" {
		if given(d, "numerator") || given(d, "denominator") {
			num := "numerator"
			if v, ok := lookup(d, "numerator"); ok && v != nil {
				num = colName(v, ".display.numerator")
			} else {
				num = colName(num, ".display.numerator")
			}
			den := "denominator"
			if v, ok := lookup(d, "denominator"); ok && v != nil {
				den = colName(v, ".display.denominator")
			} else {
				den = colName(den, ".display.denominator")
			}
			out.set("numerator", &num)
			out.set("denominator", &den)
			if givenNonNull(d, "value") {
				specFail(".display.value", "numerator/denominator and value cannot be used together")
			}
		} else if !givenNonNull(d, "value") {
			num, den := "numerator", "denominator"
			out.set("numerator", &num)
			out.set("denominator", &den)
		}
	}
	out.Extra = []string{}
	if givenNonNull(d, "extra") {
		v, _ := lookup(d, "extra")
		for i, x := range specArr(v, ".display.extra", 0, 6) {
			out.Extra = append(out.Extra, colName(x, fmt.Sprintf(".display.extra[%d]", i)))
		}
	}
	out.Key = []string{}
	if givenNonNull(d, "key") {
		v, _ := lookup(d, "key")
		for i, x := range specArr(v, ".display.key", 0, 6) {
			out.Key = append(out.Key, colName(x, fmt.Sprintf(".display.key[%d]", i)))
		}
	}
	if len(out.Key) > 0 && t != "table" {
		specFail(".display.key", "table pattern only (other patterns have fixed row keys)")
	}
	if givenNonNull(d, "headline") {
		hv, _ := lookup(d, "headline")
		h, ok := hv.(jsjson.Object)
		if !ok {
			specFail(".display.headline", "must be an object")
		}
		for _, m := range h {
			if m.Key != "x" && m.Key != "series" {
				specFail(".display.headline."+m.Key, "unknown key")
			}
		}
		if t == "table" || t == "cohort" {
			specFail(".display.headline", "the "+t+" pattern has no headline number")
		}
		hl := &Headline{}
		if v, ok := lookup(h, "x"); ok {
			x := specStr(v, ".display.headline.x", 100)
			hl.X = &x
		}
		if v, ok := lookup(h, "series"); ok {
			hl.HasSeries = true
			if v != nil {
				s := specStr(v, ".display.headline.series", 100)
				hl.Series = &s
			}
		}
		out.Headline = hl
	}
	return out
}

// ParseSpec validates a panel spec (decoded by jsjson.Parse).
func ParseSpec(raw any) (spec Spec, err error) {
	defer catch(&err)
	o, ok := raw.(jsjson.Object)
	if !ok {
		specFail("", "must be an object")
	}
	if mv, ok := lookup(o, "metric"); ok && mv != nil {
		s, isStr := mv.(string)
		if !isStr || !metricRE.MatchString(s) {
			specFail(".metric", "must be a metric dictionary id or null")
		}
	}
	if mv, ok := lookup(o, "metric"); ok {
		if s, isStr := mv.(string); isStr {
			spec.Metric = &s
		}
	}
	get := func(k string) any { v, _ := lookup(o, k); return v }
	spec.Title = specStr(get("title"), ".title", Limits.Title)
	spec.Question = specStr(get("question"), ".question", Limits.Question)
	spec.SQL = specStr(get("sql"), ".sql", Limits.SQL)
	spec.Display = parseDisplay(get("display"))
	for i, p := range specArr(get("definition"), ".definition", 1, Limits.Definition) {
		pair, ok := p.([]any)
		if !ok || len(pair) != 2 {
			specFail(fmt.Sprintf(".definition[%d]", i), "must be an [item, text] pair")
		}
		spec.Definition = append(spec.Definition, [2]string{specStr(pair[0], fmt.Sprintf(".definition[%d][0]", i), 40), specStr(pair[1], fmt.Sprintf(".definition[%d][1]", i), Limits.DefinitionText)})
	}
	spec.Caveats = []string{}
	for i, c := range specArr(get("caveats"), ".caveats", 0, Limits.Caveats) {
		spec.Caveats = append(spec.Caveats, specStr(c, fmt.Sprintf(".caveats[%d]", i), Limits.CaveatText))
	}
	spec.Answers = []Answer{}
	for i, a := range specArr(get("answers"), ".answers", 0, Limits.Answers) {
		x, ok := a.(jsjson.Object)
		if !ok {
			specFail(fmt.Sprintf(".answers[%d]", i), "must be an object")
		}
		dv, _ := lookup(x, "defaulted")
		def, isBool := dv.(bool)
		if !isBool {
			specFail(fmt.Sprintf(".answers[%d].defaulted", i), "true/false")
		}
		qv, _ := lookup(x, "question")
		av, _ := lookup(x, "answer")
		spec.Answers = append(spec.Answers, Answer{Question: specStr(qv, fmt.Sprintf(".answers[%d].question", i), 200), Answer: specStr(av, fmt.Sprintf(".answers[%d].answer", i), 200), Defaulted: def})
	}
	return spec, nil
}

func headlineJS(h *Headline) jsjson.Object {
	o := jsjson.Object{}
	if h.X != nil {
		o = append(o, jsjson.Member{Key: "x", Value: *h.X})
	}
	if h.HasSeries {
		var s any
		if h.Series != nil {
			s = *h.Series
		}
		o = append(o, jsjson.Member{Key: "series", Value: s})
	}
	return o
}

// DisplayJSON is the stored form of a display (unused roles, empty lists and no headline are left out).
func DisplayJSON(d Display) jsjson.Object {
	out := jsjson.Object{{Key: "type", Value: d.Type}}
	for _, m := range d.Columns {
		if m.Column != nil {
			out = append(out, jsjson.Member{Key: m.Role, Value: *m.Column})
		}
	}
	if len(d.Extra) > 0 {
		out = append(out, jsjson.Member{Key: "extra", Value: d.Extra})
	}
	if len(d.Key) > 0 {
		out = append(out, jsjson.Member{Key: "key", Value: d.Key})
	}
	if d.Headline != nil {
		out = append(out, jsjson.Member{Key: "headline", Value: headlineJS(d.Headline)})
	}
	return out
}

func definitionJS(def [][2]string) []any {
	out := make([]any, len(def))
	for i, p := range def {
		out[i] = []string{p[0], p[1]}
	}
	return out
}

func answersJS(as []Answer) []any {
	out := make([]any, len(as))
	for i, a := range as {
		out[i] = jsjson.Object{{Key: "question", Value: a.Question}, {Key: "answer", Value: a.Answer}, {Key: "defaulted", Value: a.Defaulted}}
	}
	return out
}

func strPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// JS returns the spec in its stored JSON form.
func (s Spec) JS() jsjson.Object {
	cols := jsjson.Object{}
	for _, m := range s.Display.Columns {
		cols = append(cols, jsjson.Member{Key: m.Role, Value: strPtr(m.Column)})
	}
	var hl any
	if s.Display.Headline != nil {
		hl = headlineJS(s.Display.Headline)
	}
	display := jsjson.Object{{Key: "type", Value: s.Display.Type}, {Key: "columns", Value: cols}, {Key: "extra", Value: s.Display.Extra}, {Key: "key", Value: s.Display.Key}, {Key: "headline", Value: hl}}
	return jsjson.Object{{Key: "metric", Value: strPtr(s.Metric)}, {Key: "title", Value: s.Title}, {Key: "question", Value: s.Question}, {Key: "sql", Value: s.SQL}, {Key: "display", Value: display},
		{Key: "definition", Value: definitionJS(s.Definition)}, {Key: "caveats", Value: s.Caveats}, {Key: "answers", Value: answersJS(s.Answers)}}
}

// ComparisonCaveat and OffdictCaveat are the engine caveats.
func ComparisonCaveat(lang string) string {
	if lang == "ko" {
		return "관측 비교이며 인과효과가 아닙니다"
	}
	return "This is an observational comparison, not a causal effect"
}

func OffdictCaveat(lang string) string {
	if lang == "ko" {
		return "지표 사전에 없는 정의로 만든 패널이에요"
	}
	return "This panel uses a definition outside the metric dictionary"
}
