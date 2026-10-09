package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"growth-lab/internal/agent"
	"growth-lab/internal/auth"
	"growth-lab/internal/i18n"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/query"
	"growth-lab/internal/sensitive"
	"growth-lab/internal/trace"
)

// IDRE is the form of conversation, request and panel ids.
var IDRE = regexp.MustCompile(`^[a-z0-9]{12}$`)

var nonID = regexp.MustCompile(`[^a-z0-9]`)

// NewID is a random 12-character id.
func NewID() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	s := nonID.ReplaceAllString(strings.ToLower(base64.RawURLEncoding.EncodeToString(b)), "")
	for len(s) < 12 {
		s += "0"
	}
	return s[:12]
}

const eventBuffer = 500

// Event is one SSE event of a conversation.
type Event struct {
	ID        int
	Conv      string
	RequestID *string
	TurnNo    int
	Type      string
	Data      jsjson.Object
}

// JS is the event as the screen reads it.
func (e Event) JS() jsjson.Object {
	var rid any
	if e.RequestID != nil {
		rid = *e.RequestID
	}
	return jsjson.Object{{Key: "event_id", Value: e.ID}, {Key: "conversation_id", Value: e.Conv}, {Key: "request_id", Value: rid}, {Key: "turn_no", Value: e.TurnNo}, {Key: "type", Value: e.Type}, {Key: "data", Value: e.Data}}
}

// Preview is a finished preview: Obj as sent and stored, plus the parsed parts the server uses.
type Preview struct {
	Obj        jsjson.Object
	RequestID  string
	Hash       string
	SnapshotID string
	AsOf       string
	Spec       panels.Spec
	Columns    []panels.Column
	Rows       []panels.Row
	Tables     []string
	Versions   jsjson.Object
}

// IsSensitiveInput refuses without calling the agent; earlier are the conversation's previous inputs.
func IsSensitiveInput(text string, earlier []string) bool {
	return sensitive.Topic(text) != "" || sensitive.Topic(strings.Join(append(append([]string{}, earlier...), text), " ")) != ""
}

// LoopInput are what one request's loop needs.
type LoopInput struct {
	Snap         *Snapshot
	SystemPrompt string
	Metrics      panels.MetricDict
	Outbound     *agent.Outbound
	Emit         func(ev agent.Event, turnNo int)
	Tracer       *trace.Tracer
}

func toAgentQuery(r query.Result) agent.QueryResult {
	rows := make([][]agent.Tagged, len(r.Rows))
	for i, row := range r.Rows {
		rows[i] = make([]agent.Tagged, len(row))
		for j, t := range row {
			rows[i][j] = agent.Tagged{Tag: t.Tag, Value: t.Value}
		}
	}
	return agent.QueryResult{OK: r.OK, Columns: panels.ToColumns(r.Columns), Rows: rows, More: r.More, TruncatedCells: r.TruncatedCells, Ms: r.Ms, Tables: r.Tables, Kind: r.Kind, Message: r.Message}
}

func actionOf(structured any) any {
	o, ok := structured.(jsjson.Object)
	if !ok {
		return nil
	}
	step, _ := o.Get("step")
	so, ok := step.(jsjson.Object)
	if !ok {
		return nil
	}
	a, _ := so.Get("action")
	return a
}

// MakeLoopDeps connects one request's loop to the agent, the query worker and the panel runner (shared by conversations and eval).
func MakeLoopDeps(a *App, in LoopInput) agent.LoopDeps {
	cfg := a.WS.Config
	snap := in.Snap
	blocked := a.Blocked()
	cancelled := i18n.Tr("Cancelled", "취소됨")
	t := in.Tracer
	return agent.LoopDeps{
		Call: func(ctx context.Context, input, sessionID string, budget float64) agent.CallResult {
			t0 := time.Now()
			var res agent.CallResult
			err := a.Slots.Run(ctx, query.Interactive, func(*query.SlotLease) error {
				res = a.Runner.Call(agent.CallOptions{Input: input, SessionID: sessionID, BudgetUsd: budget, Ctx: ctx, SystemPrompt: in.SystemPrompt,
					JSONSchema: agent.ActionSchemaArg, Model: model(cfg.Agent), TimeoutMs: cfg.Agent.CallTimeoutMs})
				return nil
			})
			if err != nil {
				res = agent.CallResult{Type: "cancelled", Message: cancelled}
			}
			if t != nil {
				c := trace.Call{OK: res.OK, RunMs: res.Ms, APIMs: res.APIMs, CostUSD: res.CostUsd, Action: actionOf(res.Structured), ErrType: res.Type}
				if res.Usage != nil {
					c.Usage = &trace.Usage{Input: res.Usage.Input, Output: res.Usage.Output, CacheRead: res.Usage.CacheRead, CacheWrite: res.Usage.CacheWrite}
				}
				t.AgentCall(t0, c)
			}
			return res
		},
		Probe: func(ctx context.Context, sql string) agent.QueryResult {
			t0 := time.Now()
			var r query.Result
			err := a.Slots.Run(ctx, query.Interactive, func(l *query.SlotLease) error {
				r = query.RunQuery(query.Request{Lease: l, SQL: sql, Path: snap.Agent, Mode: query.Probe, AsOf: snap.AsOf, Params: cfg.Params,
					ReadablePrefixes: cfg.ReadablePrefixes, Blocked: &blocked, HeapLimitMb: float64(cfg.HeapLimitMb), Ctx: ctx})
				return nil
			})
			if err != nil {
				r = query.Result{Kind: "cancelled", Message: cancelled}
			}
			if t != nil {
				t.Probe(t0, r.OK, len(r.Rows), r.Kind, sql)
			}
			return toAgentQuery(r)
		},
		Panel: func(ctx context.Context, spec panels.Spec) agent.PanelResult {
			t0 := time.Now()
			var r panels.RunResult
			err := a.Slots.Run(ctx, query.Interactive, func(l *query.SlotLease) error {
				r = panels.Run(panels.RunInput{Spec: spec, RealPath: snap.Real, AgentPath: snap.Agent, AsOf: snap.AsOf, Params: cfg.Params, PanelPrefix: cfg.PanelPrefixes,
					Metrics: in.Metrics, Blocked: blocked, HeapLimitMb: float64(cfg.HeapLimitMb), Roles: a.Roles, Lease: l, Ctx: ctx})
				return nil
			})
			if err != nil {
				r = panels.RunResult{Stage: "cancelled", Message: cancelled}
			}
			if t != nil {
				t.Panel(t0, r.OK, len(r.Real), r.Stage, spec.SQL)
			}
			return agent.PanelResult{OK: r.OK, Columns: r.Columns, Real: r.Real, Agent: r.Agent, Ms: r.Ms, Tables: r.Tables, Stage: r.Stage, Message: r.Message, Violations: r.Violations}
		},
		Sleep: func(ctx context.Context, ms int) {
			select {
			case <-time.After(time.Duration(ms) * time.Millisecond):
			case <-ctx.Done():
			}
		},
		Outbound: in.Outbound,
		Limits:   agent.Limits{MaxTurns: cfg.Agent.MaxTurns, MaxProbes: cfg.Agent.MaxProbes, MaxFixes: cfg.Agent.MaxFixes, CallBudgetUsd: cfg.Agent.CallBudgetUsd, RequestBudgetUsd: cfg.Agent.RequestBudget},
		Emit: func(ev agent.Event, turnNo int) {
			if t != nil {
				t.Event(ev.Type, ev.Type == "preview" && ev.Spec.Metric == nil, ev.Reason)
			}
			in.Emit(ev, turnNo)
		},
		OnIsolationFailure: func() {
			a.DisableAgent(i18n.Tr("Agent isolation check failed: the agent is unavailable until the server restarts", "에이전트 격리 점검 실패: 서버를 다시 시작하기 전까지 에이전트를 쓸 수 없어요"))
		},
		Lang: a.Lang,
	}
}

type active struct {
	id        string
	cancelled bool
	req       *agent.Request
	work      sync.WaitGroup
}

// subscriber receives events without blocking the conversation.
type subscriber struct {
	mu     sync.Mutex
	queue  []Event
	notify chan struct{}
}

func (s *subscriber) push(e Event) {
	s.mu.Lock()
	s.queue = append(s.queue, e)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Take returns the queued events.
func (s *subscriber) Take() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queue
	s.queue = nil
	return q
}

// Notify fires when events are queued.
func (s *subscriber) Notify() <-chan struct{} { return s.notify }

// Conversation is one conversation: its requests, events and log file.
type Conversation struct {
	ID   string
	app  *App
	file string

	mu          sync.Mutex
	nextEventID int
	buffer      []Event
	subs        map[*subscriber]bool

	request    *agent.Request
	requestID  string
	act        *active
	queued     *struct{ id, text string }
	cancelling chan struct{}

	preview      *Preview
	previewAgent []panels.Row
	session      *struct{ id, contextVersion string }
	inputs       []string
	lastAsk      *agent.LastAsk
	askOrder     []string
	lastPlan     *string
	snap         *Snapshot
	ctx          *panels.ContextVersion
	// Origin is the panel this conversation regenerates
	Origin *struct{ PanelID, Prompt string }
	Owner  *string
	// Device is the hashed browser id the conversation was started from (nil in older logs)
	Device   *string
	Drafting bool
	locked   bool
	restart  jsjson.Object
}

func newConversation(app *App, id, dir string) *Conversation {
	return &Conversation{ID: id, app: app, file: filepath.Join(dir, id+".jsonl"), nextEventID: 1, subs: map[*subscriber]bool{}}
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return jsjson.MustStringify(v)
}

func parseQuestions(v any) []agent.AskQuestion {
	arr, _ := v.([]any)
	out := make([]agent.AskQuestion, 0, len(arr))
	for _, x := range arr {
		o, _ := x.(jsjson.Object)
		q := agent.AskQuestion{}
		if s, ok := getKey(o, "id").(string); ok {
			q.ID = s
		}
		if s, ok := getKey(o, "text").(string); ok {
			q.Text = s
		}
		if b, ok := getKey(o, "allow_free_text").(bool); ok {
			q.AllowFreeText = b
		}
		opts, _ := getKey(o, "options").([]any)
		for _, y := range opts {
			oo, _ := y.(jsjson.Object)
			label, _ := getKey(oo, "label").(string)
			def, _ := getKey(oo, "is_default").(bool)
			q.Options = append(q.Options, agent.Option{Label: label, IsDefault: def})
		}
		out = append(out, q)
	}
	return out
}

func getKey(o jsjson.Object, k string) any {
	v, _ := o.Get(k)
	return v
}

func parseRows(v any) []panels.Row {
	arr, _ := v.([]any)
	out := make([]panels.Row, len(arr))
	for i, r := range arr {
		cells, _ := r.([]any)
		out[i] = panels.Row(cells)
	}
	return out
}

func parseColumns(v any) []panels.Column {
	arr, _ := v.([]any)
	out := make([]panels.Column, len(arr))
	for i, x := range arr {
		o, _ := x.(jsjson.Object)
		c := panels.Column{}
		c.Name, _ = getKey(o, "name").(string)
		if s, ok := getKey(o, "table").(string); ok {
			c.Table = &s
		}
		if s, ok := getKey(o, "column").(string); ok {
			c.Column = &s
		}
		out[i] = c
	}
	return out
}

func stringList(v any) []string {
	arr, _ := v.([]any)
	out := []string{}
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// parsePreview reads a stored preview.
func parsePreview(o jsjson.Object) (*Preview, error) {
	// the stored spec keeps the parsed display; the preview's display field has the input form
	raw, _ := getKey(o, "spec").(jsjson.Object)
	specIn := jsjson.Object{}
	for _, m := range raw {
		if m.Key == "display" {
			m.Value = getKey(o, "display")
		}
		specIn = append(specIn, m)
	}
	spec, err := panels.ParseSpec(specIn)
	if err != nil {
		return nil, err
	}
	s := func(k string) string { x, _ := getKey(o, k).(string); return x }
	versions, _ := getKey(o, "versions").(jsjson.Object)
	return &Preview{Obj: o, RequestID: s("request_id"), Hash: s("preview_hash"), SnapshotID: s("snapshot_id"), AsOf: s("as_of"), Spec: spec,
		Columns: parseColumns(getKey(o, "columns")), Rows: parseRows(getKey(o, "rows")), Tables: stringList(getKey(o, "tables")), Versions: versions}, nil
}

// load restores the conversation from its log.
func (c *Conversation) load() {
	b, err := os.ReadFile(c.file)
	if err != nil {
		return
	}
	failedRequest := ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		v, err := jsjson.Parse(line)
		if err != nil {
			continue
		}
		e, ok := v.(jsjson.Object)
		if !ok {
			continue
		}
		typ, _ := getKey(e, "type").(string)
		switch typ {
		case "user_input":
			c.inputs = append(c.inputs, str(getKey(e, "text")))
		case "question":
			c.lastAsk = &agent.LastAsk{Questions: parseQuestions(getKey(e, "questions")), Answers: map[string]string{}}
			c.askOrder = nil
		case "answers":
			if c.lastAsk != nil {
				arr, _ := getKey(e, "answers").([]any)
				for _, x := range arr {
					o, _ := x.(jsjson.Object)
					c.setAnswer(str(getKey(o, "id")), str(getKey(o, "answer")))
				}
			}
		case "session":
			c.session = &struct{ id, contextVersion string }{str(getKey(e, "session_id")), str(getKey(e, "context_version"))}
		case "session_discarded":
			c.session = nil
		case "locked":
			c.locked = true
			c.preview = nil
			c.previewAgent = nil
		case "preview":
			if o, ok := getKey(e, "preview").(jsjson.Object); ok {
				if p, err := parsePreview(o); err == nil {
					c.preview = p
					if _, isArr := getKey(e, "agent_rows").([]any); isArr {
						c.previewAgent = parseRows(getKey(e, "agent_rows"))
					} else {
						c.previewAgent = nil
					}
				}
			}
		case "owner":
			u := str(getKey(e, "user"))
			c.Owner = &u
			if d, ok := getKey(e, "device").(string); ok && d != "" {
				c.Device = &d
			}
		case "origin":
			c.Origin = &struct{ PanelID, Prompt string }{str(getKey(e, "panel_id")), str(getKey(e, "prompt"))}
		case "request_started":
			failedRequest = str(getKey(e, "request_id"))
		case "done", "failed", "cancelled":
			failedRequest = ""
		}
	}
	if failedRequest != "" {
		msg := i18n.Tr("The server restarted, so the running request was stopped", "서버가 다시 시작되어 진행 중이던 요청을 멈췄어요")
		c.record(jsjson.Object{{Key: "type", Value: "failed"}, {Key: "request_id", Value: failedRequest}, {Key: "reason", Value: "server_restart"}, {Key: "message", Value: msg}})
		c.restart = jsjson.Object{{Key: "request_id", Value: failedRequest}, {Key: "reason", Value: "server_restart"}, {Key: "message", Value: msg}}
	}
}

func (c *Conversation) setAnswer(id, answer string) {
	if _, ok := c.lastAsk.Answers[id]; !ok {
		c.askOrder = append(c.askOrder, id)
	}
	c.lastAsk.Answers[id] = answer
}

func (c *Conversation) record(e jsjson.Object) {
	line := jsjson.MustStringify(append(jsjson.Object{{Key: "t", Value: time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}}, e...)) + "\n"
	f, err := os.OpenFile(c.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		slog.Error("conversation log write failed", "err", err, "conversation", c.ID)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// HasInput reports whether anything was asked.
func (c *Conversation) HasInput() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inputs) > 0
}

// IsLocked: locked by a sensitive question.
func (c *Conversation) IsLocked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.locked
}

// SetOwner records the creator and the browser it was started from.
func (c *Conversation) SetOwner(user, device string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Owner = &user
	e := jsjson.Object{{Key: "type", Value: "owner"}, {Key: "user", Value: user}}
	if device != "" {
		c.Device = &device
		e = append(e, jsjson.Member{Key: "device", Value: device})
	}
	c.record(e)
}

// SetOrigin records the panel being regenerated.
func (c *Conversation) SetOrigin(panelID, prompt string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Origin = &struct{ PanelID, Prompt string }{panelID, prompt}
	c.record(jsjson.Object{{Key: "type", Value: "origin"}, {Key: "panel_id", Value: panelID}, {Key: "prompt", Value: prompt}})
}

// FirstPrompt is the original request text of the panel to save.
func (c *Conversation) FirstPrompt() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Origin != nil {
		return c.Origin.Prompt
	}
	if len(c.inputs) > 0 {
		return c.inputs[0]
	}
	return ""
}

// OwnedBy: own conversations from the same browser (older ones without a device: any browser); ownerless legacy ones belong to admins.
func (c *Conversation) OwnedBy(u auth.User, device string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Owner == nil {
		return u.Role == "admin"
	}
	return *c.Owner == u.Username && (c.Device == nil || *c.Device == device)
}

// CompletedPreview: only when the request id and semantic hash match the current finished preview and nothing runs after it.
func (c *Conversation) CompletedPreview(requestID, hash string) (*Preview, []panels.Row, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.preview
	if c.locked || p == nil || p.RequestID != requestID || p.Hash != hash || c.act != nil || c.queued != nil {
		return nil, nil, false
	}
	return p, c.previewAgent, true
}

// Subscribe registers a listener; replay holds events after lastEventID, or resync is true when they are gone.
func (c *Conversation) Subscribe(lastEventID int, hasLast bool) (sub *subscriber, replay []Event, resync bool, unsubscribe func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sub = &subscriber{notify: make(chan struct{}, 1)}
	c.subs[sub] = true
	if hasLast {
		oldest := c.nextEventID
		if len(c.buffer) > 0 {
			oldest = c.buffer[0].ID
		}
		if lastEventID+1 < oldest {
			resync = true
		} else {
			for _, e := range c.buffer {
				if e.ID > lastEventID {
					replay = append(replay, e)
				}
			}
		}
	}
	return sub, replay, resync, func() {
		c.mu.Lock()
		delete(c.subs, sub)
		c.mu.Unlock()
	}
}

// push must be called with c.mu held.
func (c *Conversation) push(typ, requestID string, turnNo int, data jsjson.Object) {
	var rid *string
	if requestID != "" {
		r := requestID
		rid = &r
	}
	e := Event{ID: c.nextEventID, Conv: c.ID, RequestID: rid, TurnNo: turnNo, Type: typ, Data: data}
	c.nextEventID++
	c.buffer = append(c.buffer, e)
	if len(c.buffer) > eventBuffer {
		c.buffer = c.buffer[1:]
	}
	for s := range c.subs {
		s.push(e)
	}
}

func (c *Conversation) offdictView(spec panels.Spec, columns []panels.Column, rows []panels.Row, tables []string) jsjson.Object {
	caveats, _ := panels.EffectiveCaveats(spec, columns, rows, c.app.Lang)
	if caveats == nil {
		caveats = []string{}
	}
	return jsjson.Object{{Key: "spec", Value: spec.JS()}, {Key: "tables", Value: tables}, {Key: "caveats", Value: caveats}}
}

// State is the conversation state for the screen.
func (c *Conversation) State() jsjson.Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	var request, question, offdict, preview, failed any
	if c.request != nil {
		r := c.request
		request = jsjson.Object{{Key: "request_id", Value: c.requestID}, {Key: "state", Value: r.State()}, {Key: "turn_no", Value: r.TurnNo()}}
		if r.State() == "waiting_user" {
			if qs, turn, ok := r.PendingAsk(); ok {
				arr := make([]any, len(qs))
				for i, q := range qs {
					arr[i] = q.JS()
				}
				question = jsjson.Object{{Key: "request_id", Value: c.requestID}, {Key: "turn_no", Value: turn}, {Key: "questions", Value: arr}}
			}
			if spec, cols, rows, tables, turn, ok := r.PendingOffdict(); ok {
				offdict = append(jsjson.Object{{Key: "request_id", Value: c.requestID}, {Key: "turn_no", Value: turn}}, c.offdictView(spec, cols, rows, tables)...)
			}
		}
	} else if c.restart != nil {
		failed = c.restart
	}
	if c.preview != nil {
		preview = c.preview.Obj
	}
	return jsjson.Object{{Key: "conversation_id", Value: c.ID}, {Key: "request", Value: request}, {Key: "question", Value: question}, {Key: "offdict", Value: offdict},
		{Key: "preview", Value: preview}, {Key: "locked", Value: c.locked}, {Key: "failed", Value: failed}, {Key: "last_event_id", Value: c.nextEventID - 1}}
}

// cancelActive stops the active request; the returned channel closes when it has stopped. Called with c.mu held.
func (c *Conversation) cancelActive(message string) chan struct{} {
	if c.cancelling != nil {
		return c.cancelling
	}
	a := c.act
	done := make(chan struct{})
	if a.req != nil && a.req.Finished() {
		c.act = nil
		close(done)
		return done
	}
	a.cancelled = true
	req := a.req
	c.cancelling = done
	go func() {
		if req != nil {
			req.Cancel()
		}
		a.work.Wait()
		c.mu.Lock()
		turn := 0
		if req != nil {
			turn = req.TurnNo()
		}
		c.record(jsjson.Object{{Key: "type", Value: "cancelled"}, {Key: "request_id", Value: a.id}})
		c.push("cancelled", a.id, turn, jsjson.Object{{Key: "message", Value: message}})
		if c.act == a {
			c.act = nil
		}
		c.cancelling = nil
		c.mu.Unlock()
		close(done)
	}()
	return done
}

// earlier are all inputs of this conversation, for questions split across turns. Called with c.mu held.
func (c *Conversation) earlier() []string {
	out := append([]string{}, c.inputs...)
	if c.lastAsk != nil {
		for _, k := range c.askOrder {
			out = append(out, c.lastAsk.Answers[k])
		}
	}
	return out
}

func (c *Conversation) notRecorded() jsjson.Object {
	return jsjson.Object{{Key: "text", Value: i18n.Tr("(request not recorded)", "(기록하지 않은 요청)")}}
}

// refuseSensitive locks the conversation without calling the agent; the input is not logged. Called with c.mu held.
func (c *Conversation) refuseSensitive(id string) {
	c.locked = true
	c.session = nil
	c.preview = nil
	c.previewAgent = nil
	c.record(jsjson.Object{{Key: "type", Value: "locked"}, {Key: "request_id", Value: id}})
	if c.app.Audit != nil {
		c.app.Audit.TryWrite(auth.AuditRecord{Event: "sensitive_blocked", User: c.Owner, Target: &c.ID})
	}
	c.push("user_input", id, 0, c.notRecorded())
	c.push("refused", id, 0, jsjson.Object{{Key: "reason", Value: i18n.Tr("This tool does not handle credentials, tokens, connection details, settings or personal contacts. This conversation is locked. Please ask about metrics in a new conversation.", "이 도구는 인증 정보·토큰·연결 정보·설정·개인 연락처를 다루지 않아요. 이 대화는 잠겼어요. 새 대화에서 지표로 물어봐 주세요.")}, {Key: "alternatives", Value: []string{}}})
	c.push("done", id, 0, jsjson.Object{})
}

// Submit starts a request, cancelling a running one first; only the last waiting input is kept.
func (c *Conversation) Submit(text string) string {
	id := NewID()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locked || IsSensitiveInput(text, c.earlier()) {
		wasLocked := c.locked
		c.queued = nil
		finish := func() {
			if !wasLocked {
				c.refuseSensitive(id)
				return
			}
			c.push("user_input", id, 0, c.notRecorded())
			c.push("refused", id, 0, jsjson.Object{{Key: "reason", Value: i18n.Tr("This conversation is locked. Please open a new one.", "이 대화는 잠겼어요. 새 대화를 열어 주세요.")}, {Key: "alternatives", Value: []string{}}})
			c.push("done", id, 0, jsjson.Object{})
		}
		if c.act != nil {
			done := c.cancelActive(i18n.Tr("Cancelled the previous request", "이전 요청을 취소했습니다"))
			go func() {
				<-done
				c.mu.Lock()
				finish()
				c.mu.Unlock()
			}()
		} else {
			finish()
		}
		return id
	}
	if c.act != nil {
		c.queued = &struct{ id, text string }{id, text}
		c.push("queued", id, 0, jsjson.Object{{Key: "text", Value: text}})
		done := c.cancelActive(i18n.Tr("Cancelled the previous request", "이전 요청을 취소했습니다"))
		go func() {
			<-done
			c.mu.Lock()
			q := c.queued
			c.queued = nil
			if q != nil && c.act == nil {
				c.begin(q.id, q.text)
			}
			c.mu.Unlock()
		}()
		return id
	}
	c.begin(id, text)
	return id
}

// begin runs a request in the background. Called with c.mu held.
func (c *Conversation) begin(id, text string) {
	a := &active{id: id}
	c.act = a
	c.request = nil
	c.segment(a, func() { c.start(a, text) })
}

// segment runs part of a request; when it ends with the request finished, the conversation is free again.
func (c *Conversation) segment(a *active, fn func()) {
	a.work.Add(1)
	go func() {
		defer a.work.Done()
		fn()
		c.mu.Lock()
		if c.act == a && !a.cancelled && (a.req == nil || a.req.Finished()) {
			c.act = nil
		}
		c.mu.Unlock()
	}()
}

// Stop cancels the running request; false when none.
func (c *Conversation) Stop() bool {
	c.mu.Lock()
	c.queued = nil
	if c.act == nil {
		c.mu.Unlock()
		return false
	}
	done := c.cancelActive(i18n.Tr("Request stopped", "요청을 멈췄어요"))
	c.mu.Unlock()
	<-done
	return true
}

// Answer takes the answers to the open question; false when not expected.
func (c *Conversation) Answer(requestID string, turnNo int, answers map[string]string, order []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.act
	if a == nil || a.id != requestID || a.cancelled || a.req == nil {
		return false
	}
	var free []string
	for _, k := range order {
		if v, ok := answers[k]; ok {
			free = append(free, v)
		}
	}
	if len(free) > 0 && IsSensitiveInput(strings.Join(free, " "), c.earlier()) {
		done := c.cancelActive(i18n.Tr("Request stopped", "요청을 멈췄어요"))
		go func() {
			<-done
			c.mu.Lock()
			c.refuseSensitive(NewID())
			c.mu.Unlock()
		}()
		return true
	}
	next, ok := a.req.AnswerStart(turnNo, answers)
	if !ok {
		return false
	}
	c.segment(a, next)
	return true
}

// Offdict takes the approval of an off-dictionary preview; false when not expected.
func (c *Conversation) Offdict(requestID string, turnNo int, approve bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.act
	if a == nil || a.id != requestID || a.cancelled || a.req == nil {
		return false
	}
	next, ok := a.req.ApproveOffdictStart(turnNo, approve)
	if !ok {
		return false
	}
	c.segment(a, next)
	return true
}

func (c *Conversation) start(a *active, text string) {
	app := c.app
	requestID := a.id
	c.mu.Lock()
	c.requestID = requestID
	c.inputs = append(c.inputs, text)
	c.record(jsjson.Object{{Key: "type", Value: "user_input"}, {Key: "request_id", Value: requestID}, {Key: "text", Value: text}})
	c.push("user_input", requestID, 0, jsjson.Object{{Key: "text", Value: text}})
	c.mu.Unlock()

	app.WaitAgent()
	fail := func(message string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.record(jsjson.Object{{Key: "type", Value: "failed"}, {Key: "request_id", Value: requestID}, {Key: "reason", Value: "unavailable"}, {Key: "message", Value: message}})
		c.push("failed", requestID, 0, jsjson.Object{{Key: "reason", Value: "unavailable"}, {Key: "message", Value: message}})
	}
	c.mu.Lock()
	stop := a.cancelled
	c.mu.Unlock()
	if stop {
		return
	}
	if st := app.AgentStatus(); st.State != "ok" {
		fail(st.Message)
		return
	}
	snap := app.Snapshot()
	if snap == nil {
		fail(i18n.Tr("No snapshot yet. Run collect first", "스냅샷이 없어요. 먼저 collect를 실행해 주세요"))
		return
	}
	ctx := app.ContextVersion(snap)
	ctxKey := ctx.Key()
	setupFailed := i18n.Tr("There is a problem with the agent setup (guide, metric dictionary or example panels). Please tell an admin", "에이전트 설정(설명서·지표 사전·예시 패널)에 문제가 있어요. 관리자에게 알려 주세요")
	dict, _, err := app.Metrics()
	var systemPrompt string
	if err == nil {
		systemPrompt, err = app.SystemPrompt(snap, ctx, false)
	}
	if err != nil {
		slog.Error("building the agent context failed", "err", err, "conversation", c.ID, "request", requestID)
		fail(setupFailed)
		return
	}
	cfg := app.WS.Config
	outbound := &agent.Outbound{Mode: cfg.Agent.DataMode, Roles: app.Roles}

	c.mu.Lock()
	c.snap = snap
	c.ctx = &ctx
	resume := ""
	if c.session != nil && c.session.contextVersion == ctxKey {
		resume = c.session.id
	}
	var current *panels.Spec
	var kept *agent.KeptResult
	if c.preview != nil {
		s := c.preview.Spec
		current = &s
		// the stored result can be shown again only for the same snapshot and context
		if c.previewAgent != nil && c.preview.SnapshotID == snap.ID && jsjson.MustStringify(c.preview.Versions) == jsjson.MustStringify(ContextJS(ctx)) {
			kept = &agent.KeptResult{Columns: c.preview.Columns, Real: c.preview.Rows, Agent: c.previewAgent, Tables: c.preview.Tables}
		}
	}
	recovery := ""
	if len(c.inputs) > 1 || c.preview != nil {
		recovery, err = outbound.RecoverySummary(c.inputs[:len(c.inputs)-1], c.lastAsk, current)
	}
	if c.session != nil && resume == "" {
		c.push("step", requestID, 0, jsjson.Object{{Key: "kind", Value: "retry"}, {Key: "text", Value: i18n.Tr("Data or guide changed; continuing in a new session", "데이터나 설명서가 바뀌어 새 세션으로 이어갑니다")}})
	}
	c.mu.Unlock()
	if err != nil {
		fail(err.Error())
		return
	}

	var tracer *trace.Tracer
	if app.Trace != nil {
		conv := c.ID
		tracer = trace.NewTracer(requestID, &conv, cfg.Agent.Provider, cfg.Agent.Model, func(s trace.Span) { app.Trace.Write(s) }, nil)
	}
	deps := MakeLoopDeps(app, LoopInput{Snap: snap, SystemPrompt: systemPrompt, Metrics: dict, Outbound: outbound, Tracer: tracer,
		Emit: func(ev agent.Event, turnNo int) { c.onLoopEvent(requestID, ev, turnNo, ctxKey) }})
	deps.Keep = func() *agent.KeptResult { return kept }
	req := agent.NewRequest(requestID, deps)
	c.mu.Lock()
	if a.cancelled {
		c.mu.Unlock()
		return
	}
	a.req = req
	c.request = req
	c.lastPlan = nil
	c.mu.Unlock()
	req.Run(agent.StartInput{Text: text, Current: current, ResumeSessionID: resume, Recovery: recovery})
}

func rowsJS(rs []panels.Row) []any {
	out := make([]any, len(rs))
	for i, r := range rs {
		out[i] = []any(r)
	}
	return out
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

func headlineJS(spec panels.Spec, columns []panels.Column, rows []panels.Row) any {
	h, err := panels.ComputeHeadline(spec, columns, rows)
	if err != nil || h == nil {
		var ce *panels.ContractError
		if err != nil && !errors.As(err, &ce) {
			slog.Warn("headline failed", "err", err)
		}
		return nil
	}
	return h.JS()
}

func (c *Conversation) onLoopEvent(requestID string, ev agent.Event, turnNo int, ctxKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if requestID != c.requestID {
		return
	}
	rec := func(fields ...jsjson.Member) {
		c.record(append(jsjson.Object{{Key: "type", Value: ev.Type}, {Key: "request_id", Value: requestID}}, fields...))
	}
	switch ev.Type {
	case "request_started":
		rec()
		c.push("request_started", requestID, turnNo, jsjson.Object{})
	case "session":
		c.session = &struct{ id, contextVersion string }{ev.SessionID, ctxKey}
		rec(jsjson.Member{Key: "session_id", Value: ev.SessionID}, jsjson.Member{Key: "context_version", Value: ctxKey})
	case "question":
		c.lastAsk = &agent.LastAsk{Questions: ev.Questions, Answers: map[string]string{}}
		c.askOrder = nil
		qs := make([]any, len(ev.Questions))
		for i, q := range ev.Questions {
			qs[i] = q.JS()
		}
		rec(jsjson.Member{Key: "questions", Value: qs})
		c.push("question", requestID, turnNo, jsjson.Object{{Key: "questions", Value: qs}})
	case "answers":
		as := make([]any, len(ev.Answers))
		for i, x := range ev.Answers {
			as[i] = x
			if c.lastAsk != nil {
				c.setAnswer(str(getKey(x, "id")), str(getKey(x, "answer")))
			}
		}
		rec(jsjson.Member{Key: "answers", Value: as})
		c.push("answers", requestID, turnNo, jsjson.Object{{Key: "answers", Value: as}})
	case "plan":
		t := ev.Text
		c.lastPlan = &t
		rec(jsjson.Member{Key: "text", Value: ev.Text})
		c.push("plan", requestID, turnNo, jsjson.Object{{Key: "text", Value: ev.Text}})
	case "preview":
		snap := c.snap
		caveats, _ := panels.EffectiveCaveats(ev.Spec, ev.Columns, ev.RealRows, c.app.Lang)
		if caveats == nil {
			caveats = []string{}
		}
		var plan any
		if c.lastPlan != nil {
			plan = *c.lastPlan
		}
		tables := ev.Tables
		if tables == nil {
			tables = []string{}
		}
		hash := panels.SemanticHash(ev.Spec, snap.ParamsHash)
		obj := jsjson.Object{
			{Key: "request_id", Value: requestID}, {Key: "spec", Value: ev.Spec.JS()}, {Key: "display", Value: panels.DisplayJSON(ev.Spec.Display)},
			{Key: "columns", Value: columnsJS(ev.Columns)}, {Key: "rows", Value: rowsJS(ev.RealRows)}, {Key: "caveats", Value: caveats},
			{Key: "headline", Value: headlineJS(ev.Spec, ev.Columns, ev.RealRows)}, {Key: "preview_hash", Value: hash}, {Key: "snapshot_id", Value: snap.ID},
			{Key: "as_of", Value: snap.AsOf}, {Key: "plan", Value: plan}, {Key: "versions", Value: ContextJS(*c.ctx)}, {Key: "tables", Value: tables},
		}
		if ev.Spec.Metric == nil {
			c.record(jsjson.Object{{Key: "type", Value: "offdict_approved"}, {Key: "request_id", Value: requestID}, {Key: "preview_hash", Value: hash}})
		}
		c.preview = &Preview{Obj: obj, RequestID: requestID, Hash: hash, SnapshotID: snap.ID, AsOf: snap.AsOf, Spec: ev.Spec, Columns: ev.Columns, Rows: ev.RealRows, Tables: tables, Versions: ContextJS(*c.ctx)}
		c.previewAgent = ev.AgentRows
		rec(jsjson.Member{Key: "preview", Value: obj}, jsjson.Member{Key: "agent_rows", Value: rowsJS(ev.AgentRows)})
		c.push("preview", requestID, turnNo, jsjson.Object{{Key: "preview", Value: obj}})
	case "offdict":
		rec(jsjson.Member{Key: "spec", Value: ev.Spec.JS()}, jsjson.Member{Key: "tables", Value: ev.Tables})
		c.push("offdict", requestID, turnNo, c.offdictView(ev.Spec, ev.Columns, ev.RealRows, ev.Tables))
	case "offdict_answer":
		if !ev.Approve {
			c.record(jsjson.Object{{Key: "type", Value: "offdict_rejected"}, {Key: "request_id", Value: requestID}})
		}
		c.push("offdict_answer", requestID, turnNo, jsjson.Object{{Key: "approve", Value: ev.Approve}})
	case "refused":
		alts := ev.Alternatives
		if alts == nil {
			alts = []string{}
		}
		rec(jsjson.Member{Key: "reason", Value: ev.Reason}, jsjson.Member{Key: "alternatives", Value: alts})
		c.push("refused", requestID, turnNo, jsjson.Object{{Key: "reason", Value: ev.Reason}, {Key: "alternatives", Value: alts}})
	case "failed":
		if ev.Reason == "sensitive" {
			c.session = nil
			c.record(jsjson.Object{{Key: "type", Value: "session_discarded"}, {Key: "request_id", Value: requestID}})
			if c.app.Audit != nil {
				c.app.Audit.TryWrite(auth.AuditRecord{Event: "sensitive_blocked", User: c.Owner, Target: &c.ID})
			}
		}
		rec(jsjson.Member{Key: "reason", Value: ev.Reason}, jsjson.Member{Key: "message", Value: ev.Message})
		c.push("failed", requestID, turnNo, jsjson.Object{{Key: "reason", Value: ev.Reason}, {Key: "message", Value: ev.Message}})
	case "done":
		rec()
		c.push("done", requestID, turnNo, jsjson.Object{})
	case "step":
		var ms, rows any
		if ev.Ms != nil {
			ms = *ev.Ms
		}
		if ev.Rows != nil {
			rows = *ev.Rows
		}
		c.push("step", requestID, turnNo, jsjson.Object{{Key: "kind", Value: ev.Kind}, {Key: "text", Value: ev.Text}, {Key: "ms", Value: ms}, {Key: "rows", Value: rows}})
	}
}

// Hub holds the conversations of the server.
type Hub struct {
	app   *App
	dir   string
	mu    sync.Mutex
	items map[string]*Conversation
}

// NewHub opens the conversations folder.
func NewHub(app *App) (*Hub, error) {
	dir := filepath.Join(app.WS.Config.OutDir, "conversations")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	return &Hub{app: app, dir: dir, items: map[string]*Conversation{}}, nil
}

// Create starts a conversation owned by a user on one browser.
func (h *Hub) Create(owner, device string) *Conversation {
	c := newConversation(h.app, NewID(), h.dir)
	c.SetOwner(owner, device)
	h.mu.Lock()
	h.items[c.ID] = c
	h.mu.Unlock()
	return c
}

// Get returns a conversation, loading it from its log, or nil.
func (h *Hub) Get(id string) *Conversation {
	if !IDRE.MatchString(id) {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.items[id]; ok {
		return c
	}
	if _, err := os.Stat(filepath.Join(h.dir, id+".jsonl")); err != nil {
		return nil
	}
	c := newConversation(h.app, id, h.dir)
	c.load()
	h.items[id] = c
	return c
}

// LatestFor is the user's latest conversation on this browser to continue: has input and is not locked (last 200 files).
func (h *Hub) LatestFor(u auth.User, device string) *string {
	entries, _ := os.ReadDir(h.dir)
	type f struct {
		id    string
		mtime time.Time
	}
	var files []f
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".jsonl") || !IDRE.MatchString(strings.TrimSuffix(n, ".jsonl")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, f{strings.TrimSuffix(n, ".jsonl"), info.ModTime()})
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	if len(files) > 200 {
		files = files[:200]
	}
	for _, x := range files {
		c := h.Get(x.id)
		if c == nil || !c.OwnedBy(u, device) {
			continue
		}
		if c.HasInput() && !c.IsLocked() {
			id := x.id
			return &id
		}
	}
	return nil
}

// StopAll cancels every running request.
func (h *Hub) StopAll() {
	h.mu.Lock()
	cs := make([]*Conversation, 0, len(h.items))
	for _, c := range h.items {
		cs = append(cs, c)
	}
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Stop()
		}()
	}
	wg.Wait()
}
