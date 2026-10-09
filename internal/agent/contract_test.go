package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"growth-lab/internal/jsstr"
	"path/filepath"
	"strings"
	"testing"

	"growth-lab/internal/collect"
	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/sqlitec"
)

// parse decodes vector JSON the way JSON.parse does.
func parse(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	if len(raw) == 0 {
		return jsjson.Undefined
	}
	v, err := jsjson.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func canon(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	s, err := jsjson.Compact(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type outVec struct {
	OK          bool            `json:"ok"`
	Value       json.RawMessage `json:"value"`
	Error       string          `json:"error"`
	ActionError bool            `json:"actionError"`
	Blocked     bool            `json:"blocked"`
}

func TestActions(t *testing.T) {
	var v struct {
		Actions []struct {
			Input json.RawMessage `json:"input"`
			Out   outVec          `json:"out"`
		} `json:"actions"`
	}
	if err := contract.Load("agent-actions.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Actions {
		a, err := ParseAction(parse(t, c.Input))
		if !c.Out.OK {
			_, isAE := err.(*ActionError)
			if err == nil || err.Error() != c.Out.Error || isAE != c.Out.ActionError {
				t.Errorf("%s: got %v, want %q", c.Input, err, c.Out.Error)
			}
			continue
		}
		if err != nil || jsjson.MustStringify(a.JS()) != canon(t, c.Out.Value) {
			t.Errorf("%s: got %v %s", c.Input, err, jsjson.MustStringify(a.JS()))
		}
	}
	t.Logf("%d actions", len(v.Actions))
}

// fixtures decoded from vector JSON

func strp(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func columnsOf(v any) []panels.Column {
	arr, _ := v.([]any)
	out := make([]panels.Column, len(arr))
	for i, c := range arr {
		out[i] = panels.Column{Name: field(c, "name").(string), Table: strp(field(c, "table")), Column: strp(field(c, "column"))}
	}
	return out
}

func rowsOf(v any) []panels.Row {
	arr, _ := v.([]any)
	out := make([]panels.Row, len(arr))
	for i, r := range arr {
		out[i] = r.([]any)
	}
	return out
}

func numOrZero(v any) float64 { f, _ := v.(float64); return f }

func queryOf(v any) QueryResult {
	if field(v, "ok") != true {
		return QueryResult{Kind: field(v, "kind").(string), Message: field(v, "message").(string)}
	}
	q := QueryResult{OK: true, Columns: columnsOf(field(v, "columns")), More: field(v, "more") == true, TruncatedCells: int(numOrZero(field(v, "truncatedCells"))), Ms: numOrZero(field(v, "ms"))}
	for _, r := range field(v, "rows").([]any) {
		var row []Tagged
		for _, c := range r.([]any) {
			p := c.([]any)
			row = append(row, Tagged{Tag: p[0].(string), Value: p[1]})
		}
		q.Rows = append(q.Rows, row)
	}
	if ts, ok := field(v, "tables").([]any); ok {
		for _, x := range ts {
			q.Tables = append(q.Tables, x.(string))
		}
	}
	return q
}

func panelOf(v any) PanelResult {
	if field(v, "ok") == true {
		r := PanelResult{OK: true, Columns: columnsOf(field(v, "columns")), Real: rowsOf(field(v, "real")), Agent: rowsOf(field(v, "agent")), Ms: numOrZero(field(v, "ms"))}
		for _, x := range field(v, "tables").([]any) {
			r.Tables = append(r.Tables, x.(string))
		}
		return r
	}
	r := PanelResult{Stage: field(v, "stage").(string), Message: field(v, "message").(string)}
	if vs, ok := field(v, "violations").([]any); ok {
		for _, x := range vs {
			viol := panels.Violation{Row: field(x, "row").(string), Problem: field(x, "problem").(string), Rule: field(x, "rule").(string), Column: strp(field(x, "column"))}
			if f, ok := field(x, "index").(float64); ok {
				n := int(f)
				viol.Index = &n
			}
			r.Violations = append(r.Violations, viol)
		}
	}
	return r
}

func specOf(t *testing.T, v any) *panels.Spec {
	if v == nil {
		return nil
	}
	s, err := panels.ParseSpec(v)
	if err != nil {
		t.Fatal(err)
	}
	return &s
}

func questionsOf(v any) []AskQuestion {
	var out []AskQuestion
	for _, q := range v.([]any) {
		aq := AskQuestion{ID: field(q, "id").(string), Text: field(q, "text").(string), AllowFreeText: field(q, "allow_free_text") == true}
		for _, o := range field(q, "options").([]any) {
			aq.Options = append(aq.Options, Option{Label: field(o, "label").(string), IsDefault: field(o, "is_default") == true})
		}
		out = append(out, aq)
	}
	return out
}

func givenOf(v any) map[string]string {
	m := map[string]string{}
	o, _ := v.(jsjson.Object)
	for _, e := range o {
		if s, ok := e.Value.(string); ok {
			m[e.Key] = s
		}
	}
	return m
}

func strsOf(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func rolesOf(v any) *collect.Roles {
	r := collect.NewRoles()
	for _, p := range v.([]any) {
		pair := p.([]any)
		switch x := pair[1].(type) {
		case string:
			r.Set(pair[0].(string), collect.Role{Name: x})
		case jsjson.Object:
			r.Set(pair[0].(string), collect.Role{Identifier: field(x, "identifier").(string)})
		}
	}
	return r
}

func TestOutbound(t *testing.T) {
	var v struct {
		Roles json.RawMessage `json:"roles"`
		Ops   []struct {
			Mode string          `json:"mode"`
			Op   string          `json:"op"`
			Args json.RawMessage `json:"args"`
			Out  outVec          `json:"out"`
		} `json:"ops"`
	}
	if err := contract.Load("agent-outbound.json", &v); err != nil {
		t.Fatal(err)
	}
	roles := rolesOf(parse(t, v.Roles))
	for i, c := range v.Ops {
		o := &Outbound{Mode: c.Mode, Roles: roles}
		a := parse(t, c.Args)
		var got string
		var err error
		switch c.Op {
		case "request":
			rec, _ := field(a, "recovery").(string)
			got, err = o.Request(field(a, "text").(string), specOf(t, field(a, "current")), rec)
		case "answers":
			got, err = o.Answers(questionsOf(field(a, "questions")), givenOf(field(a, "given")))
		case "probe":
			got, err = o.ProbeResult(queryOf(field(a, "result")), int(field(a, "remaining").(float64)))
		case "panelFailure":
			got, err = o.PanelFailure(panelOf(field(a, "result")), int(field(a, "remaining").(float64)))
		case "zero":
			got, err = o.ZeroResult()
		case "offdict":
			got, err = o.OffdictRejected()
		case "schema":
			got, err = o.SchemaMismatch(field(a, "error").(string))
		case "summarize":
			got, err = o.Summarize(*specOf(t, field(a, "spec")), field(a, "result").(jsjson.Object))
		case "summaryRetry":
			got, err = o.SummaryRetry(strsOf(field(a, "problems")))
		case "recovery":
			var ask *LastAsk
			if la := field(a, "lastAsk"); la != nil {
				ask = &LastAsk{Questions: questionsOf(field(la, "questions")), Answers: givenOf(field(la, "answers"))}
			}
			got, err = o.RecoverySummary(strsOf(field(a, "inputs")), ask, specOf(t, field(a, "current")))
		}
		if !c.Out.OK {
			_, blocked := err.(*OutboundBlocked)
			if err == nil || err.Error() != c.Out.Error || blocked != c.Out.Blocked {
				t.Errorf("%d %s %s: got %v, want error %q", i, c.Mode, c.Op, err, c.Out.Error)
			}
			continue
		}
		var want string
		_ = json.Unmarshal(c.Out.Value, &want)
		if err != nil || got != want {
			t.Errorf("%d %s %s: err %v\n got %q\nwant %q", i, c.Mode, c.Op, err, trunc(got), trunc(want))
		}
	}
	t.Logf("%d outbound ops", len(v.Ops))
}

func trunc(s string) string {
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestContext(t *testing.T) {
	var v struct {
		Schema struct {
			SQL   string `json:"sql"`
			Cases []struct {
				Guide    string   `json:"guide"`
				Readable []string `json:"readable"`
				Panel    []string `json:"panel"`
				Out      struct {
					Text   string   `json:"text"`
					Tables []string `json:"tables"`
				} `json:"out"`
			} `json:"cases"`
			Descriptions [][][2]string `json:"descriptions"`
		} `json:"schema"`
		Contexts []struct {
			Input   json.RawMessage `json:"input"`
			SeedSQL bool            `json:"seedSql"`
			Lang    string          `json:"lang"`
			Out     struct {
				OK     bool   `json:"ok"`
				Length int    `json:"length"`
				Sha    string `json:"sha"`
				Error  string `json:"error"`
			} `json:"out"`
		} `json:"contexts"`
		SummaryGuide map[string]string `json:"summaryGuide"`
	}
	if err := contract.Load("agent-context.json", &v); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "agent.sqlite")
	db, err := sqlitec.Open(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(v.Schema.SQL); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, c := range v.Schema.Cases {
		text, tables, err := SnapshotSchema(p, c.Readable, c.Panel, c.Guide)
		if err != nil || text != c.Out.Text || strings.Join(tables, ",") != strings.Join(c.Out.Tables, ",") {
			t.Errorf("schema %v %v: err %v\n got %q\nwant %q", c.Readable, c.Panel, err, text, c.Out.Text)
		}
	}
	guides := []string{}
	for _, c := range v.Schema.Cases {
		if len(guides) == 0 || guides[len(guides)-1] != c.Guide {
			guides = append(guides, c.Guide)
		}
	}
	for i, g := range guides {
		keys, desc := ColumnDescriptions(g)
		var got [][2]string
		for _, k := range keys {
			got = append(got, [2]string{k, desc[k]})
		}
		if jsjson.MustStringify(toAny(got)) != jsjson.MustStringify(toAny(v.Schema.Descriptions[i])) {
			t.Errorf("descriptions %q: got %v want %v", g, got, v.Schema.Descriptions[i])
		}
	}
	for i, c := range v.Contexts {
		in := parse(t, c.Input)
		st := field(in, "state")
		var seeds []SeedPanel
		for _, s := range field(in, "seedPanels").([]any) {
			seeds = append(seeds, SeedPanel{ID: field(s, "id").(string), Spec: *specOf(t, field(s, "raw"))})
		}
		cs, _ := field(st, "calendarStart").(string)
		params, _ := field(st, "params").(jsjson.Object)
		got, err := BuildContext(ContextInput{Schema: field(in, "schema").(string), Metrics: field(in, "metrics").(string), Guide: field(in, "guide").(string), SeedPanels: seeds,
			State: ContextState{AsOf: field(st, "asOf").(string), Today: field(st, "today").(string), CalendarStart: cs, Params: params}, Lang: c.Lang}, c.SeedSQL)
		if !c.Out.OK {
			if err == nil || err.Error() != c.Out.Error {
				t.Errorf("context %d: got %v want %q", i, err, c.Out.Error)
			}
			continue
		}
		if err != nil || sha(got) != c.Out.Sha {
			t.Errorf("context %d: err %v, length %d want %d", i, err, jsstr.U16Len(got), c.Out.Length)
		}
	}
	for lang, want := range v.SummaryGuide {
		if sha(SummaryGuide(lang)) != want {
			t.Errorf("summary guide %s differs", lang)
		}
	}
	t.Logf("%d schema cases, %d contexts", len(v.Schema.Cases), len(v.Contexts))
}

func toAny(pairs [][2]string) []any {
	out := []any{}
	for _, p := range pairs {
		out = append(out, []any{p[0], p[1]})
	}
	return out
}

func TestClaudePieces(t *testing.T) {
	var v struct {
		Env struct {
			Input map[string]string `json:"input"`
			Extra []string          `json:"extra"`
			Out   map[string]string `json:"out"`
			Plain map[string]string `json:"plain"`
		} `json:"env"`
		Args []struct {
			Input struct {
				Input, SystemPrompt, JSONSchema, SessionID, Model string
				BudgetUsd                                         float64
			} `json:"input"`
			Out []string `json:"out"`
		} `json:"args"`
		Streams []struct {
			Lines     []string        `json:"lines"`
			Init      json.RawMessage `json:"init"`
			Result    json.RawMessage `json:"result"`
			NonJSON   int             `json:"nonJson"`
			Violation *string         `json:"violation"`
		} `json:"streams"`
		Isolation []struct {
			Init json.RawMessage `json:"init"`
			Out  *string         `json:"out"`
		} `json:"isolation"`
		Stats []struct {
			Input json.RawMessage `json:"input"`
			Out   json.RawMessage `json:"out"`
		} `json:"stats"`
	}
	if err := contract.Load("agent-claude.json", &v); err != nil {
		t.Fatal(err)
	}
	var env []string
	for k, val := range v.Env.Input {
		env = append(env, k+"="+val)
	}
	check := func(got []string, want map[string]string) {
		m := map[string]string{}
		for _, kv := range got {
			k, val, _ := strings.Cut(kv, "=")
			m[k] = val
		}
		if jsjson.MustStringify(sortedObj(m)) != jsjson.MustStringify(sortedObj(want)) {
			t.Errorf("env: got %v want %v", m, want)
		}
	}
	check(ChildEnv(env, v.Env.Extra), v.Env.Out)
	check(ChildEnv(env, nil), v.Env.Plain)
	for _, c := range v.Args {
		got := BuildArgs(CallOptions{Input: c.Input.Input, SystemPrompt: c.Input.SystemPrompt, JSONSchema: c.Input.JSONSchema, BudgetUsd: c.Input.BudgetUsd, SessionID: c.Input.SessionID, Model: c.Input.Model})
		if jsjson.MustStringify(got) != jsjson.MustStringify(c.Out) {
			t.Errorf("args: got %v want %v", got, c.Out)
		}
	}
	for _, c := range v.Streams {
		p := &StreamParser{}
		for _, l := range c.Lines {
			p.Line(l)
		}
		want := ""
		if c.Violation != nil {
			want = *c.Violation
		}
		var initJS, resJS any = nil, nil
		if p.Init != nil {
			initJS = p.Init
		}
		if p.Result != nil {
			resJS = p.Result
		}
		if p.Violation != want || p.NonJSON != c.NonJSON || jsjson.MustStringify(initJS) != canon(t, c.Init) || jsjson.MustStringify(resJS) != canon(t, c.Result) {
			t.Errorf("stream %v: got %+v", c.Lines, p)
		}
	}
	for _, c := range v.Isolation {
		init, _ := parse(t, c.Init).(jsjson.Object)
		want := ""
		if c.Out != nil {
			want = *c.Out
		}
		if got := IsolationProblem(init); got != want {
			t.Errorf("isolation %s: got %q want %q", c.Init, got, want)
		}
	}
	for _, c := range v.Stats {
		r, _ := parse(t, c.Input).(jsjson.Object)
		u, api := ResultStats(r)
		o := jsjson.Object{}
		if u != nil {
			o = append(o, jsjson.Member{Key: "usage", Value: usageJS(u)})
		}
		if api != nil {
			o = append(o, jsjson.Member{Key: "apiMs", Value: *api})
		}
		if jsjson.MustStringify(o) != canon(t, c.Out) {
			t.Errorf("stats %s: got %s want %s", c.Input, jsjson.MustStringify(o), c.Out)
		}
	}
}

func usageJS(u *Usage) jsjson.Object {
	return jsjson.Object{{Key: "input", Value: u.Input}, {Key: "output", Value: u.Output}, {Key: "cacheRead", Value: u.CacheRead}, {Key: "cacheWrite", Value: u.CacheWrite}}
}

func sortedObj(m map[string]string) jsjson.Object {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	o := jsjson.Object{}
	for _, k := range keys {
		o = append(o, jsjson.Member{Key: k, Value: m[k]})
	}
	return o
}

func callOf(v any) CallResult {
	r := CallResult{OK: field(v, "ok") == true, CostUsd: numOrZero(field(v, "costUsd")), Ms: numOrZero(field(v, "ms"))}
	r.SessionID, _ = field(v, "sessionId").(string)
	r.Type, _ = field(v, "type").(string)
	r.Message, _ = field(v, "message").(string)
	o, _ := v.(jsjson.Object)
	if s, ok := o.Get("structured"); ok {
		r.Structured = s
	} else {
		r.Structured = jsjson.Undefined
	}
	return r
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestLoop(t *testing.T) {
	var v struct {
		DefaultProbe json.RawMessage `json:"defaultProbe"`
		DefaultPanel json.RawMessage `json:"defaultPanel"`
		Loops        []struct {
			Scenario json.RawMessage `json:"scenario"`
			Result   json.RawMessage `json:"result"`
		} `json:"loops"`
		Summaries []struct {
			Scenario json.RawMessage `json:"scenario"`
			Result   json.RawMessage `json:"result"`
		} `json:"summaries"`
	}
	if err := contract.Load("agent-loop.json", &v); err != nil {
		t.Fatal(err)
	}
	roles := rolesOf(parse(t, json.RawMessage(`[["r_m.id",{"identifier":"m"}],["r_m.n","ordinary"],["r_m.secret","private"]]`)))
	for _, c := range v.Loops {
		s := parse(t, c.Scenario)
		name := field(s, "name").(string)
		lang, _ := field(s, "lang").(string)
		if lang == "" {
			lang = "en"
		}
		mode, _ := field(s, "mode").(string)
		if mode == "" {
			mode = "pseudonymized"
		}
		limits := Limits{MaxTurns: 8, MaxProbes: 4, MaxFixes: 2, CallBudgetUsd: 0.5, RequestBudgetUsd: 1.0}
		if l, ok := field(s, "limits").(jsjson.Object); ok {
			for _, m := range l {
				f := m.Value.(float64)
				switch m.Key {
				case "maxTurns":
					limits.MaxTurns = int(f)
				case "maxProbes":
					limits.MaxProbes = int(f)
				case "maxFixes":
					limits.MaxFixes = int(f)
				case "callBudgetUsd":
					limits.CallBudgetUsd = f
				case "requestBudgetUsd":
					limits.RequestBudgetUsd = f
				}
			}
		}
		calls, _ := field(s, "calls").([]any)
		probes, _ := field(s, "probes").([]any)
		pans, _ := field(s, "panels").([]any)
		var events, callLog, probeLog, panelLog []any
		disabled := false
		var req *Request
		req = NewRequest("r", LoopDeps{
			Call: func(_ context.Context, input, sid string, budget float64) CallResult {
				callLog = append(callLog, jsjson.Object{{Key: "input", Value: input}, {Key: "sessionId", Value: nullable(sid)}, {Key: "budgetUsd", Value: budget}})
				if len(calls) == 0 {
					return CallResult{Type: "error", Message: "script ended", Ms: 5}
				}
				c := calls[0]
				calls = calls[1:]
				return callOf(c)
			},
			Probe: func(_ context.Context, sql string) QueryResult {
				probeLog = append(probeLog, sql)
				if len(probes) == 0 {
					return queryOf(parse(t, v.DefaultProbe))
				}
				p := probes[0]
				probes = probes[1:]
				return queryOf(p)
			},
			Panel: func(_ context.Context, spec panels.Spec) PanelResult {
				panelLog = append(panelLog, spec.JS())
				if len(pans) == 0 {
					return panelOf(parse(t, v.DefaultPanel))
				}
				p := pans[0]
				pans = pans[1:]
				return panelOf(p)
			},
			Sleep:    func(context.Context, int) {},
			Outbound: &Outbound{Mode: mode, Roles: roles},
			Limits:   limits,
			Emit: func(ev Event, turnNo int) {
				o := ev.JS()
				events = append(events, append(o, jsjson.Member{Key: "turnNo", Value: turnNo}))
			},
			OnIsolationFailure: func() { disabled = true },
			HeartbeatMs:        2000000000,
			Lang:               lang,
		})
		st := field(s, "start")
		start := StartInput{Text: "Show the connection rate by signup week"}
		if st != nil {
			start.Text = field(st, "text").(string)
			start.Current = specOf(t, field(st, "current"))
			start.ResumeSessionID, _ = field(st, "resumeSessionId").(string)
			start.Recovery, _ = field(st, "recovery").(string)
		}
		req.Run(start)
		var accepted []any
		if then, ok := field(s, "then").([]any); ok {
			for _, x := range then {
				turn := req.TurnNo()
				if f, ok := field(x, "turn").(float64); ok {
					turn = int(f)
				}
				var acc bool
				if field(x, "op") == "answer" {
					acc = req.Answer(turn, givenOf(field(x, "given")))
				} else {
					acc = req.ApproveOffdict(turn, field(x, "approve") == true)
				}
				accepted = append(accepted, acc)
			}
		}
		if accepted == nil {
			accepted = []any{}
		}
		nz := func(a []any) []any {
			if a == nil {
				return []any{}
			}
			return a
		}
		got := jsjson.Object{{Key: "events", Value: nz(events)}, {Key: "calls", Value: nz(callLog)}, {Key: "probes", Value: nz(probeLog)}, {Key: "panels", Value: nz(panelLog)}, {Key: "accepted", Value: accepted},
			{Key: "state", Value: req.State()}, {Key: "counts", Value: jsjson.Object{{Key: "turns", Value: req.Turns}, {Key: "probes", Value: req.Probes}, {Key: "fixes", Value: req.Fixes}}},
			{Key: "costTotal", Value: req.CostTotal}, {Key: "sessionId", Value: nullable(req.SessionID())}, {Key: "disabled", Value: disabled}}
		want := canon(t, c.Result)
		if g := jsjson.MustStringify(got); g != want {
			t.Errorf("loop %s:\n got %s\nwant %s", name, trunc(firstDiff(g, want)), trunc(firstDiff(want, g)))
		}
	}
	for _, c := range v.Summaries {
		s := parse(t, c.Scenario)
		calls := field(s, "calls").([]any)
		var log []any
		spec := specOf(t, parse(t, json.RawMessage(`{"metric":"demo_metric","title":"t","question":"q","sql":"SELECT 1 AS numerator, 2 AS denominator","display":{"type":"number","numerator":"numerator","denominator":"denominator"},"definition":[["Population","x"]],"caveats":[],"answers":[]}`)))
		out, err := Summarize(SummarizeInput{Spec: *spec, Columns: columnsOf(parse(t, json.RawMessage(`[{"name":"numerator","table":null,"column":null},{"name":"denominator","table":null,"column":null}]`))),
			Rows: []panels.Row{{30.0, 120.0}}, Outbound: &Outbound{Mode: "pseudonymized", Roles: roles}, Lang: field(s, "lang").(string),
			Call: func(input, sid string) CallResult {
				log = append(log, jsjson.Object{{Key: "input", Value: input}, {Key: "sessionId", Value: nullable(sid)}})
				c := calls[0]
				calls = calls[1:]
				return callOf(c)
			}})
		if err != nil {
			t.Fatal(err)
		}
		o := jsjson.Object{{Key: "status", Value: map[bool]string{true: "ok", false: "failed"}[out.OK]}}
		if out.OK {
			o = append(o, jsjson.Member{Key: "text", Value: out.Text})
		} else {
			o = append(o, jsjson.Member{Key: "message", Value: out.Message})
		}
		got := jsjson.MustStringify(jsjson.Object{{Key: "calls", Value: log}, {Key: "out", Value: o}})
		if want := canon(t, c.Result); got != want {
			t.Errorf("summary %s:\n got %s\nwant %s", field(s, "name"), trunc(got), trunc(want))
		}
	}
	t.Logf("%d loops, %d summaries", len(v.Loops), len(v.Summaries))
}

// firstDiff shows a from where it starts to differ from b.
func firstDiff(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	if i > 80 {
		i -= 80
	} else {
		i = 0
	}
	return a[i:]
}
