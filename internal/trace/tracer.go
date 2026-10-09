package trace

import "time"

// Usage is the token usage of one agent call.
type Usage struct{ Input, Output, CacheRead, CacheWrite float64 }

// Call is what the tracer needs from one agent call.
type Call struct {
	OK      bool
	RunMs   float64
	APIMs   *float64
	Usage   *Usage
	CostUSD float64
	// Action is step.action of a structured reply (any value; only ask/refuse/probe/panel are recorded)
	Action any
	// ErrType is the failure type when !OK
	ErrType string
}

// Tracer reports each step of one request to a sink.
type Tracer struct {
	requestID string
	conv      *string
	provider  string
	model     *string
	sink      func(Span)
	now       func() time.Time

	turns, probes, panels float64
	start                 time.Time
	waited                float64
	waiting               *struct {
		since time.Time
		wait  string
	}
	outcome *string
}

// NewTracer starts tracing one request; now defaults to time.Now.
func NewTracer(requestID string, conv *string, provider string, model *string, sink func(Span), now func() time.Time) *Tracer {
	if now == nil {
		now = time.Now
	}
	return &Tracer{requestID: requestID, conv: conv, provider: provider, model: model, sink: sink, now: now, start: now()}
}

func (t *Tracer) base(kind string, ms float64, ok bool) Span {
	return Span{T: t.now().UTC().Format("2006-01-02T15:04:05.000Z"), RequestID: t.requestID, Conv: t.conv, Span: kind, Ms: ms, OK: ok}
}

func msBetween(a, b time.Time) float64 { return float64(b.UnixMilli() - a.UnixMilli()) }

func f(v float64) *float64 { return &v }
func s(v string) *string   { return &v }

func (t *Tracer) endWait() {
	if t.waiting == nil {
		return
	}
	ms := msBetween(t.waiting.since, t.now())
	t.waited += ms
	sp := t.base("wait_user", ms, true)
	sp.Wait = s(t.waiting.wait)
	t.sink(sp)
	t.waiting = nil
}

func (t *Tracer) finish(ok bool, outcome, reason string) {
	end := t.now()
	if t.waiting != nil {
		end = t.waiting.since
	}
	active := msBetween(t.start, end) - t.waited
	t.endWait()
	sp := t.base("request", active, ok)
	sp.Outcome = s(outcome)
	if reason != "" {
		sp.Reason = s(reason)
	}
	sp.Turns, sp.Probes, sp.Panels = f(t.turns), f(t.probes), f(t.panels)
	t.sink(sp)
}

// Event observes a loop event: request_started, question, offdict, answers, offdict_answer, preview (offdict: metric is null),
// refused, done, failed (with reason), cancelled.
func (t *Tracer) Event(kind string, offdict bool, reason string) {
	switch kind {
	case "request_started":
		t.start = t.now()
	case "question":
		t.waiting = &struct {
			since time.Time
			wait  string
		}{t.now(), "ask"}
	case "offdict":
		t.waiting = &struct {
			since time.Time
			wait  string
		}{t.now(), "offdict"}
	case "answers", "offdict_answer":
		t.endWait()
	case "preview":
		if offdict {
			t.outcome = s("offdict_preview")
		} else {
			t.outcome = s("preview")
		}
	case "refused":
		t.outcome = s("refused")
	case "done":
		o := "done"
		if t.outcome != nil {
			o = *t.outcome
		}
		t.finish(true, o, "")
	case "failed":
		t.finish(false, "failed", reason)
	case "cancelled":
		t.finish(false, "cancelled", "")
	}
}

var actions = map[string]bool{"ask": true, "refuse": true, "probe": true, "panel": true, "keep": true}

// AgentCall records one agent call that started at t0.
func (t *Tracer) AgentCall(t0 time.Time, c Call) {
	t.turns++
	sp := t.base("llm", msBetween(t0, t.now()), c.OK)
	sp.Turn, sp.Provider, sp.Model, sp.HasModel, sp.RunMs = f(t.turns), s(t.provider), t.model, true, f(c.RunMs)
	sp.APIMs = c.APIMs
	if c.Usage != nil {
		sp.In, sp.Out, sp.CacheRead, sp.CacheWrt = f(c.Usage.Input), f(c.Usage.Output), f(c.Usage.CacheRead), f(c.Usage.CacheWrite)
	}
	sp.CostUSD = f(c.CostUSD)
	if a, ok := c.Action.(string); ok && actions[a] {
		sp.Action = s(a)
	}
	if !c.OK {
		sp.Error = s(c.ErrType)
	}
	t.sink(sp)
}

// Probe records one probe query that started at t0 (rows when ok, otherwise the failure kind).
func (t *Tracer) Probe(t0 time.Time, ok bool, rows int, kind, sql string) {
	t.probes++
	sp := t.base("probe_sql", msBetween(t0, t.now()), ok)
	if ok {
		sp.Rows = f(float64(rows))
	} else {
		sp.Stage = s(kind)
	}
	sp.SQL = s(sql)
	t.sink(sp)
}

// Panel records one panel run that started at t0 (real rows when ok, otherwise the failed stage).
func (t *Tracer) Panel(t0 time.Time, ok bool, rows int, stage, sql string) {
	t.panels++
	sp := t.base("panel_run", msBetween(t0, t.now()), ok)
	if ok {
		sp.Rows = f(float64(rows))
	} else {
		sp.Stage = s(stage)
	}
	sp.SQL = s(sql)
	t.sink(sp)
}
