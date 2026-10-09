package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
)

type runnerVectors struct {
	CallDefaults struct {
		Input, SystemPrompt, JSONSchema string
		BudgetUsd                       float64
		TimeoutMs                       int
	} `json:"callDefaults"`
	Claude []struct {
		Name   string          `json:"name"`
		Script json.RawMessage `json:"script"`
		Call   json.RawMessage `json:"call"`
		Result json.RawMessage `json:"result"`
		Argv   json.RawMessage `json:"argv"`
		Logs   []string        `json:"logs"`
	} `json:"claude"`
	API []struct {
		Name            string          `json:"name"`
		Provider        string          `json:"provider"`
		APIKey          string          `json:"apiKey"`
		Pricing         *Pricing        `json:"pricing"`
		MaxOutputTokens int             `json:"maxOutputTokens"`
		BaseSuffix      string          `json:"baseSuffix"`
		Closed          bool            `json:"closed"`
		Calls           json.RawMessage `json:"calls"`
		Requests        json.RawMessage `json:"requests"`
		Results         json.RawMessage `json:"results"`
		Sessions        [][2]string     `json:"sessions"`
	} `json:"api"`
}

func callJS(r CallResult) jsjson.Object {
	o := jsjson.Object{{Key: "ok", Value: r.OK}}
	if !r.OK {
		o = append(o, jsjson.Member{Key: "type", Value: r.Type}, jsjson.Member{Key: "message", Value: r.Message})
	}
	o = append(o, jsjson.Member{Key: "sessionId", Value: nullable(r.SessionID)})
	if r.OK {
		o = append(o, jsjson.Member{Key: "structured", Value: r.Structured})
	}
	o = append(o, jsjson.Member{Key: "costUsd", Value: r.CostUsd})
	if r.Usage != nil {
		o = append(o, jsjson.Member{Key: "usage", Value: usageJS(r.Usage)})
	}
	if r.APIMs != nil {
		o = append(o, jsjson.Member{Key: "apiMs", Value: *r.APIMs})
	}
	return o
}

// sameJSON compares JSON values ignoring object key order.
func sameJSON(t *testing.T, got any, want json.RawMessage) bool {
	t.Helper()
	var w, g any
	_ = json.Unmarshal(want, &w)
	_ = json.Unmarshal([]byte(jsjson.MustStringify(got)), &g)
	a, _ := json.Marshal(g)
	b, _ := json.Marshal(w)
	return string(a) == string(b)
}

func TestClaudeRunner(t *testing.T) {
	fake := fakeClaude(t)
	var v runnerVectors
	if err := contract.Load("agent-runners.json", &v); err != nil {
		t.Fatal(err)
	}
	headerRE := regexp.MustCompile(`(?m)^--- .*$`)
	for _, c := range v.Claude {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			bin := filepath.Join(dir, "claude")
			script := fmt.Sprintf("#!/bin/sh\nFAKE_CLAUDE_SCRIPT=\"%s\" FAKE_CLAUDE_DIR=\"%s\" exec \"%s\" \"$@\"\n", filepath.Join(dir, "script.json"), dir, fake)
			_ = os.WriteFile(bin, []byte(script), 0o755)
			_ = os.WriteFile(filepath.Join(dir, "script.json"), c.Script, 0o644)
			r, err := NewClaudeRunner(bin, filepath.Join(dir, ".agent-cwd"), filepath.Join(dir, "logs"), nil)
			if err != nil {
				t.Fatal(err)
			}
			call := CallOptions{Input: v.CallDefaults.Input, SystemPrompt: v.CallDefaults.SystemPrompt, JSONSchema: v.CallDefaults.JSONSchema, BudgetUsd: v.CallDefaults.BudgetUsd, TimeoutMs: v.CallDefaults.TimeoutMs}
			var co struct {
				SessionID string   `json:"sessionId"`
				Model     string   `json:"model"`
				BudgetUsd *float64 `json:"budgetUsd"`
				TimeoutMs *int     `json:"timeoutMs"`
			}
			_ = json.Unmarshal(c.Call, &co)
			call.SessionID, call.Model = co.SessionID, co.Model
			if co.BudgetUsd != nil {
				call.BudgetUsd = *co.BudgetUsd
			}
			if co.TimeoutMs != nil {
				call.TimeoutMs = *co.TimeoutMs
			}
			res := r.Call(call)
			if !sameJSON(t, callJS(res), c.Result) {
				t.Errorf("result:\n got %s\nwant %s", trunc(jsjson.MustStringify(callJS(res))), trunc(string(c.Result)))
			}
			var argv []any
			if b, err := os.ReadFile(filepath.Join(dir, "argv.jsonl")); err == nil {
				for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
					if l != "" {
						var a any
						_ = json.Unmarshal([]byte(l), &a)
						argv = append(argv, a)
					}
				}
			}
			if argv == nil {
				argv = []any{}
			}
			// a killed CLI may not have written its argv log yet
			if c.Name != "timeout" && !sameJSON(t, argv, c.Argv) {
				t.Errorf("argv differs")
			}
			var logs []string
			if es, err := os.ReadDir(filepath.Join(dir, "logs", "agent-stderr")); err == nil {
				for _, e := range es {
					b, _ := os.ReadFile(filepath.Join(dir, "logs", "agent-stderr", e.Name()))
					logs = append(logs, headerRE.ReplaceAllString(string(b), "---"))
				}
			}
			if strings.Join(logs, "|") != strings.Join(c.Logs, "|") {
				t.Errorf("stderr logs: got %q want %q", logs, c.Logs)
			}
		})
	}
}

func TestClaudeCancel(t *testing.T) {
	fake := fakeClaude(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	_ = os.WriteFile(bin, []byte(fmt.Sprintf("#!/bin/sh\nFAKE_CLAUDE_SCRIPT=\"%s\" FAKE_CLAUDE_DIR=\"%s\" exec \"%s\" \"$@\"\n", filepath.Join(dir, "script.json"), dir, fake)), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "script.json"), []byte(`[{"structured":{"action":"refuse","reason":"r","alternatives":[]},"delayMs":3000,"chunk":10,"spawnChild":true,"childIgnoresTerm":true}]`), 0o644)
	r, _ := NewClaudeRunner(bin, filepath.Join(dir, "cwd"), filepath.Join(dir, "logs"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	t0 := time.Now()
	res := r.Call(CallOptions{Input: "x", JSONSchema: "{}", BudgetUsd: 1, TimeoutMs: 20000, Ctx: ctx})
	if res.OK || res.Type != "cancelled" {
		t.Fatalf("got %+v", res)
	}
	if time.Since(t0) > 10*time.Second {
		t.Fatal("cancel took too long")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "children.txt"))
	for _, l := range strings.Fields(string(b)) {
		var pid int
		fmt.Sscan(l, &pid)
		if pid > 0 && exec.Command("kill", "-0", fmt.Sprint(pid)).Run() == nil {
			t.Errorf("descendant %d still alive", pid)
		}
	}
	if pre := (r.Call(CallOptions{Ctx: ctx})); pre.Type != "cancelled" {
		t.Errorf("pre-cancelled call: %+v", pre)
	}
}

var apiSessionRE = regexp.MustCompile(`api_[0-9a-f]{24}`)

func TestAPIRunner(t *testing.T) {
	var v runnerVectors
	if err := contract.Load("agent-runners.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.API {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			calls, _ := parse(t, c.Calls).([]any)
			var replies []any
			for _, x := range calls {
				if r := field(x, "reply"); r != nil {
					replies = append(replies, r)
				}
			}
			var requests []any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				body, _ := jsjson.Parse(string(b))
				h := jsjson.Object{}
				for _, k := range []string{"content-type", "x-api-key", "anthropic-version", "authorization"} {
					if val := r.Header.Get(k); val != "" {
						h = append(h, jsjson.Member{Key: k, Value: val})
					}
				}
				requests = append(requests, jsjson.Object{{Key: "method", Value: r.Method}, {Key: "path", Value: r.URL.RequestURI()}, {Key: "headers", Value: h}, {Key: "body", Value: body}})
				rep := replies[0]
				replies = replies[1:]
				if d, ok := field(rep, "delayMs").(float64); ok {
					select {
					case <-time.After(time.Duration(d) * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("content-type", "application/json")
				w.WriteHeader(int(field(rep, "status").(float64)))
				if raw, ok := field(rep, "raw").(string); ok {
					_, _ = w.Write([]byte(raw))
				} else {
					_, _ = w.Write([]byte(jsjson.MustStringify(field(rep, "body"))))
				}
			}))
			defer srv.Close()
			base := srv.URL
			if c.Closed {
				l, _ := net.Listen("tcp", "127.0.0.1:0")
				base = "http://" + l.Addr().String()
				l.Close()
			}
			if c.Provider == "openai" {
				base += "/v1"
			}
			dir := t.TempDir()
			r, err := NewAPIRunner(APIOptions{Provider: c.Provider, BaseURL: base + c.BaseSuffix, APIKey: c.APIKey, Pricing: c.Pricing, MaxOutputTokens: c.MaxOutputTokens, SessionDir: filepath.Join(dir, "sessions")})
			if err != nil {
				t.Fatal(err)
			}
			var results []CallResult
			var got []any
			for _, x := range calls {
				co := field(x, "call")
				call := CallOptions{Input: v.CallDefaults.Input, SystemPrompt: v.CallDefaults.SystemPrompt, JSONSchema: v.CallDefaults.JSONSchema, BudgetUsd: v.CallDefaults.BudgetUsd, TimeoutMs: v.CallDefaults.TimeoutMs}
				if s, ok := field(co, "input").(string); ok {
					call.Input = s
				}
				if s, ok := field(co, "model").(string); ok {
					call.Model = s
				}
				if s, ok := field(co, "sessionId").(string); ok {
					call.SessionID = s
				}
				if f, ok := field(co, "budgetUsd").(float64); ok {
					call.BudgetUsd = f
				}
				if f, ok := field(co, "timeoutMs").(float64); ok {
					call.TimeoutMs = int(f)
				}
				if f, ok := field(co, "sessionFrom").(float64); ok {
					call.SessionID = results[int(f)].SessionID
				}
				res := r.Call(call)
				results = append(results, res)
				js := callJS(res)
				for i := range js {
					if js[i].Key == "sessionId" {
						if s, ok := js[i].Value.(string); ok {
							js[i].Value = apiSessionRE.ReplaceAllString(s, "api_X")
						}
					}
					if js[i].Key == "structured" && js[i].Value == jsjson.Undefined {
						js = append(js[:i], js[i+1:]...)
						break
					}
				}
				got = append(got, js)
			}
			if !sameJSON(t, got, c.Results) {
				t.Errorf("results:\n got %s\nwant %s", trunc(jsjson.MustStringify(got)), trunc(string(c.Results)))
			}
			if requests == nil {
				requests = []any{}
			}
			if !sameJSON(t, requests, c.Requests) {
				t.Errorf("requests:\n got %s\nwant %s", trunc(jsjson.MustStringify(requests)), trunc(string(c.Requests)))
			}
			var sessions [][2]string
			if es, err := os.ReadDir(filepath.Join(dir, "sessions")); err == nil {
				for _, e := range es {
					b, _ := os.ReadFile(filepath.Join(dir, "sessions", e.Name()))
					sessions = append(sessions, [2]string{apiSessionRE.ReplaceAllString(e.Name(), "api_X"), apiSessionRE.ReplaceAllString(string(b), "api_X")})
				}
			}
			sort.Slice(sessions, func(i, j int) bool { return sessions[i][0] < sessions[j][0] })
			if fmt.Sprint(sessions) != fmt.Sprint(c.Sessions) {
				t.Errorf("sessions:\n got %v\nwant %v", sessions, c.Sessions)
			}
		})
	}
}

func TestIsolationAllowsOnlyStructuredOutput(t *testing.T) {
	for _, tool := range []string{"Read", "WebFetch", "structuredoutput", "StructuredOutput "} {
		init := jsjson.Object{{Key: "tools", Value: []any{"StructuredOutput", tool}}, {Key: "mcp_servers", Value: []any{}}}
		if got := IsolationProblem(init); got != "init event has tools that are not allowed: "+tool {
			t.Errorf("%q: %q", tool, got)
		}
	}
	if got := IsolationProblem(jsjson.Object{{Key: "tools", Value: []any{"StructuredOutput"}}, {Key: "mcp_servers", Value: []any{}}}); got != "" {
		t.Error(got)
	}
}

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
)

// fakeClaude builds the scripted Claude CLI (test/e2e/fakeclaude) once per test run.
func fakeClaude(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gl-fakeclaude-")
		if err != nil {
			fakeErr = err
			return
		}
		fakePath = filepath.Join(dir, "fakeclaude")
		cmd := exec.Command("go", "build", "-o", fakePath, "./test/e2e/fakeclaude")
		cmd.Dir = filepath.Join("..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeErr = fmt.Errorf("%v: %s", err, out)
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakePath
}

// The CLI reports cost and API time summed over the session; the runner returns each call's share.
func TestClaudeRunnerSessionTotals(t *testing.T) {
	fake := fakeClaude(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	_ = os.WriteFile(bin, []byte(fmt.Sprintf("#!/bin/sh\nFAKE_CLAUDE_SCRIPT=\"%s\" FAKE_CLAUDE_DIR=\"%s\" exec \"%s\" \"$@\"\n", filepath.Join(dir, "script.json"), dir, fake)), 0o755)
	const sid = "6fb56957-280b-440a-b43b-be8f8042c294"
	step := func(cost, api float64) map[string]any {
		return map[string]any{"structured": map[string]any{}, "sessionId": sid, "cost": cost, "apiMs": api}
	}
	script, _ := json.Marshal([]any{step(0.02, 1000), step(0.05, 2500), step(0.09, 4000), step(0.12, 5000)})
	_ = os.WriteFile(filepath.Join(dir, "script.json"), script, 0o644)
	r, err := NewClaudeRunner(bin, filepath.Join(dir, ".agent-cwd"), filepath.Join(dir, "logs"), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.TotalsDir = filepath.Join(dir, "agent-sessions")
	call := func(resume string) CallResult {
		return r.Call(CallOptions{Input: "q", SystemPrompt: "s", JSONSchema: "{}", BudgetUsd: 1, TimeoutMs: 10_000, SessionID: resume})
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	for i, want := range []struct {
		resume    string
		cost, api float64
	}{{"", 0.02, 1000}, {sid, 0.03, 1500}, {sid, 0.04, 1500}} {
		res := call(want.resume)
		if !res.OK || !near(res.CostUsd, want.cost) || res.APIMs == nil || !near(*res.APIMs, want.api) {
			t.Fatalf("call %d: ok=%v cost=%v api=%v, want %v %v", i+1, res.OK, res.CostUsd, res.APIMs, want.cost, want.api)
		}
	}
	st, err := os.Stat(filepath.Join(r.TotalsDir, "claude-"+sid+".json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("totals file: %v %v", err, st)
	}
	// A resumed session without stored totals keeps the reported totals
	_ = os.Remove(filepath.Join(r.TotalsDir, "claude-"+sid+".json"))
	if res := call(sid); !near(res.CostUsd, 0.12) {
		t.Fatalf("without stored totals: cost %v", res.CostUsd)
	}
}
