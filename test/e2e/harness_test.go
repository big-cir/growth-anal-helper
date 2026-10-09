// Package e2e runs the Go server on a demo workspace with a scripted fake agent.
package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"growth-lab/internal/auth"
	"growth-lab/internal/demo"
	"growth-lab/internal/query"
	"growth-lab/internal/server"
	"growth-lab/internal/snapshot"
	"growth-lab/internal/workspace"
)

const (
	password = "correct-horse-battery"
	anchor   = "2024-06-03 12:00:00"
	cutoff   = "2024-06-03 12:00:00.000000"
)

var (
	root     string
	binDir   string
	fakeBin  string
	engineGo string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	root = filepath.Join(filepath.Dir(file), "..", "..")
	var err error
	binDir, err = os.MkdirTemp("", "gl-e2e-bin-")
	if err != nil {
		panic(err)
	}
	engineGo = filepath.Join(binDir, "growth-lab")
	fakeBin = filepath.Join(binDir, "fakeclaude")
	for _, c := range [][]string{
		{filepath.Join(root, "scripts", "go"), "build", "-o", engineGo, "./cmd/growth-lab"},
		{"go", "build", "-o", fakeBin, "./test/e2e/fakeclaude"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
			os.Exit(1)
		}
	}
	query.WorkerCommand = func() (string, []string, error) { return engineGo, []string{"query-worker"}, nil }
	code := m.Run()
	os.RemoveAll(binDir)
	os.Exit(code)
}

// env is one running server on its own workspace copy.
type env struct {
	t    *testing.T
	dir  string
	ws   *workspace.Workspace
	srv  *server.Server
	base string
}

func copyFile(t *testing.T, from, to string) {
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyDir(t *testing.T, from, to string) {
	_ = os.MkdirAll(to, 0o755)
	entries, _ := os.ReadDir(from)
	for _, e := range entries {
		if e.IsDir() {
			copyDir(t, filepath.Join(from, e.Name()), filepath.Join(to, e.Name()))
		} else {
			copyFile(t, filepath.Join(from, e.Name()), filepath.Join(to, e.Name()))
		}
	}
}

// newEnv builds a demo workspace with a snapshot and four accounts; edit changes files before collecting.
func newEnv(t *testing.T, edit func(dir string)) *env {
	t.Helper()
	dir := t.TempDir()
	demoDir := filepath.Join(root, "examples", "demo")
	for _, f := range []string{"tables.json", "derived.sql", "derived-columns.json", "metrics.json", "guide.md"} {
		copyFile(t, filepath.Join(demoDir, f), filepath.Join(dir, f))
	}
	copyDir(t, filepath.Join(demoDir, "seed-panels"), filepath.Join(dir, "seed-panels"))
	copyDir(t, filepath.Join(demoDir, "quality"), filepath.Join(dir, "quality"))
	fakeDir := filepath.Join(dir, "fake")
	_ = os.MkdirAll(fakeDir, 0o755)
	wrapper := filepath.Join(fakeDir, "claude")
	_ = os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nFAKE_CLAUDE_SCRIPT=%q FAKE_CLAUDE_DIR=%q exec %q \"$@\"\n", filepath.Join(fakeDir, "script.json"), fakeDir, fakeBin)), 0o755)
	cfg := map[string]any{
		"name": "demo-board", "outDir": ".out",
		"datasource": map[string]any{"host": "sqlite://.out/source.sqlite"},
		"policy":     map[string]any{"readablePrefixes": []string{"r_", "d_", "snapshot_"}},
		"params":     map[string]any{"cohort_start": "2024-01-01 00:00:00.000000", "calendar_start": "2024-01-01 00:00:00.000000", "quality_min_ts": "2020-01-01 00:00:00.000000"},
		"agent":      map[string]any{"bin": wrapper, "callTimeoutMs": 10000},
		"server":     map[string]any{"auth": true},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "workspace.json"), b, 0o644)
	if edit != nil {
		edit(dir)
	}
	_ = os.MkdirAll(filepath.Join(dir, ".out"), 0o755)
	if _, err := demo.Seed(filepath.Join(dir, ".out", "source.sqlite"), demo.Options{Anchor: anchor}); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	collectAt(t, ws, cutoff)
	for _, a := range [][2]string{{"admin1", "admin"}, {"editor1", "editor"}, {"editor2", "editor"}, {"viewer1", "viewer"}} {
		if err := auth.AddAccount(ws.Config.OutDir, a[0], auth.Role(a[1]), password); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{t: t, dir: dir, ws: ws}
	e.script(map[string]any{"structured": map[string]any{"action": "refuse", "reason": "ok", "alternatives": []string{}}})
	app, err := server.NewApp(ws)
	if err != nil {
		t.Fatal(err)
	}
	s, err := server.Start(app, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = s
	e.base = fmt.Sprintf("http://127.0.0.1:%d", s.Port)
	app.StartIsolationCheck()
	app.WaitAgent()
	if st := app.AgentStatus(); st.State != "ok" {
		t.Fatalf("agent: %+v", st)
	}
	t.Cleanup(s.Close)
	return e
}

// collectAt builds a snapshot with the source clock fixed.
func collectAt(t *testing.T, ws *workspace.Workspace, now string) {
	t.Helper()
	t.Setenv("GROWTH_LAB_TEST_SOURCE_NOW", now)
	if _, err := snapshot.RunCollect(ws, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// script sets the fake agent's replies (one per call, the last repeats).
func (e *env) script(steps ...map[string]any) {
	b, _ := json.Marshal(steps)
	_ = os.WriteFile(filepath.Join(e.dir, "fake", "script.json"), b, 0o644)
	_ = os.WriteFile(filepath.Join(e.dir, "fake", "state.json"), []byte(`{"calls":0}`), 0o644)
}

type resp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Text   string
}

func (r resp) str(path ...string) string {
	v := r.get(path...)
	s, _ := v.(string)
	return s
}

func (r resp) get(path ...string) any {
	var v any = r.Body
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// call sends a request; non-GET requests carry the same-origin headers unless headers override them.
func (e *env) call(method, path, cookie string, body any, headers map[string]string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rd = strings.NewReader(s)
		} else {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
	} else if method != "GET" {
		rd = strings.NewReader("{}")
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	if method != "GET" {
		req.Header.Set("Origin", e.base)
		req.Header.Set("X-Growth-Lab", "1")
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Header: res.Header, Text: string(b)}
	_ = json.Unmarshal(b, &out.Body)
	return out
}

// login signs in as a new browser; the cookie holds the session and the device id.
func (e *env) login(user string) string { return e.loginOn(user, "") }

// loginOn signs in again from a browser that already has device (a "gl_device=…" cookie, or "" for a new one).
func (e *env) loginOn(user, device string) string {
	e.t.Helper()
	r := e.call("POST", "/api/login", device, map[string]string{"username": user, "password": password}, nil)
	if r.Status != 200 {
		e.t.Fatalf("login %s: %d %s", user, r.Status, r.Text)
	}
	var parts []string
	if device != "" {
		parts = append(parts, device)
	}
	for _, v := range r.Header.Values("Set-Cookie") {
		parts = append(parts, strings.SplitN(v, ";", 2)[0])
	}
	return strings.Join(parts, "; ")
}

// cookiePart returns "name=value" of one cookie in a Cookie header.
func cookiePart(cookie, name string) string {
	for _, p := range strings.Split(cookie, "; ") {
		if strings.HasPrefix(p, name+"=") {
			return p
		}
	}
	return ""
}

type sse struct {
	Type string
	ID   string
	Data map[string]any
}

// events reads SSE events until one of the types arrives or the timeout passes.
func (e *env) events(path, cookie string, headers map[string]string, until []string, timeout time.Duration) []sse {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.base+path, nil)
	req.Header.Set("Cookie", cookie)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: timeout}
	res, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out []sse
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	cur := sse{}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data)
		case line == "":
			if cur.Type != "" {
				out = append(out, cur)
				for _, u := range until {
					if cur.Type == u {
						return out
					}
				}
			}
			cur = sse{}
		}
	}
	return out
}

func types(evs []sse) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// ask creates a conversation, sends text and waits for the request to end or wait.
func (e *env) ask(cookie, text string, until ...string) (string, []sse) {
	e.t.Helper()
	if len(until) == 0 {
		until = []string{"done", "failed", "cancelled", "question", "offdict"}
	}
	c := e.call("POST", "/api/conversations", cookie, nil, nil)
	if c.Status != 201 {
		e.t.Fatalf("create: %d %s", c.Status, c.Text)
	}
	id := c.str("conversation_id")
	m := e.call("POST", "/api/conversations/"+id+"/messages", cookie, map[string]string{"text": text}, nil)
	if m.Status != 202 {
		e.t.Fatalf("message: %d %s", m.Status, m.Text)
	}
	return id, e.events("/api/conversations/"+id+"/events?since=0", cookie, nil, until, 20*time.Second)
}

func wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 150; i++ {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// restart stops the server and starts a new one on the same workspace.
func (e *env) restart() {
	e.t.Helper()
	e.srv.Close()
	app, err := server.NewApp(e.ws)
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := server.Start(app, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv = s
	e.base = fmt.Sprintf("http://127.0.0.1:%d", s.Port)
	app.StartIsolationCheck()
	app.WaitAgent()
	e.t.Cleanup(s.Close)
}
