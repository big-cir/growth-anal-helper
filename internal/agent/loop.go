package agent

import (
	"context"
	"errors"
	"growth-lab/internal/i18n"
	"math"
	"sync"
	"time"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/sensitive"
)

// Event is one loop event; JS gives its JSON form.
type Event struct {
	Type         string
	SessionID    string
	Kind         string
	Text         string
	Ms           *float64
	Rows         *int
	Questions    []AskQuestion
	Answers      []jsjson.Object
	Spec         panels.Spec
	Columns      []panels.Column
	RealRows     []panels.Row
	AgentRows    []panels.Row
	Tables       []string
	Approve      bool
	Reason       string
	Alternatives []string
	Message      string
}

func columnsJS(cs []panels.Column) []any {
	out := make([]any, len(cs))
	for i, c := range cs {
		f := func(p *string) any {
			if p == nil {
				return nil
			}
			return *p
		}
		out[i] = jsjson.Object{{Key: "name", Value: c.Name}, {Key: "table", Value: f(c.Table)}, {Key: "column", Value: f(c.Column)}}
	}
	return out
}

func rowsJS(rs []panels.Row) []any {
	out := make([]any, len(rs))
	for i, r := range rs {
		out[i] = []any(r)
	}
	return out
}

// JS is the event in its JSON form.
func (e Event) JS() jsjson.Object {
	o := jsjson.Object{{Key: "type", Value: e.Type}}
	add := func(k string, v any) { o = append(o, jsjson.Member{Key: k, Value: v}) }
	switch e.Type {
	case "session":
		add("sessionId", e.SessionID)
	case "step":
		add("kind", e.Kind)
		add("text", e.Text)
		if e.Ms != nil {
			add("ms", *e.Ms)
		}
		if e.Rows != nil {
			add("rows", *e.Rows)
		}
	case "plan":
		add("text", e.Text)
	case "question":
		qs := make([]any, len(e.Questions))
		for i, q := range e.Questions {
			qs[i] = q.JS()
		}
		add("questions", qs)
	case "answers":
		as := make([]any, len(e.Answers))
		for i, a := range e.Answers {
			as[i] = a
		}
		add("answers", as)
	case "preview", "offdict":
		add("spec", e.Spec.JS())
		add("columns", columnsJS(e.Columns))
		add("rows", rowsJS(e.RealRows))
		if e.Type == "preview" {
			add("agentRows", rowsJS(e.AgentRows))
		}
		add("tables", e.Tables)
	case "offdict_answer":
		add("approve", e.Approve)
	case "refused":
		add("reason", e.Reason)
		add("alternatives", e.Alternatives)
	case "failed":
		add("reason", e.Reason)
		add("message", e.Message)
	}
	return o
}

// Limits are the per-request limits.
type Limits struct {
	MaxTurns, MaxProbes, MaxFixes   int
	CallBudgetUsd, RequestBudgetUsd float64
}

// LoopDeps connect the loop to the agent, queries and the conversation.
type LoopDeps struct {
	Call               func(ctx context.Context, input, sessionID string, budgetUsd float64) CallResult
	Probe              func(ctx context.Context, sql string) QueryResult
	Panel              func(ctx context.Context, spec panels.Spec) PanelResult
	Sleep              func(ctx context.Context, ms int)
	Outbound           *Outbound
	Limits             Limits
	Emit               func(ev Event, turnNo int)
	OnIsolationFailure func()
	// HeartbeatMs is the progress heartbeat interval (5000 when 0)
	HeartbeatMs int
	Lang        string
	// Keep returns the current preview's result when it can be shown again as is (same snapshot and context);
	// nil (or no Keep) reruns the current panel instead
	Keep func() *KeptResult
}

// KeptResult is the result of the current preview panel.
type KeptResult struct {
	Columns     []panels.Column
	Real, Agent []panels.Row
	Tables      []string
}

// StartInput is one user input. Recovery is the earlier conversation for the first turn of a new session.
type StartInput struct {
	Text            string
	Current         *panels.Spec
	ResumeSessionID string
	Recovery        string
}

// MinCallBudget is the smallest budget a call starts with.
const MinCallBudget = 0.02

var rateWaits = []int{10000, 30000}

var errStop = errors.New("stop")

// AllZero: there are rows, at least one number column, and every number is 0.
func AllZero(rows []panels.Row) bool {
	numbers := 0
	for _, row := range rows {
		for _, v := range row {
			f, ok := v.(float64)
			if !ok {
				continue
			}
			numbers++
			if f != 0 {
				return false
			}
		}
	}
	return len(rows) > 0 && numbers > 0
}

// SensitiveMessage does not say what was blocked.
func SensitiveMessage(lang string) string {
	return i18n.In(lang, "This tool can't work with that data", "이 도구가 다룰 수 없는 데이터예요")
}

type pendingAsk struct {
	questions []AskQuestion
	turnNo    int
}

type pendingOffdict struct {
	spec            panels.Spec
	columns         []panels.Column
	rows, agentRows []panels.Row
	tables          []string
	turnNo          int
}

// Request is the state machine for one request (one user input). A turn is one agent call.
// States: calling, querying, checking, waiting_user, done, failed, cancelling, cancelled.
type Request struct {
	ID string

	mu        sync.Mutex
	state     string
	turnNo    int
	Turns     int
	Probes    int
	Fixes     int
	CostTotal float64
	sessionID string
	ask       *pendingAsk
	offdict   *pendingOffdict

	deps       LoopDeps
	ctx        context.Context
	cancelFn   context.CancelFunc
	fixPending bool
	zeroWarned bool
	work       sync.WaitGroup
	start      *StartInput
}

// NewRequest creates a request in the calling state.
func NewRequest(id string, deps LoopDeps) *Request {
	ctx, cancel := context.WithCancel(context.Background())
	if deps.HeartbeatMs == 0 {
		deps.HeartbeatMs = 5000
	}
	return &Request{ID: id, state: "calling", deps: deps, ctx: ctx, cancelFn: cancel}
}

// State is the current state.
func (r *Request) State() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *Request) setState(s string) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
}

// SessionID is the agent session ("" when none).
func (r *Request) SessionID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessionID
}

// TurnNo is the number of the latest turn.
func (r *Request) TurnNo() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnNo
}

// PendingAsk is the open question and its turn, if any.
func (r *Request) PendingAsk() ([]AskQuestion, int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ask == nil {
		return nil, 0, false
	}
	return r.ask.questions, r.ask.turnNo, true
}

// Finished: done, failed or cancelled.
func (r *Request) Finished() bool {
	s := r.State()
	return s == "done" || s == "failed" || s == "cancelled"
}

func (r *Request) emit(ev Event) { r.deps.Emit(ev, r.TurnNo()) }

func (r *Request) cancelled() bool { return r.ctx.Err() != nil }

func (r *Request) fail(reason, message string) error {
	r.setState("failed")
	r.emit(Event{Type: "failed", Reason: reason, Message: message})
	return errStop
}

// guard runs fn, turning its error into the matching end state.
func (r *Request) guard(fn func() error) {
	r.work.Add(1)
	defer r.work.Done()
	err := fn()
	if err == nil || errors.Is(err, errStop) {
		return
	}
	var ob *OutboundBlocked
	if errors.As(err, &ob) {
		r.setState("failed")
		r.emit(Event{Type: "failed", Reason: "outbound_blocked", Message: ob.Msg})
		return
	}
	if r.cancelled() {
		return
	}
	r.setState("failed")
	r.emit(Event{Type: "failed", Reason: "error", Message: err.Error()})
}

// Run starts the request and returns when it waits for the user or ends.
func (r *Request) Run(start StartInput) {
	r.start = &start
	r.mu.Lock()
	r.sessionID = start.ResumeSessionID
	r.mu.Unlock()
	r.emit(Event{Type: "request_started"})
	r.guard(func() error {
		rec := start.Recovery
		if start.ResumeSessionID != "" {
			rec = ""
		}
		input, err := r.deps.Outbound.Request(start.Text, start.Current, rec)
		if err != nil {
			return err
		}
		return r.drive(input)
	})
}

// Answer is accepted only for the current question's turn; false when not accepted.
func (r *Request) Answer(turnNo int, given map[string]string) bool {
	next, ok := r.AnswerStart(turnNo, given)
	if ok {
		next()
	}
	return ok
}

// AnswerStart accepts an answer and returns the continuation to run (the loop until the next wait or end).
func (r *Request) AnswerStart(turnNo int, given map[string]string) (func(), bool) {
	r.mu.Lock()
	if r.state != "waiting_user" || r.ask == nil || r.ask.turnNo != turnNo {
		r.mu.Unlock()
		return nil, false
	}
	ask := r.ask
	r.ask = nil
	r.state = "calling"
	r.mu.Unlock()
	return func() {
		r.guard(func() error {
			input, err := r.deps.Outbound.Answers(ask.questions, given)
			if err != nil {
				return err
			}
			r.emit(Event{Type: "answers", Answers: AnswerValues(ask.questions, given)})
			return r.drive(input)
		})
	}, true
}

// ApproveOffdict: approve shows the preview, reject has it rebuilt from a dictionary metric.
func (r *Request) ApproveOffdict(turnNo int, approve bool) bool {
	next, ok := r.ApproveOffdictStart(turnNo, approve)
	if ok {
		next()
	}
	return ok
}

// ApproveOffdictStart accepts the decision and returns the continuation to run.
func (r *Request) ApproveOffdictStart(turnNo int, approve bool) (func(), bool) {
	r.mu.Lock()
	p := r.offdict
	if r.state != "waiting_user" || p == nil || p.turnNo != turnNo {
		r.mu.Unlock()
		return nil, false
	}
	r.offdict = nil
	if !approve {
		r.state = "calling"
	}
	r.mu.Unlock()
	return func() {
		r.emit(Event{Type: "offdict_answer", Approve: approve})
		if approve {
			r.setState("done")
			r.emit(Event{Type: "preview", Spec: p.spec, Columns: p.columns, RealRows: p.rows, AgentRows: p.agentRows, Tables: p.tables})
			r.emit(Event{Type: "done"})
			return
		}
		r.guard(func() error {
			input, err := r.deps.Outbound.OffdictRejected()
			if err != nil {
				return err
			}
			return r.drive(input)
		})
	}, true
}

// PendingOffdict is the result waiting for approval, if any.
func (r *Request) PendingOffdict() (spec panels.Spec, columns []panels.Column, rows []panels.Row, tables []string, turnNo int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.offdict == nil {
		return panels.Spec{}, nil, nil, nil, 0, false
	}
	p := r.offdict
	return p.spec, p.columns, p.rows, p.tables, p.turnNo, true
}

// Cancel stops the request; while it runs, it returns after the work has stopped.
func (r *Request) Cancel() {
	r.mu.Lock()
	if r.state == "done" || r.state == "failed" || r.state == "cancelled" || r.state == "cancelling" {
		r.mu.Unlock()
		r.work.Wait()
		return
	}
	wasWaiting := r.state == "waiting_user"
	r.state = "cancelling"
	r.mu.Unlock()
	r.cancelFn()
	if !wasWaiting {
		r.work.Wait()
	}
	r.mu.Lock()
	r.state = "cancelled"
	r.ask = nil
	r.offdict = nil
	r.mu.Unlock()
	r.emit(Event{Type: "cancelled"})
}

func (r *Request) nextAction(firstInput string) (Action, error) {
	L := r.deps.Limits
	lang := r.deps.Lang
	input := firstInput
	processRetried, schemaRetried, resumeRecovered := false, false, false
	rateRetries := 0
	for {
		if r.cancelled() {
			return Action{}, errStop
		}
		if r.Turns >= L.MaxTurns {
			return Action{}, r.fail("turn_limit", i18n.In(lang, "Reached the call limit for one request ("+itoa(L.MaxTurns)+")", "한 요청의 호출 한도("+itoa(L.MaxTurns)+"회)에 도달했어요"))
		}
		remaining := L.RequestBudgetUsd - r.CostTotal
		if remaining < MinCallBudget {
			return Action{}, r.fail("request_budget", i18n.In(lang, "Reached the request budget", "요청 한도에 도달했어요"))
		}
		r.setState("calling")
		if r.fixPending {
			r.fixPending = false
			r.Fixes++
		}
		r.emit(Event{Type: "step", Kind: "calling", Text: i18n.In(lang, "The agent is thinking", "에이전트가 생각하는 중")})
		res := r.deps.Call(r.ctx, input, r.SessionID(), math.Min(L.CallBudgetUsd, remaining))
		r.Turns++
		r.mu.Lock()
		r.turnNo++
		r.mu.Unlock()
		r.CostTotal += res.CostUsd
		if r.cancelled() {
			return Action{}, errStop
		}
		if !res.OK {
			if res.SessionID != "" && (res.Type == "rate_limit" || res.Type == "error") && res.SessionID != r.SessionID() {
				r.mu.Lock()
				r.sessionID = res.SessionID
				r.mu.Unlock()
				r.emit(Event{Type: "session", SessionID: res.SessionID})
			}
			switch res.Type {
			case "cancelled":
				return Action{}, errStop
			case "isolation":
				r.deps.OnIsolationFailure()
				return Action{}, r.fail("isolation", i18n.In(lang, "Agent isolation check failed: the agent has been turned off", "에이전트 격리 점검 실패: 에이전트 기능을 껐어요"))
			case "budget":
				return Action{}, r.fail("call_budget", i18n.In(lang, "Reached the call budget", "호출 한도에 도달했어요"))
			case "rate_limit":
				if rateRetries < len(rateWaits) {
					wait := rateWaits[rateRetries]
					rateRetries++
					s := jsjsonNum(float64(wait) / 1000)
					r.emit(Event{Type: "step", Kind: "retry", Text: i18n.In(lang, "The API is busy. Retrying in "+s+"s", "API가 바빠요. "+s+"초 뒤 다시 시도")})
					r.deps.Sleep(r.ctx, wait)
					continue
				}
				return Action{}, r.fail("rate_limit", i18n.In(lang, "The API is still rate-limited or overloaded. Please try again later", "API 제한·과부하가 계속돼요. 잠시 뒤 다시 시도해 주세요"))
			case "resume":
				if !resumeRecovered && r.start != nil {
					resumeRecovered = true
					r.mu.Lock()
					r.sessionID = ""
					r.mu.Unlock()
					var err error
					if input, err = r.deps.Outbound.Request(r.start.Text, r.start.Current, r.start.Recovery); err != nil {
						return Action{}, err
					}
					r.emit(Event{Type: "step", Kind: "retry", Text: i18n.In(lang, "Could not resume the earlier session, continuing in a new one", "이전 세션을 이어갈 수 없어 새 세션으로 이어가요")})
					continue
				}
				return Action{}, r.fail("resume", i18n.In(lang, "Could not resume the session", "세션을 이어갈 수 없어요"))
			default:
				if !processRetried {
					processRetried = true
					r.emit(Event{Type: "step", Kind: "retry", Text: i18n.In(lang, "Call failed ("+res.Message+"), retrying", "호출 실패("+res.Message+"), 다시 시도")})
					continue
				}
				reason := "process"
				if res.Type == "timeout" {
					reason = "timeout"
				} else if res.Type == "error" {
					reason = "error"
				}
				return Action{}, r.fail(reason, res.Message)
			}
		}
		if res.SessionID != r.SessionID() {
			r.mu.Lock()
			r.sessionID = res.SessionID
			r.mu.Unlock()
			r.emit(Event{Type: "session", SessionID: res.SessionID})
		}
		// agent output (SQL included) with a secret-looking value is dropped before parsing
		structured := res.Structured
		if structured == jsjson.Undefined {
			structured = nil
		}
		raw := jsjson.MustStringify(structured)
		if sensitive.SecretShape(raw) != "" || sensitive.SecretAssignment(raw) {
			return Action{}, r.fail("sensitive", SensitiveMessage(lang))
		}
		a, err := ParseAction(res.Structured)
		if err == nil {
			return a, nil
		}
		var ae *ActionError
		if !errors.As(err, &ae) {
			return Action{}, err
		}
		if schemaRetried {
			return Action{}, r.fail("schema", i18n.In(lang, "The reply does not match the action schema: "+ae.Msg, "응답이 행동 스키마에 맞지 않아요: "+ae.Msg))
		}
		schemaRetried = true
		if input, err = r.deps.Outbound.SchemaMismatch(ae.Msg); err != nil {
			return Action{}, err
		}
	}
}

func jsjsonNum(f float64) string { return jsjson.Number(f) }

// withHeartbeat emits the step, then a progress step every HeartbeatMs while fn runs.
func (r *Request) withHeartbeat(kind, label string, fn func()) {
	t0 := time.Now()
	r.emit(Event{Type: "step", Kind: kind, Text: label})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(time.Duration(r.deps.HeartbeatMs) * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				el := float64(time.Since(t0).Milliseconds())
				r.emit(Event{Type: "step", Kind: kind, Text: label + " (" + jsjson.Number(math.Floor(el/1000+0.5)) + i18n.In(r.deps.Lang, "s", "초") + ")", Ms: &el})
			}
		}
	}()
	fn()
	close(stop)
	<-done
}

func (r *Request) drive(firstInput string) error {
	L := r.deps.Limits
	lang := r.deps.Lang
	input := firstInput
	for {
		action, err := r.nextAction(input)
		if err != nil {
			return err
		}
		if r.cancelled() {
			return errStop
		}
		if action.Kind == "keep" {
			var cur *panels.Spec
			if r.start != nil {
				cur = r.start.Current
			}
			if cur == nil {
				if input, err = r.deps.Outbound.SchemaMismatch("keep: there is no current preview panel"); err != nil {
					return err
				}
				continue
			}
			if r.deps.Keep != nil {
				if kept := r.deps.Keep(); kept != nil {
					r.emit(Event{Type: "plan", Text: action.Plan})
					r.emit(Event{Type: "step", Kind: "kept", Text: i18n.In(lang, "Keeping the current panel", "현재 패널을 그대로 유지")})
					r.setState("done")
					r.emit(Event{Type: "preview", Spec: *cur, Columns: kept.Columns, RealRows: kept.Real, AgentRows: kept.Agent, Tables: kept.Tables})
					r.emit(Event{Type: "done"})
					return nil
				}
			}
			// the stored result is out of date: run the same panel again
			action = Action{Kind: "panel", Plan: action.Plan, Panel: *cur}
		}
		switch action.Kind {
		case "ask":
			r.mu.Lock()
			r.ask = &pendingAsk{questions: action.Questions, turnNo: r.turnNo}
			r.state = "waiting_user"
			r.mu.Unlock()
			r.emit(Event{Type: "question", Questions: action.Questions})
			return nil
		case "refuse":
			r.setState("done")
			r.emit(Event{Type: "refused", Reason: action.Reason, Alternatives: action.Alternatives})
			r.emit(Event{Type: "done"})
			return nil
		case "probe":
			r.emit(Event{Type: "plan", Text: action.Plan})
			if r.Probes >= L.MaxProbes {
				return r.fail("probe_limit", i18n.In(lang, "Reached the probe limit ("+itoa(L.MaxProbes)+")", "탐색 한도("+itoa(L.MaxProbes)+"회)에 도달했어요"))
			}
			r.Probes++ // failures count too
			r.setState("querying")
			var res QueryResult
			r.withHeartbeat("querying", i18n.In(lang, "Running probe query: "+action.Purpose, "탐색 쿼리 실행: "+action.Purpose), func() { res = r.deps.Probe(r.ctx, action.SQL) })
			if r.cancelled() {
				return errStop
			}
			// an attempt to read sensitive data ends the request without returning anything to the agent
			if !res.OK && res.Kind == "sensitive" {
				return r.fail("sensitive", SensitiveMessage(lang))
			}
			if res.OK {
				more := ""
				if res.More {
					more = "+"
				}
				ms := res.Ms
				n := len(res.Rows)
				r.emit(Event{Type: "step", Kind: "query_done", Text: action.Purpose + ": " + itoa(n) + more + " " + i18n.In(lang, "rows", "행"), Ms: &ms, Rows: &n})
			} else {
				r.emit(Event{Type: "step", Kind: "query_done", Text: action.Purpose + ": " + i18n.In(lang, "failed", "실패") + " (" + res.Message + ")"})
			}
			if input, err = r.deps.Outbound.ProbeResult(res, L.MaxProbes-r.Probes); err != nil {
				return err
			}
		case "panel":
			r.emit(Event{Type: "plan", Text: action.Plan})
			r.setState("checking")
			var res PanelResult
			r.withHeartbeat("checking", i18n.In(lang, "Checking and running the panel", "패널 검사·실행"), func() { res = r.deps.Panel(r.ctx, action.Panel) })
			if r.cancelled() {
				return errStop
			}
			if res.OK {
				for _, row := range res.Real {
					for _, v := range row {
						if s, ok := v.(string); ok && (sensitive.SecretShape(s) != "" || sensitive.SecretAssignment(s)) {
							return r.fail("sensitive", SensitiveMessage(lang))
						}
					}
				}
			}
			// all-zero numbers usually mean a join or format mistake: ask once, accept the same result if sent again
			if res.OK && !r.zeroWarned && AllZero(res.Real) {
				r.zeroWarned = true
				r.emit(Event{Type: "step", Kind: "check_failed", Text: i18n.In(lang, "All result numbers are 0 → asking to check the conditions", "결과 숫자가 모두 0 → 조건 확인 요청")})
				if input, err = r.deps.Outbound.ZeroResult(); err != nil {
					return err
				}
				continue
			}
			if res.OK {
				if action.Panel.Metric == nil {
					r.mu.Lock()
					r.offdict = &pendingOffdict{spec: action.Panel, columns: res.Columns, rows: res.Real, agentRows: res.Agent, tables: res.Tables, turnNo: r.turnNo}
					r.state = "waiting_user"
					r.mu.Unlock()
					r.emit(Event{Type: "offdict", Spec: action.Panel, Columns: res.Columns, RealRows: res.Real, Tables: res.Tables})
					return nil
				}
				r.setState("done")
				r.emit(Event{Type: "preview", Spec: action.Panel, Columns: res.Columns, RealRows: res.Real, AgentRows: res.Agent, Tables: res.Tables})
				r.emit(Event{Type: "done"})
				return nil
			}
			if res.Stage == "cancelled" {
				return errStop
			}
			if res.Stage == "sensitive" {
				return r.fail("sensitive", SensitiveMessage(lang))
			}
			r.emit(Event{Type: "step", Kind: "check_failed", Text: i18n.In(lang, "Panel check failed ("+res.Stage+") → fixing", "패널 검사 실패("+res.Stage+") → 수정 중")})
			if r.Fixes >= L.MaxFixes {
				return r.fail("fix_limit", i18n.In(lang, "Reached the panel fix limit ("+itoa(L.MaxFixes)+"): "+res.Message, "패널 수정 한도("+itoa(L.MaxFixes)+"회)에 도달했어요: "+res.Message))
			}
			r.fixPending = true
			if input, err = r.deps.Outbound.PanelFailure(res, L.MaxFixes-r.Fixes-1); err != nil {
				return err
			}
		}
	}
}
