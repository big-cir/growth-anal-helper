// Package eval runs evaluation cases against the agent and grades the answers.
package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"growth-lab/internal/jsstr"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
)

// Error is an eval setup error shown to the user.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Expect is what a case expects: panel (metric, patterns), ask, or refuse (via preflight or agent).
type Expect struct {
	Action   string
	Metric   *string
	Patterns []string
	Via      string
}

// Reference is a case's reference panel; Hash covers the whole file.
type Reference struct {
	Path string
	Spec panels.Spec
	Hash string
}

// LabelAliases are accepted alternative labels.
type LabelAliases struct {
	StepName []struct {
		Step  string
		Names []string
	}
	Label []string
}

func (a LabelAliases) stepNames(step string) []string {
	for _, s := range a.StepName {
		if s.Step == step {
			return s.Names
		}
	}
	return nil
}

// Case is one eval/cases/<id>.json.
type Case struct {
	ID        string
	Kind      string
	Question  string
	Tags      []string
	Note      *string
	Expect    Expect
	Reference *Reference
	XGrain    string
	Aliases   LabelAliases
}

var (
	idRE       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	kindAction = map[string]string{"metric": "panel", "breakdown": "panel", "ambiguous": "ask", "refuse": "refuse"}
	graded     = []string{"number", "line", "bar", "funnel", "cohort"}
)

// CasesDir is <workspace>/eval/cases.
func CasesDir(wsDir string) string { return filepath.Join(wsDir, "eval", "cases") }

func fail(msg string) { panic(&Error{msg}) }

func only(o jsjson.Object, keys []string, at string) {
	for _, m := range o {
		if !contains(keys, m.Key) {
			fail(at + ": unknown key " + m.Key)
		}
	}
}

func strList(v any, at string) []string {
	arr, ok := v.([]any)
	if !ok {
		fail(at + ": must be an array of non-empty strings")
	}
	out := make([]string, len(arr))
	for i, x := range arr {
		s, ok := x.(string)
		if !ok || s == "" {
			fail(at + ": must be an array of non-empty strings")
		}
		out[i] = s
	}
	return out
}

func parseAliases(v any, present bool, at string) LabelAliases {
	var out LabelAliases
	if !present {
		return out
	}
	o, ok := v.(jsjson.Object)
	if !ok {
		fail(at + ": must be an object")
	}
	only(o, []string{"step_name", "label"}, at)
	if sv, ok := o.Get("step_name"); ok {
		so, isObj := sv.(jsjson.Object)
		if !isObj {
			fail(at + `.step_name: must be { "<step_no>": [names] }`)
		}
		for _, m := range so {
			out.StepName = append(out.StepName, struct {
				Step  string
				Names []string
			}{m.Key, strList(m.Value, at+".step_name."+m.Key)})
		}
	}
	if lv, ok := o.Get("label"); ok {
		out.Label = strList(lv, at+".label")
	}
	return out
}

// jsString is String(v) for values found in JSON.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case float64:
		return jsjson.Number(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

func strOrNull(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

// ParseCase reads one case file. metricIDs are the ids of the current metric dictionary.
func ParseCase(file string, metricIDs map[string]bool) (c *Case, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*Error)
			if !ok {
				panic(r)
			}
			err = e
		}
	}()
	name := strings.TrimSuffix(filepath.Base(file), ".json")
	at := "eval/cases/" + name + ".json"
	b, rerr := os.ReadFile(file)
	if rerr != nil {
		return nil, rerr
	}
	parsed, perr := jsjson.Parse(string(b))
	if perr != nil {
		fail(at + ": not valid JSON (" + perr.Error() + ")")
	}
	raw, ok := parsed.(jsjson.Object)
	if !ok {
		fail(at + ": must be an object")
	}
	only(raw, []string{"id", "kind", "question", "tags", "expect", "reference", "x_grain", "label_aliases", "note"}, at)
	get := func(k string) (any, bool) { return raw.Get(k) }
	idv, _ := get("id")
	id, ok := idv.(string)
	if !ok || !idRE.MatchString(id) {
		fail(at + ": id must match /" + idRE.String() + "/")
	}
	if id != name {
		fail(at + ": id (" + id + ") must equal the file name")
	}
	kv, _ := get("kind")
	kind, ok := kv.(string)
	if _, known := kindAction[kind]; !ok || !known {
		fail(at + ": kind must be metric | breakdown | ambiguous | refuse")
	}
	qv, _ := get("question")
	question, ok := qv.(string)
	if !ok || jsstr.Trim(question) == "" {
		fail(at + ": question is required")
	}
	tags := []string{}
	if tv, ok := get("tags"); ok {
		tags = strList(tv, at+".tags")
	}
	var note *string
	if nv, ok := get("note"); ok && nv != nil {
		s, isStr := nv.(string)
		if !isStr {
			fail(at + ".note: must be a string")
		}
		note = &s
	}
	ev, _ := get("expect")
	e, ok := ev.(jsjson.Object)
	if !ok {
		fail(at + ".expect: must be an object")
	}
	action, _ := e.Get("action")
	if action != kindAction[kind] {
		fail(at + ".expect.action: kind " + kind + " needs action " + kindAction[kind])
	}

	out := &Case{ID: id, Kind: kind, Question: question, Tags: tags, Note: note}
	switch kindAction[kind] {
	case "panel":
		only(e, []string{"action", "metric", "pattern"}, at+".expect")
		mv, present := e.Get("metric")
		var metric *string
		if !present || mv != nil {
			s, isStr := mv.(string)
			if !present || !isStr || !metricIDs[s] {
				fail(at + ".expect.metric: must be a metric dictionary id or null")
			}
			metric = &s
		}
		pv, _ := e.Get("pattern")
		var pats []any
		switch p := pv.(type) {
		case string:
			pats = []any{p}
		case []any:
			pats = p
		}
		if len(pats) == 0 {
			fail(at + ".expect.pattern: a pattern or a list of patterns")
		}
		var patterns []string
		for _, p := range pats {
			s, isStr := p.(string)
			if !isStr || !contains(graded, s) {
				fail(at + ".expect.pattern: " + jsString(p) + " is not graded (use " + strings.Join(graded, ", ") + ")")
			}
			if !contains(patterns, s) {
				patterns = append(patterns, s)
			}
		}
		out.Expect = Expect{Action: "panel", Metric: metric, Patterns: patterns}
		rv, _ := get("reference")
		ref, isStr := rv.(string)
		if !isStr || ref == "" {
			fail(at + ".reference: path of the reference panel (relative to eval/cases)")
		}
		refPath := filepath.Join(filepath.Dir(file), ref)
		if filepath.IsAbs(ref) {
			refPath = filepath.Clean(ref)
		}
		text, rerr := os.ReadFile(refPath)
		if rerr != nil {
			fail(at + ".reference: file not found: " + ref)
		}
		specRaw, perr := jsjson.Parse(string(text))
		if perr != nil {
			fail(at + ".reference: " + perr.Error())
		}
		spec, serr := panels.ParseSpec(specRaw)
		if serr != nil {
			fail(at + ".reference: " + serr.Error())
		}
		if strOrNull(spec.Metric) != strOrNull(metric) || (spec.Metric == nil) != (metric == nil) {
			fail(at + ": expect.metric (" + strOrNull(metric) + ") differs from the reference metric (" + strOrNull(spec.Metric) + ")")
		}
		if !contains(patterns, spec.Display.Type) {
			fail(at + ": the reference pattern (" + spec.Display.Type + ") is not in expect.pattern")
		}
		sum := sha256.Sum256(text)
		out.Reference = &Reference{Path: refPath, Spec: spec, Hash: hex.EncodeToString(sum[:])}
		if xv, ok := get("x_grain"); ok && xv != nil {
			if xv != "day" && xv != "week" && xv != "month" {
				fail(at + ".x_grain: day | week | month | null")
			}
			out.XGrain = xv.(string)
		}
	default:
		_, r := get("reference")
		_, x := get("x_grain")
		_, l := get("label_aliases")
		if r || x || l {
			fail(at + ": reference, x_grain and label_aliases are only for panel cases")
		}
		if kindAction[kind] == "ask" {
			only(e, []string{"action"}, at+".expect")
			out.Expect = Expect{Action: "ask"}
		} else {
			only(e, []string{"action", "via"}, at+".expect")
			via, _ := e.Get("via")
			if via != "preflight" && via != "agent" {
				fail(at + ".expect.via: preflight | agent")
			}
			out.Expect = Expect{Action: "refuse", Via: via.(string)}
		}
	}
	lv, present := get("label_aliases")
	out.Aliases = parseAliases(lv, present, at+".label_aliases")
	return out, nil
}

// LoadCases reads all cases; problems are collected per file so `eval check` can list them together.
func LoadCases(wsDir string, metricIDs map[string]bool) ([]*Case, []string, error) {
	dir := CasesDir(wsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, &Error{"no cases folder: " + dir}
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var cases []*Case
	var problems []string
	for _, n := range names {
		c, err := ParseCase(filepath.Join(dir, n), metricIDs)
		if err != nil {
			var e *Error
			if !asError(err, &e) {
				return nil, nil, err
			}
			problems = append(problems, e.Msg)
			continue
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 && len(problems) == 0 {
		problems = append(problems, "no case files in "+dir)
	}
	return cases, problems, nil
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
