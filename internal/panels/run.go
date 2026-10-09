package panels

import (
	"context"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"growth-lab/internal/collect"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/query"
)

// RunInput runs a panel on the real copy and the pseudonymized copy.
type RunInput struct {
	Spec        Spec
	RealPath    string
	AgentPath   string
	AsOf        string
	Params      jsjson.Object
	PanelPrefix []string
	Metrics     MetricDict
	Blocked     query.BlockedList
	HeapLimitMb float64
	Roles       *collect.Roles
	Lease       *query.SlotLease
	Ctx         context.Context
}

// RunResult: on success the columns and both copies' rows; otherwise the failed stage
// (lint, exec, contract, invariant, id_column, id_dependent, metric_tables, sensitive, cancelled).
type RunResult struct {
	OK         bool
	Columns    []Column
	Real       []Row
	Agent      []Row
	Ms         float64
	Tables     []string
	Stage      string
	Message    string
	Violations []Violation
	// FailTables is set for metric_tables failures
	FailTables []string
}

// ToColumns converts query result columns.
func ToColumns(cs []query.ResultColumn) []Column {
	out := make([]Column, len(cs))
	for i, c := range cs {
		out[i] = Column{Name: c.Name, Table: c.Table, Column: c.Column}
	}
	return out
}

// UntagRows drops the type tags.
func UntagRows(rows [][]query.Tagged) []Row {
	out := make([]Row, len(rows))
	for i, r := range rows {
		row := make(Row, len(r))
		for j, t := range r {
			row[j] = t.Value
		}
		out[i] = row
	}
	return out
}

// DirectIdentifierColumns are result columns that come straight from a non-ordinary column.
func DirectIdentifierColumns(columns []Column, roles *collect.Roles) []string {
	var out []string
	for _, c := range columns {
		if c.Table == nil || c.Column == nil {
			continue
		}
		if r, ok := roles.Get(*c.Table + "." + *c.Column); ok && r != collect.Ordinary {
			out = append(out, c.Name)
		}
	}
	return out
}

var tagOrder = map[string]int{"n": 0, "i": 1, "f": 2, "s": 3}

func compareTagged(a, b query.Tagged) int {
	if a.Tag != b.Tag {
		return tagOrder[a.Tag] - tagOrder[b.Tag]
	}
	switch a.Tag {
	case "n":
		return 0
	case "s":
		return strings.Compare(a.Value.(string), b.Value.(string))
	}
	d := a.Value.(float64) - b.Value.(float64)
	if d < 0 {
		return -1
	}
	if d > 0 {
		return 1
	}
	return 0
}

func compareRow(a, b []query.Tagged) int {
	for i := range a {
		if c := compareTagged(a[i], b[i]); c != 0 {
			return c
		}
	}
	return 0
}

// SameResult compares ignoring row order.
func SameResult(a, b [][]query.Tagged) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([][]query.Tagged(nil), a...)
	sb := append([][]query.Tagged(nil), b...)
	sort.SliceStable(sa, func(i, j int) bool { return compareRow(sa[i], sa[j]) < 0 })
	sort.SliceStable(sb, func(i, j int) bool { return compareRow(sb[i], sb[j]) < 0 })
	for i := range sa {
		if len(sa[i]) != len(sb[i]) || compareRow(sa[i], sb[i]) != 0 {
			return false
		}
	}
	return true
}

var unreadableRE = regexp.MustCompile(`(?i)^[^:]*(unreadable|not readable|cannot read|can't read)[^:]*:`)

func fail(stage, msg string) RunResult { return RunResult{Stage: stage, Message: msg} }

// Run runs a panel and checks the contract, the invariants and that both copies agree.
func Run(o RunInput) RunResult {
	t0 := time.Now()
	req := func(path string) query.Request {
		blocked := o.Blocked
		return query.Request{Lease: o.Lease, SQL: o.Spec.SQL, Path: path, Mode: query.Panel, AsOf: o.AsOf, Params: o.Params, ReadablePrefixes: o.PanelPrefix, Blocked: &blocked, HeapLimitMb: o.HeapLimitMb, Ctx: o.Ctx}
	}
	real := query.RunQuery(req(o.RealPath))
	if !real.OK {
		switch real.Kind {
		case "cancelled", "sensitive":
			return fail(real.Kind, real.Message)
		}
		hint := ""
		if unreadableRE.MatchString(real.Message) {
			ps := make([]string, len(o.PanelPrefix))
			for i, p := range o.PanelPrefix {
				ps[i] = p + "*"
			}
			hint = " (panels can only read panel tables " + strings.Join(ps, ", ") + ")"
		}
		stage := "exec"
		if real.Kind == "lint" {
			stage = "lint"
		}
		return fail(stage, real.Message+hint)
	}
	if o.Spec.Metric != nil {
		if p := MetricTablesProblem(o.Metrics, *o.Spec.Metric, real.Tables); p != "" {
			r := fail("metric_tables", p)
			r.FailTables = real.Tables
			return r
		}
	}
	realCols := ToColumns(real.Columns)
	if ids := DirectIdentifierColumns(realCols, o.Roles); len(ids) > 0 {
		return fail("id_column", "ID columns cannot be in a panel result: "+strings.Join(ids, ", ")+" (use aggregates such as counts or rates)")
	}
	if err := CheckContract(o.Spec, realCols); err != nil {
		return fail("contract", err.Error())
	}
	agent := query.RunQuery(req(o.AgentPath))
	if !agent.OK {
		switch agent.Kind {
		case "cancelled", "sensitive":
			return fail(agent.Kind, agent.Message)
		}
		return fail("exec", agent.Message)
	}
	if strings.Join(agent.Tables, ",") != strings.Join(real.Tables, ",") {
		return fail("exec", "the real and pseudonymized copies read different tables (their schemas differ)")
	}
	agentRows := UntagRows(agent.Rows)
	realRows := UntagRows(real.Rows)
	agentCols := ToColumns(agent.Columns)
	v, err := CheckInvariants(o.Spec, agentCols, agentRows)
	if err != nil {
		return fail("contract", err.Error())
	}
	if len(v) > 0 {
		r := fail("invariant", "invariant violations:\n"+FormatViolations(v, 10))
		r.Violations = v
		return r
	}
	if rv, err := CheckInvariants(o.Spec, realCols, realRows); err != nil || len(rv) > 0 {
		return fail("id_dependent", "panel depends on ID values: do not depend on the size, range or order of IDs")
	}
	if !SameResult(real.Rows, agent.Rows) {
		return fail("id_dependent", "panel depends on ID values: the real and pseudonymized results differ. Do not depend on the size, range or order of IDs")
	}
	ms := math.Floor(float64(time.Since(t0).Microseconds())/1000 + 0.5)
	return RunResult{OK: true, Columns: realCols, Real: realRows, Agent: agentRows, Ms: ms, Tables: real.Tables}
}

// JS returns the result in its stored JSON form.
func (r RunResult) JS() jsjson.Object {
	if !r.OK {
		o := jsjson.Object{{Key: "ok", Value: false}, {Key: "stage", Value: r.Stage}, {Key: "message", Value: r.Message}}
		if r.Violations != nil {
			vs := make([]any, len(r.Violations))
			for i, v := range r.Violations {
				vs[i] = v.JS()
			}
			o = append(o, jsjson.Member{Key: "violations", Value: vs})
		}
		if r.FailTables != nil {
			o = append(o, jsjson.Member{Key: "tables", Value: r.FailTables})
		}
		return o
	}
	cols := make([]any, len(r.Columns))
	for i, c := range r.Columns {
		cols[i] = jsjson.Object{{Key: "name", Value: c.Name}, {Key: "table", Value: strPtr(c.Table)}, {Key: "column", Value: strPtr(c.Column)}}
	}
	rows := func(rs []Row) []any {
		out := make([]any, len(rs))
		for i, r := range rs {
			out[i] = []any(r)
		}
		return out
	}
	return jsjson.Object{{Key: "ok", Value: true}, {Key: "columns", Value: cols}, {Key: "real", Value: rows(r.Real)}, {Key: "agent", Value: rows(r.Agent)}, {Key: "ms", Value: r.Ms}, {Key: "tables", Value: r.Tables}}
}
