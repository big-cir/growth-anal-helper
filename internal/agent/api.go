package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"growth-lab/internal/jsstr"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/sensitive"
)

// Pricing is dollars per million tokens.
type Pricing struct{ InputPerMTok, OutputPerMTok float64 }

// DefaultBaseURLs per provider.
var DefaultBaseURLs = map[string]string{"anthropic": "https://api.anthropic.com", "openai": "https://api.openai.com/v1"}

const (
	toolName     = "respond"
	errorTextMax = 300
)

var sessionRE = regexp.MustCompile(`^api_[0-9a-f]{24}$`)

// APIOptions configure an API runner.
type APIOptions struct {
	Provider        string
	BaseURL         string // "" for the default
	APIKey          string
	Pricing         *Pricing
	MaxOutputTokens int
	SessionDir      string
	// Client replaces the HTTP client (tests)
	Client *http.Client
}

// APIRunner calls the Anthropic or an OpenAI-compatible API with no tools and JSON replies only.
// The engine stores the conversation and resends it each call.
type APIRunner struct{ o APIOptions }

// NewAPIRunner creates the session folder.
func NewAPIRunner(o APIOptions) (*APIRunner, error) {
	if err := os.MkdirAll(o.SessionDir, 0o700); err != nil {
		return nil, err
	}
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	return &APIRunner{o}, nil
}

type reply struct {
	ok         bool
	structured any
	typ, msg   string
	usage      Usage
}

// Call runs one API call.
func (a *APIRunner) Call(c CallOptions) CallResult {
	t0 := time.Now()
	ctx := c.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	fail := func(typ, msg string, cost float64, sid string, usage *Usage) CallResult {
		return CallResult{Type: typ, Message: msg, SessionID: sid, CostUsd: cost, Ms: msSinceRounded(t0), Usage: usage}
	}
	if ctx.Err() != nil {
		return fail("cancelled", "cancelled", 0, c.SessionID, nil)
	}
	if c.Model == "" {
		return fail("error", "agent.model is required", 0, c.SessionID, nil)
	}
	var history []any
	if c.SessionID != "" {
		h, ok := a.load(c.SessionID)
		if !ok {
			return fail("resume", "could not resume session", 0, "", nil)
		}
		history = h
	}
	messages := append(append([]any{}, history...), jsjson.Object{{Key: "role", Value: "user"}, {Key: "text", Value: c.Input}})
	tctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutMs)*time.Millisecond)
	defer cancel()
	var r reply
	var err error
	if a.o.Provider == "anthropic" {
		r, err = a.anthropic(tctx, c, messages)
	} else {
		r, err = a.openai(tctx, c, messages)
	}
	if err != nil {
		if ctx.Err() != nil {
			return fail("cancelled", "cancelled", 0, c.SessionID, nil)
		}
		if errors.Is(tctx.Err(), context.DeadlineExceeded) {
			return fail("timeout", fmt.Sprintf("call timed out (%dms)", c.TimeoutMs), 0, c.SessionID, nil)
		}
		return fail("process", "API connection failed: fetch failed", 0, c.SessionID, nil)
	}
	cost := a.cost(r.usage)
	usage := r.usage
	if !r.ok {
		return fail(r.typ, r.msg, cost, c.SessionID, &usage)
	}
	if cost > c.BudgetUsd {
		return fail("budget", "call budget reached", cost, c.SessionID, &usage)
	}
	id := c.SessionID
	if id == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		id = "api_" + hex.EncodeToString(b)
	}
	answer := jsjson.Object{{Key: "role", Value: "assistant"}}
	if r.structured != jsjson.Undefined {
		answer = append(answer, jsjson.Member{Key: "text", Value: jsjson.MustStringify(r.structured)})
	}
	if err := a.save(id, append(messages, answer)); err != nil {
		return fail("error", err.Error(), cost, c.SessionID, &usage)
	}
	return CallResult{OK: true, SessionID: id, Structured: r.structured, CostUsd: cost, Ms: msSinceRounded(t0), Usage: &usage}
}

func (a *APIRunner) cost(u Usage) float64 {
	p := a.o.Pricing
	if p == nil {
		return 0
	}
	return ((u.Input+u.CacheRead+u.CacheWrite)*p.InputPerMTok + u.Output*p.OutputPerMTok) / 1e6
}

func (a *APIRunner) base() string {
	b := a.o.BaseURL
	if b == "" {
		b = DefaultBaseURLs[a.o.Provider]
	}
	return strings.TrimSuffix(b, "/")
}

// apiMessages maps stored turns to { role, content }.
func apiMessages(turns []any) []any {
	out := make([]any, len(turns))
	for i, t := range turns {
		o, _ := t.(jsjson.Object)
		role, hasRole := o.Get("role")
		text, hasText := o.Get("text")
		m := jsjson.Object{}
		if hasRole {
			m = append(m, jsjson.Member{Key: "role", Value: role})
		} else {
			m = append(m, jsjson.Member{Key: "role", Value: jsjson.Undefined})
		}
		if hasText {
			m = append(m, jsjson.Member{Key: "content", Value: text})
		} else {
			m = append(m, jsjson.Member{Key: "content", Value: jsjson.Undefined})
		}
		out[i] = m
	}
	return out
}

func (a *APIRunner) post(ctx context.Context, url string, headers [][2]string, body jsjson.Object) (int, any, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader([]byte(jsjson.MustStringify(body))))
	if err != nil {
		return 0, nil, err
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	res, err := a.o.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, rerr := io.ReadAll(res.Body)
	var parsed any
	if rerr == nil {
		if v, perr := jsjson.Parse(string(data)); perr == nil {
			parsed = v
		}
	}
	return res.StatusCode, parsed, nil
}

func field(v any, k string) any {
	o, ok := v.(jsjson.Object)
	if !ok {
		return nil
	}
	return get(o, k)
}

func (a *APIRunner) anthropic(ctx context.Context, c CallOptions, messages []any) (reply, error) {
	schema, err := jsjson.Parse(c.JSONSchema)
	if err != nil {
		return reply{}, err
	}
	body := jsjson.Object{
		{Key: "model", Value: c.Model}, {Key: "max_tokens", Value: a.o.MaxOutputTokens}, {Key: "system", Value: c.SystemPrompt},
		{Key: "messages", Value: apiMessages(messages)},
		{Key: "tools", Value: []any{jsjson.Object{{Key: "name", Value: toolName}, {Key: "description", Value: "Give the answer in this format"}, {Key: "input_schema", Value: schema}}}},
		{Key: "tool_choice", Value: jsjson.Object{{Key: "type", Value: "tool"}, {Key: "name", Value: toolName}}},
	}
	status, res, err := a.post(ctx, a.base()+"/v1/messages", [][2]string{{"content-type", "application/json"}, {"x-api-key", a.o.APIKey}, {"anthropic-version", "2023-06-01"}}, body)
	if err != nil {
		return reply{}, err
	}
	u := field(res, "usage")
	usage := Usage{Input: numOr0(field(u, "input_tokens")), Output: numOr0(field(u, "output_tokens")), CacheRead: numOr0(field(u, "cache_read_input_tokens")), CacheWrite: numOr0(field(u, "cache_creation_input_tokens"))}
	if status < 200 || status > 299 {
		return httpError(status, res, usage), nil
	}
	if field(res, "stop_reason") == "max_tokens" {
		return reply{typ: "error", msg: "response cut off at the output limit (maxOutputTokens)", usage: usage}, nil
	}
	if blocks, ok := field(res, "content").([]any); ok {
		for _, b := range blocks {
			if field(b, "type") == "tool_use" && field(b, "name") == toolName {
				input, has := b.(jsjson.Object).Get("input")
				if !has {
					input = jsjson.Undefined
				}
				return reply{ok: true, structured: input, usage: usage}, nil
			}
		}
	}
	return reply{typ: "error", msg: "response has no structured output", usage: usage}, nil
}

var fenceRE = regexp.MustCompile(`^` + jsstr.Space + "*```(?:json)?" + jsstr.Space + `*((?s:.)*?)` + jsstr.Space + "*```" + jsstr.Space + `*$`)

// stripFence: models sometimes wrap JSON in a ```json fence.
func stripFence(s string) string {
	if m := fenceRE.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

func (a *APIRunner) openai(ctx context.Context, c CallOptions, messages []any) (reply, error) {
	schema, err := jsjson.Parse(c.JSONSchema)
	if err != nil {
		return reply{}, err
	}
	msgs := append([]any{jsjson.Object{{Key: "role", Value: "system"}, {Key: "content", Value: c.SystemPrompt + "\n\nRespond with one JSON object that matches this JSON Schema, and nothing else:\n" + c.JSONSchema}}}, apiMessages(messages)...)
	body := jsjson.Object{
		{Key: "model", Value: c.Model}, {Key: "messages", Value: msgs},
		{Key: "response_format", Value: jsjson.Object{{Key: "type", Value: "json_schema"}, {Key: "json_schema", Value: jsjson.Object{{Key: "name", Value: toolName}, {Key: "schema", Value: schema}, {Key: "strict", Value: false}}}}},
	}
	headers := [][2]string{{"content-type", "application/json"}}
	if a.o.APIKey != "" {
		headers = append(headers, [2]string{"authorization", "Bearer " + a.o.APIKey})
	}
	status, res, err := a.post(ctx, a.base()+"/chat/completions", headers, body)
	if err != nil {
		return reply{}, err
	}
	u := field(res, "usage")
	cached := numOr0(field(field(u, "prompt_tokens_details"), "cached_tokens"))
	usage := Usage{Input: numOr0(field(u, "prompt_tokens")) - cached, Output: numOr0(field(u, "completion_tokens")), CacheRead: cached}
	if status < 200 || status > 299 {
		return httpError(status, res, usage), nil
	}
	var choice any
	if cs, ok := field(res, "choices").([]any); ok && len(cs) > 0 {
		choice = cs[0]
	}
	msg := field(choice, "message")
	if field(choice, "finish_reason") == "length" {
		return reply{typ: "error", msg: "response cut off at the output limit (maxOutputTokens)", usage: usage}, nil
	}
	if r, ok := field(msg, "refusal").(string); ok && r != "" {
		return reply{typ: "error", msg: "model refused to answer", usage: usage}, nil
	}
	content, ok := field(msg, "content").(string)
	if !ok {
		return reply{typ: "error", msg: "response has no content", usage: usage}, nil
	}
	if v, err := jsjson.Parse(stripFence(content)); err == nil {
		return reply{ok: true, structured: v, usage: usage}, nil
	}
	return reply{ok: true, structured: jsjson.Object{{Key: "not_json", Value: jsstr.U16Slice(content, 200)}}, usage: usage}, nil
}

func httpError(status int, body any, usage Usage) reply {
	raw := field(field(body, "error"), "message")
	if raw == nil {
		raw = field(body, "message")
	}
	detail := ""
	if raw != nil {
		detail = jsstr.U16String(jsstr.U16Slice(jsString(raw), errorTextMax))
	}
	if sensitive.SecretShape(detail) != "" || sensitive.SecretAssignment(detail) {
		detail = ""
	}
	tail := ""
	if detail != "" {
		tail = ": " + detail
	}
	if status == 429 || status == 529 || status == 503 || field(field(body, "error"), "type") == "overloaded_error" {
		return reply{typ: "rate_limit", msg: fmt.Sprintf("API rate limit or overload (%d)", status), usage: usage}
	}
	if status == 401 || status == 403 {
		return reply{typ: "error", msg: fmt.Sprintf("API authentication failed (%d)%s", status, tail), usage: usage}
	}
	return reply{typ: "error", msg: fmt.Sprintf("API error (%d)%s", status, tail), usage: usage}
}

func (a *APIRunner) file(id string) string { return filepath.Join(a.o.SessionDir, id+".json") }

func (a *APIRunner) load(id string) ([]any, bool) {
	if !sessionRE.MatchString(id) {
		return nil, false
	}
	b, err := os.ReadFile(a.file(id))
	if err != nil {
		return nil, false
	}
	v, err := jsjson.Parse(string(b))
	if err != nil {
		return nil, false
	}
	turns, ok := field(v, "turns").([]any)
	if field(v, "provider") != a.o.Provider || !ok {
		return nil, false
	}
	return turns, true
}

func (a *APIRunner) save(id string, turns []any) error {
	tmp := fmt.Sprintf("%s.tmp-%d", a.file(id), os.Getpid())
	if err := os.WriteFile(tmp, []byte(jsjson.MustStringify(jsjson.Object{{Key: "provider", Value: a.o.Provider}, {Key: "turns", Value: turns}})), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.file(id))
}
