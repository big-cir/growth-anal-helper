package agent

import (
	"growth-lab/internal/jsstr"
	"math"
	"strings"
	"unicode/utf16"

	"growth-lab/internal/collect"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
)

// Tagged is a typed cell from the query worker: n (null), s, i, f.
type Tagged struct {
	Tag   string
	Value any
}

// QueryResult is a probe result.
type QueryResult struct {
	OK             bool
	Columns        []panels.Column
	Rows           [][]Tagged
	More           bool
	TruncatedCells int
	Ms             float64
	Tables         []string
	Kind           string
	Message        string
}

// PanelResult is a panel run result.
type PanelResult struct {
	OK         bool
	Columns    []panels.Column
	Real       []panels.Row
	Agent      []panels.Row
	Ms         float64
	Tables     []string
	Stage      string
	Message    string
	Violations []panels.Violation
}

// OutboundBlocked: data the dataMode forbids was about to be sent.
type OutboundBlocked struct{ Msg string }

func (e *OutboundBlocked) Error() string { return e.Msg }

// Piece is one part of a turn input.
type Piece struct {
	Kind           string // text, json, rows, stats, result, violation
	Text           string
	Label          string
	Value          any
	Target         string
	Columns        []string
	Rows           [][]Tagged
	More           bool
	TruncatedCells int
	Stats          []ColumnStats
	RowCount       int
	Stage          string
	Items          []any
}

// ColumnStats are the schema_only statistics of one column.
type ColumnStats struct {
	Name       string
	Nulls      int
	Min, Max   *float64
	Identifier bool
}

func (c ColumnStats) js() jsjson.Object {
	f := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	return jsjson.Object{{Key: "name", Value: c.Name}, {Key: "nulls", Value: c.Nulls}, {Key: "min", Value: f(c.Min)}, {Key: "max", Value: f(c.Max)}, {Key: "identifier", Value: c.Identifier}}
}

// Render builds the text; nothing that the mode forbids is sent.
func Render(mode string, pieces []Piece) (string, error) {
	var out []string
	for _, p := range pieces {
		switch p.Kind {
		case "text":
			out = append(out, p.Text)
		case "json":
			out = append(out, p.Label+":\n```json\n"+jsjson.Indent(p.Value, 1)+"\n```")
		case "rows":
			if mode != "pseudonymized" {
				return "", &OutboundBlocked{"result rows cannot be sent in schema_only mode"}
			}
			if p.Target != "agent" {
				return "", &OutboundBlocked{"only rows from the pseudonymized copy can be sent"}
			}
			rows := make([]any, len(p.Rows))
			for i, r := range p.Rows {
				vals := make([]any, len(r))
				for j, t := range r {
					vals[j] = t.Value
				}
				rows[i] = vals
			}
			out = append(out, "Result (pseudonymous IDs):\n```json\n"+jsjson.MustStringify(jsjson.Object{{Key: "columns", Value: p.Columns}, {Key: "rows", Value: rows}, {Key: "more", Value: p.More}, {Key: "truncated_cells", Value: p.TruncatedCells}})+"\n```")
		case "result":
			if mode != "pseudonymized" {
				return "", &OutboundBlocked{"result values cannot be sent in schema_only mode"}
			}
			if p.Target != "agent" {
				return "", &OutboundBlocked{"only results from the pseudonymized copy can be sent"}
			}
			out = append(out, "Panel result (pseudonymous IDs):\n```json\n"+jsjson.MustStringify(p.Value)+"\n```")
		case "stats":
			cols := make([]any, len(p.Stats))
			for i, c := range p.Stats {
				cols[i] = c.js()
			}
			out = append(out, "Result statistics (values not sent):\n```json\n"+jsjson.MustStringify(jsjson.Object{{Key: "row_count", Value: p.RowCount}, {Key: "more", Value: p.More}, {Key: "columns", Value: cols}})+"\n```")
		case "violation":
			out = append(out, "Violations ("+p.Stage+"):\n```json\n"+jsjson.MustStringify(p.Items)+"\n```")
		}
	}
	return strings.Join(out, "\n\n"), nil
}

// ColumnStatistics are column statistics for schema_only.
func ColumnStatistics(columns []panels.Column, rows [][]Tagged, roles *collect.Roles) []ColumnStats {
	out := make([]ColumnStats, len(columns))
	for i, c := range columns {
		identifier := false
		if c.Table != nil && c.Column != nil && *c.Table != "" && *c.Column != "" {
			if r, ok := roles.Get(*c.Table + "." + *c.Column); ok && r != collect.Ordinary {
				identifier = true
			}
		}
		s := ColumnStats{Name: c.Name, Identifier: identifier}
		for _, r := range rows {
			t := r[i]
			if t.Tag == "n" {
				s.Nulls++
			} else if (t.Tag == "i" || t.Tag == "f") && !identifier {
				v, _ := t.Value.(float64)
				if s.Min == nil {
					a, b := v, v
					s.Min, s.Max = &a, &b
				} else {
					a, b := math.Min(*s.Min, v), math.Max(*s.Max, v)
					s.Min, s.Max = &a, &b
				}
			}
		}
		out[i] = s
	}
	return out
}

// specJSON is the spec with the stored display form; sql replaces the SQL when not nil.
func specJSON(s panels.Spec, sql any) jsjson.Object {
	o := s.JS()
	for i := range o {
		switch o[i].Key {
		case "display":
			o[i].Value = panels.DisplayJSON(s.Display)
		case "sql":
			if sql != nil {
				o[i].Value = sql
			}
		}
	}
	return o
}

// Outbound builds the turn inputs sent to the agent.
type Outbound struct {
	Mode  string
	Roles *collect.Roles
}

func (o *Outbound) render(pieces ...Piece) (string, error) { return Render(o.Mode, pieces) }

func text(t string) Piece { return Piece{Kind: "text", Text: t} }

// Request is the first input of a request.
func (o *Outbound) Request(userText string, current *panels.Spec, recovery string) (string, error) {
	var ps []Piece
	if recovery != "" {
		ps = append(ps, text("Summary of the earlier conversation (continuing in a new session):\n"+recovery))
	}
	ps = append(ps, text("User request: "+userText))
	if current != nil {
		ps = append(ps, Piece{Kind: "json", Label: "Current preview panel spec (if this is a change request, edit this one; if it already answers the request, send keep)", Value: specJSON(*current, nil)})
	}
	return o.render(ps...)
}

// AnswerValues are the answers with defaults filled in (empty or missing answers use the default).
func AnswerValues(questions []AskQuestion, given map[string]string) []jsjson.Object {
	out := make([]jsjson.Object, len(questions))
	for i, q := range questions {
		a, ok := given[q.ID]
		if !ok || jsstr.Trim(a) == "" {
			out[i] = jsjson.Object{{Key: "id", Value: q.ID}, {Key: "answer", Value: q.DefaultLabel()}, {Key: "defaulted", Value: true}}
		} else {
			out[i] = jsjson.Object{{Key: "id", Value: q.ID}, {Key: "answer", Value: a}, {Key: "defaulted", Value: false}}
		}
	}
	return out
}

// Answers sends the answers to the questions.
func (o *Outbound) Answers(questions []AskQuestion, given map[string]string) (string, error) {
	vals := AnswerValues(questions, given)
	arr := make([]any, len(vals))
	for i, v := range vals {
		arr[i] = v
	}
	return o.render(text("Answers to your questions. Unanswered questions were set to their defaults."), Piece{Kind: "json", Label: "Answers", Value: arr})
}

// ProbeResult sends a probe outcome.
func (o *Outbound) ProbeResult(r QueryResult, remaining int) (string, error) {
	tail := text("Probes left: " + itoa(remaining) + ".")
	if !r.OK {
		return o.render(text("Probe query failed ("+r.Kind+"): "+r.Message), tail)
	}
	more := ""
	if r.More {
		more = " (more exist, first 50 only)"
	}
	head := text("Probe query result: " + itoa(len(r.Rows)) + " rows" + more + ", " + jsjson.Number(r.Ms) + "ms.")
	if o.Mode == "schema_only" {
		return o.render(head, Piece{Kind: "stats", Stats: ColumnStatistics(r.Columns, r.Rows, o.Roles), RowCount: len(r.Rows), More: r.More}, tail)
	}
	names := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		names[i] = c.Name
	}
	return o.render(head, Piece{Kind: "rows", Target: "agent", Columns: names, Rows: r.Rows, More: r.More, TruncatedCells: r.TruncatedCells}, tail)
}

var stageNames = map[string]string{
	"lint": "static check", "exec": "execution", "contract": "result contract", "invariant": "invariants", "id_column": "ID column in output",
	"id_dependent": "depends on ID values", "metric_tables": "metric dictionary table rule", "cancelled": "cancelled",
}

// PanelFailure sends a failed panel check.
func (o *Outbound) PanelFailure(r PanelResult, remainingFixes int) (string, error) {
	name, ok := stageNames[r.Stage]
	if !ok {
		name = r.Stage
	}
	head := "Panel check failed — stage: " + name + ". Fixes left: " + itoa(remainingFixes) + "."
	if o.Mode == "schema_only" && r.Stage == "invariant" {
		vs := r.Violations
		if len(vs) > 20 {
			vs = vs[:20]
		}
		items := make([]any, len(vs))
		for i, v := range vs {
			var idx, col any
			if v.Index != nil {
				idx = *v.Index
			}
			if v.Column != nil {
				col = *v.Column
			}
			items[i] = jsjson.Object{{Key: "rule", Value: v.Rule}, {Key: "index", Value: idx}, {Key: "column", Value: col}}
		}
		return o.render(text(head), Piece{Kind: "violation", Stage: r.Stage, Items: items})
	}
	return o.render(text(head + "\n" + r.Message))
}

// ZeroResult asks to check an all-zero result.
func (o *Outbound) ZeroResult() (string, error) {
	return o.render(text("Every number column in the panel result is 0 (or NULL). Check that the join and filter conditions really match. In particular, comparing a date string (YYYY-MM-DD) with a timestamp string (YYYY-MM-DD HH:MM:SS.ffffff) as-is matches no rows. Use a probe if needed, then send the fixed panel. If 0 is correct, send the same panel again and it will be accepted."))
}

// OffdictRejected asks for a dictionary metric.
func (o *Outbound) OffdictRejected() (string, error) {
	return o.render(text("The user did not accept a panel built on a definition outside the metric dictionary. Rebuild it with one of the dictionary metrics (its id in metric). If no dictionary metric fits, refuse and give the reason and the closest dictionary metrics."))
}

// SchemaMismatch asks for a reply matching the schema.
func (o *Outbound) SchemaMismatch(e string) (string, error) {
	return o.render(text("Your last reply does not match the action schema: " + e + "\nSend one action (ask | probe | panel | refuse | keep) that matches the schema."))
}

// Summarize asks for the long description.
func (o *Outbound) Summarize(spec panels.Spec, result jsjson.Object) (string, error) {
	return o.render(text(`Write "what this panel tells you" for the panel below.`), Piece{Kind: "json", Label: "Panel spec", Value: specJSON(spec, nil)}, Piece{Kind: "result", Target: "agent", Value: result})
}

// SummaryRetry sends the check problems.
func (o *Outbound) SummaryRetry(problems []string) (string, error) {
	lines := make([]string, len(problems))
	for i, p := range problems {
		lines[i] = "- " + p
	}
	return o.render(text("Your last description failed the check. Fix it and send it again.\n" + strings.Join(lines, "\n")))
}

// LastAsk is the latest questions and given answers.
type LastAsk struct {
	Questions []AskQuestion
	Answers   map[string]string
}

// cutU16 is t.length > n ? t.slice(0, n) + "…" : t.
func cutU16(t string, n int) any {
	if jsstr.U16Len(t) <= n {
		return t
	}
	return append(jsstr.U16Slice(t, n), utf16.Encode([]rune("…"))...)
}

// RecoverySummary is the earlier conversation for a new session (8,000 characters or fewer).
func (o *Outbound) RecoverySummary(inputs []string, lastAsk *LastAsk, current *panels.Spec) (string, error) {
	const max = 8000
	if len(inputs) > 5 {
		inputs = inputs[len(inputs)-5:]
	}
	in := make([]any, len(inputs))
	for i, t := range inputs {
		in[i] = cutU16(t, 500)
	}
	cur := current
	var sql any
	ask := lastAsk
	build := func() (string, error) {
		ps := []Piece{{Kind: "json", Label: "Recent user inputs", Value: in}}
		if ask != nil {
			qa := make([]any, len(ask.Questions))
			for i, q := range ask.Questions {
				a, ok := ask.Answers[q.ID]
				if !ok {
					a = q.DefaultLabel() + " (default)"
				}
				qa[i] = jsjson.Object{{Key: "question", Value: q.Text}, {Key: "answer", Value: cutU16(a, 300)}}
			}
			ps = append(ps, Piece{Kind: "json", Label: "Latest questions and answers", Value: qa})
		}
		if cur != nil {
			ps = append(ps, Piece{Kind: "json", Label: "Current preview panel spec", Value: specJSON(*cur, sql)})
		}
		return o.render(ps...)
	}
	t, err := build()
	if err != nil {
		return "", err
	}
	for jsstr.U16Len(t) > max && len(in) > 0 {
		in = in[1:]
		if t, err = build(); err != nil {
			return "", err
		}
	}
	if jsstr.U16Len(t) > max && cur != nil {
		over := jsstr.U16Len(t) - max
		c := *cur
		cur = &c
		keep := jsstr.U16Len(c.SQL) - over - 40
		sql = append(jsstr.U16Slice(c.SQL, keep), utf16.Encode([]rune("\n…(truncated)"))...)
		if t, err = build(); err != nil {
			return "", err
		}
	}
	if jsstr.U16Len(t) > max && cur != nil {
		c := *cur
		if len(c.Definition) > 2 {
			c.Definition = c.Definition[:2]
		}
		c.Caveats = []string{}
		cur = &c
		if t, err = build(); err != nil {
			return "", err
		}
	}
	if jsstr.U16Len(t) > max {
		cur = nil
		if t, err = build(); err != nil {
			return "", err
		}
	}
	if jsstr.U16Len(t) > max {
		ask = nil
		if t, err = build(); err != nil {
			return "", err
		}
	}
	for jsstr.U16Len(t) > max && len(in) > 0 {
		in = in[1:]
		if t, err = build(); err != nil {
			return "", err
		}
	}
	return t, nil
}

func itoa(n int) string { return jsjson.Number(float64(n)) }
