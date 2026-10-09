// Package agent holds the agent: actions, outbound data, context, runners (Claude Code CLI, APIs), summaries and the request loop.
package agent

import (
	"fmt"
	"growth-lab/internal/jsstr"
	"regexp"

	"growth-lab/internal/assets"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
)

var actionSchemaText = assets.ActionsSchema

// ActionSchemaText is the action schema file; ActionSchemaArg is its compact form passed to the agent.
var (
	ActionSchemaText = actionSchemaText
	ActionSchemaArg  = mustCompact(actionSchemaText)
)

func mustCompact(s string) string {
	c, err := jsjson.Compact([]byte(s))
	if err != nil {
		panic(err)
	}
	return c
}

// Option is one answer choice.
type Option struct {
	Label     string
	IsDefault bool
}

// AskQuestion is one clarifying question.
type AskQuestion struct {
	ID            string
	Text          string
	Options       []Option
	AllowFreeText bool
}

// JS is the question in its stored JSON form.
func (q AskQuestion) JS() jsjson.Object {
	opts := make([]any, len(q.Options))
	for i, o := range q.Options {
		opts[i] = jsjson.Object{{Key: "label", Value: o.Label}, {Key: "is_default", Value: o.IsDefault}}
	}
	return jsjson.Object{{Key: "id", Value: q.ID}, {Key: "text", Value: q.Text}, {Key: "options", Value: opts}, {Key: "allow_free_text", Value: q.AllowFreeText}}
}

// DefaultLabel is the label of the default option.
func (q AskQuestion) DefaultLabel() string {
	for _, o := range q.Options {
		if o.IsDefault {
			return o.Label
		}
	}
	return ""
}

// Action is ask | probe | panel | refuse | keep (the current preview panel already answers the request).
type Action struct {
	Kind         string
	Questions    []AskQuestion
	Plan         string
	Purpose      string
	SQL          string
	Panel        panels.Spec
	Reason       string
	Alternatives []string
}

// JS is the action in its stored JSON form.
func (a Action) JS() jsjson.Object {
	switch a.Kind {
	case "ask":
		qs := make([]any, len(a.Questions))
		for i, q := range a.Questions {
			qs[i] = q.JS()
		}
		return jsjson.Object{{Key: "action", Value: "ask"}, {Key: "questions", Value: qs}}
	case "probe":
		return jsjson.Object{{Key: "action", Value: "probe"}, {Key: "plan", Value: a.Plan}, {Key: "purpose", Value: a.Purpose}, {Key: "sql", Value: a.SQL}}
	case "panel":
		return jsjson.Object{{Key: "action", Value: "panel"}, {Key: "plan", Value: a.Plan}, {Key: "panel", Value: a.Panel.JS()}}
	case "keep":
		return jsjson.Object{{Key: "action", Value: "keep"}, {Key: "plan", Value: a.Plan}}
	}
	return jsjson.Object{{Key: "action", Value: "refuse"}, {Key: "reason", Value: a.Reason}, {Key: "alternatives", Value: a.Alternatives}}
}

// ActionError is a reply that does not match the action schema.
type ActionError struct{ Msg string }

func (e *ActionError) Error() string { return e.Msg }

type actionFail struct{ err error }

func afail(msg string) { panic(actionFail{&ActionError{msg}}) }

func only(o jsjson.Object, keys []string, path string) {
	for _, m := range o {
		found := false
		for _, k := range keys {
			if k == m.Key {
				found = true
			}
		}
		if !found {
			afail(path + "." + m.Key + ": unknown key")
		}
	}
	for _, k := range keys {
		if _, ok := o.Get(k); !ok {
			afail(path + "." + k + ": required")
		}
	}
}

func astr(v any, path string, max int) string {
	s, ok := v.(string)
	if !ok || jsstr.Trim(s) == "" {
		afail(path + ": must be a non-empty string")
	}
	if jsstr.CPLen(s) > max {
		afail(fmt.Sprintf("%s: at most %d characters", path, max))
	}
	return s
}

func aobj(v any, path string) jsjson.Object {
	o, ok := v.(jsjson.Object)
	if !ok {
		afail(path + ": must be an object")
	}
	return o
}

func alist(v any, path string, min, max int) []any {
	a, ok := v.([]any)
	if !ok || len(a) < min || len(a) > max {
		afail(fmt.Sprintf("%s: must be an array of %d–%d items", path, min, max))
	}
	return a
}

func get(o jsjson.Object, k string) any {
	v, _ := o.Get(k)
	return v
}

var askIDRE = regexp.MustCompile(`^[a-z_]{1,32}$`)

// ParseAction turns structured_output ({ step: action }) into an action.
func ParseAction(raw any) (a Action, err error) {
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(actionFail)
			if !ok {
				panic(r)
			}
			err = f.err
		}
	}()
	top := aobj(raw, "structured_output")
	only(top, []string{"step"}, "")
	o := aobj(get(top, "step"), ".step")
	switch get(o, "action") {
	case "ask":
		only(o, []string{"action", "questions"}, "")
		ids := map[string]bool{}
		a.Kind = "ask"
		for i, q := range alist(get(o, "questions"), ".questions", 1, 4) {
			p := fmt.Sprintf(".questions[%d]", i)
			x := aobj(q, p)
			only(x, []string{"id", "text", "options", "allow_free_text"}, p)
			id, ok := get(x, "id").(string)
			if !ok || !askIDRE.MatchString(id) {
				afail(p + ".id: ^[a-z_]{1,32}$")
			}
			if ids[id] {
				afail(p + ".id: duplicate in this request")
			}
			ids[id] = true
			var opts []Option
			for j, op := range alist(get(x, "options"), p+".options", 1, 4) {
				op := op
				pj := fmt.Sprintf("%s.options[%d]", p, j)
				y := aobj(op, pj)
				only(y, []string{"label", "is_default"}, pj)
				d, ok := get(y, "is_default").(bool)
				if !ok {
					afail(pj + ".is_default: true/false")
				}
				opts = append(opts, Option{Label: astr(get(y, "label"), pj+".label", 60), IsDefault: d})
			}
			n := 0
			for _, op := range opts {
				if op.IsDefault {
					n++
				}
			}
			if n != 1 {
				afail(p + ".options: exactly one default (is_default: true)")
			}
			free, ok := get(x, "allow_free_text").(bool)
			if !ok {
				afail(p + ".allow_free_text: true/false")
			}
			a.Questions = append(a.Questions, AskQuestion{ID: id, Text: astr(get(x, "text"), p+".text", 200), Options: opts, AllowFreeText: free})
		}
		return a, nil
	case "probe":
		only(o, []string{"action", "plan", "purpose", "sql"}, "")
		return Action{Kind: "probe", Plan: astr(get(o, "plan"), ".plan", 200), Purpose: astr(get(o, "purpose"), ".purpose", 100), SQL: astr(get(o, "sql"), ".sql", 8000)}, nil
	case "panel":
		only(o, []string{"action", "plan", "panel"}, "")
		po, ok := get(o, "panel").(jsjson.Object)
		if _, has := po.Get("metric"); !ok || !has {
			afail(".panel.metric: required (metric dictionary id or null)")
		}
		plan := astr(get(o, "plan"), ".plan", 300)
		spec, err := panels.ParseSpec(stripNullRoles(po))
		if err != nil {
			if se, ok := err.(*panels.SpecError); ok {
				afail(se.Msg)
			}
			panic(err)
		}
		return Action{Kind: "panel", Plan: plan, Panel: spec}, nil
	case "refuse":
		only(o, []string{"action", "reason", "alternatives"}, "")
		reason := astr(get(o, "reason"), ".reason", 300)
		alts := []string{}
		for i, x := range alist(get(o, "alternatives"), ".alternatives", 0, 3) {
			alts = append(alts, astr(x, fmt.Sprintf(".alternatives[%d]", i), 100))
		}
		return Action{Kind: "refuse", Reason: reason, Alternatives: alts}, nil
	case "keep":
		only(o, []string{"action", "plan"}, "")
		return Action{Kind: "keep", Plan: astr(get(o, "plan"), ".plan", 300)}, nil
	}
	act, _ := o.Get("action")
	if _, present := o.Get("action"); !present {
		act = jsjson.Undefined
	}
	shown := "undefined"
	if act != jsjson.Undefined {
		shown = jsjson.MustStringify(act)
	}
	afail("action must be one of ask|probe|panel|refuse|keep (got " + shown + ")")
	return a, nil
}

// stripNullRoles drops null role columns.
func stripNullRoles(p jsjson.Object) jsjson.Object {
	var d jsjson.Object
	switch x := get(p, "display").(type) {
	case jsjson.Object:
		d = x
	case []any:
		for i, e := range x {
			d = append(d, jsjson.Member{Key: fmt.Sprint(i), Value: e})
		}
	default:
		return p
	}
	nd := jsjson.Object{}
	for _, m := range d {
		if m.Value != nil || m.Key == "headline" {
			nd = append(nd, m)
		}
	}
	out := make(jsjson.Object, len(p))
	for i, m := range p {
		if m.Key == "display" {
			m.Value = nd
		}
		out[i] = m
	}
	return out
}
