package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"growth-lab/internal/jsstr"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/sensitive"
)

var envKeys = map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TMPDIR": true, "TERM": true, "TZ": true,
	"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "https_proxy": true, "http_proxy": true, "no_proxy": true, "NODE_EXTRA_CA_CERTS": true}
var envPrefixes = []string{"ANTHROPIC_", "CLAUDE_"}

// ChildEnv is the environment passed to the CLI (no DB or cloud credentials), in input order.
func ChildEnv(env []string, extraPrefixes []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		ok := envKeys[k]
		for _, p := range append(append([]string{}, envPrefixes...), extraPrefixes...) {
			if strings.HasPrefix(k, p) {
				ok = true
			}
		}
		if ok {
			out = append(out, kv)
		}
	}
	return out
}

// Stream limits.
const (
	lineBytes   = 4 * 1024 * 1024
	totalBytes  = 16 * 1024 * 1024
	stderrBytes = 64 * 1024
)

// BuildArgs are the CLI arguments.
func BuildArgs(o CallOptions) []string {
	args := []string{"-p", o.Input, "--safe-mode", "--strict-mcp-config", "--tools", "", "--output-format", "stream-json", "--verbose",
		"--json-schema", o.JSONSchema, "--append-system-prompt", o.SystemPrompt, "--max-budget-usd", toFixed4(o.BudgetUsd)}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.SessionID != "" {
		args = append(args, "--resume", o.SessionID)
	}
	return args
}

// toFixed4 is Number.prototype.toFixed(4).
func toFixed4(x float64) string {
	if math.IsNaN(x) {
		return "NaN"
	}
	if math.Abs(x) >= 1e21 {
		return jsjson.Number(x)
	}
	return fmt.Sprintf("%.4f", x)
}

func numOr0(v any) float64 {
	if f, ok := finite(v); ok {
		return f
	}
	return 0
}

// ResultStats are the token counts and API time of a result event.
func ResultStats(r jsjson.Object) (*Usage, *float64) {
	if r == nil {
		return nil, nil
	}
	var u *Usage
	switch uv := get(r, "usage").(type) {
	case jsjson.Object:
		u = &Usage{Input: numOr0(get(uv, "input_tokens")), Output: numOr0(get(uv, "output_tokens")), CacheRead: numOr0(get(uv, "cache_read_input_tokens")), CacheWrite: numOr0(get(uv, "cache_creation_input_tokens"))}
	case []any:
		// an array is an object in JavaScript: no named counts
		u = &Usage{}
	}
	var api *float64
	if f, ok := get(r, "duration_api_ms").(float64); ok {
		api = &f
	}
	return u, api
}

var (
	rateRE   = regexp.MustCompile(`(?i)rate.?limit|overloaded|\b429\b|\b529\b|too many requests`)
	budgetRE = regexp.MustCompile(`(?i)budget`)
	resumeRE = regexp.MustCompile(`(?i)no conversation found|session.*(not found|does not exist)|could not (find|load|resume)`)
)

// StreamParser checks the event order: one init first, one result last.
type StreamParser struct {
	Init, Result jsjson.Object
	NonJSON      int
	Violation    string
}

// Line reads one stream-json line.
func (p *StreamParser) Line(text string) {
	if p.Violation != "" || jsstr.Trim(text) == "" {
		return
	}
	v, err := jsjson.Parse(text)
	if err != nil {
		p.NonJSON++
		return
	}
	ev, ok := v.(jsjson.Object)
	if !ok {
		p.NonJSON++
		return
	}
	isInit := get(ev, "type") == "system" && get(ev, "subtype") == "init"
	if p.Result != nil {
		p.Violation = "event after result"
		return
	}
	if p.Init == nil {
		if !isInit {
			p.Violation = "event before init"
		} else {
			p.Init = ev
		}
		return
	}
	if isInit {
		p.Violation = "duplicate init"
	} else if get(ev, "type") == "result" {
		p.Result = ev
	}
}

// IsolationProblem: the CLI may expose no tool but StructuredOutput and no MCP server.
func IsolationProblem(init jsjson.Object) string {
	tools, ok := get(init, "tools").([]any)
	if !ok {
		return "init event has no tools"
	}
	mcp, ok := get(init, "mcp_servers").([]any)
	if !ok {
		return "init event has no mcp_servers"
	}
	var extra []string
	for _, t := range tools {
		if s, ok := t.(string); !ok || s != "StructuredOutput" {
			extra = append(extra, jsString(t))
		}
	}
	if len(extra) > 0 {
		return "init event has tools that are not allowed: " + strings.Join(extra, ", ")
	}
	if len(mcp) > 0 {
		return fmt.Sprintf("init event has %d mcp_servers", len(mcp))
	}
	return ""
}

// jsString is String(v).
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, 1):
			return "Infinity"
		case math.IsInf(x, -1):
			return "-Infinity"
		}
		return jsjson.Number(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	case jsjson.Object:
		return "[object Object]"
	}
	return "undefined"
}

func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }

// ReapGroup kills the process group; true when it is gone.
func ReapGroup(pgid int) bool {
	if !groupAlive(pgid) {
		return true
	}
	if syscall.Kill(-pgid, syscall.SIGTERM) != nil {
		return !groupAlive(pgid)
	}
	for i := 0; i < 20 && groupAlive(pgid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !groupAlive(pgid) {
		return true
	}
	if syscall.Kill(-pgid, syscall.SIGKILL) != nil {
		return !groupAlive(pgid)
	}
	for i := 0; i < 50 && groupAlive(pgid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return !groupAlive(pgid)
}

// ClaudeRunner runs `claude -p` and reads its stream-json output.
type ClaudeRunner struct {
	Bin, Cwd, LogDir string
	EnvPrefixes      []string
	// TotalsDir keeps each session's running cost and API time, because the CLI reports
	// them summed over the whole session; empty keeps the reported values as they are
	TotalsDir string
	// Env is the parent environment (os.Environ when nil)
	Env []string
}

// NewClaudeRunner creates the working folder.
func NewClaudeRunner(bin, cwd, logDir string, envPrefixes []string) (*ClaudeRunner, error) {
	if err := os.MkdirAll(cwd, 0o777); err != nil {
		return nil, err
	}
	return &ClaudeRunner{Bin: bin, Cwd: cwd, LogDir: logDir, EnvPrefixes: envPrefixes}, nil
}

func msSinceRounded(t0 time.Time) float64 {
	return math.Floor(float64(time.Since(t0).Microseconds())/1000 + 0.5)
}

// Call runs one CLI call.
func (c *ClaudeRunner) Call(o CallOptions) CallResult {
	t0 := time.Now()
	ctx := o.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return CallResult{Type: "cancelled", Message: "cancelled"}
	}
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	cmd := exec.Command(c.Bin, BuildArgs(o)...)
	cmd.Dir = c.Cwd
	cmd.Env = ChildEnv(env, c.EnvPrefixes)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	parser := &StreamParser{}
	var mu sync.Mutex
	var stop *struct{ typ, msg string }
	var killTimer *time.Timer
	pid := 0
	terminate := func(typ, msg string) {
		mu.Lock()
		defer mu.Unlock()
		if stop == nil {
			stop = &struct{ typ, msg string }{typ, msg}
		}
		if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
		if killTimer == nil && pid > 0 {
			p := pid
			killTimer = time.AfterFunc(2*time.Second, func() { _ = syscall.Kill(-p, syscall.SIGKILL) })
		}
	}
	startErr := cmd.Start()
	if startErr != nil {
		r := CallResult{Type: "process", Message: "failed to start: " + startErr.Error(), Ms: msSinceRounded(t0)}
		return r
	}
	mu.Lock()
	pid = cmd.Process.Pid
	mu.Unlock()
	timer := time.AfterFunc(time.Duration(o.TimeoutMs)*time.Millisecond, func() { terminate("timeout", fmt.Sprintf("call timed out (%dms)", o.TimeoutMs)) })
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			terminate("cancelled", "cancelled")
		case <-stopWatch:
		}
	}()

	var stderr strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 && stderr.Len() < stderrBytes {
				s := strings.ToValidUTF8(string(buf[:n]), "�")
				room := stderrBytes - stderr.Len()
				if len(s) > room {
					s = s[:room]
				}
				stderr.WriteString(s)
			}
			if err != nil {
				return
			}
		}
	}()

	// each read is handled as one chunk
	total := 0
	var pending []byte
	buf := make([]byte, 64*1024)
	onChunk := func(chunk []byte) {
		total += len(chunk)
		if total > totalBytes {
			terminate("process", "output over 16MB")
			return
		}
		pending = append(pending, chunk...)
		for {
			nl := bytes.IndexByte(pending, '\n')
			if nl < 0 {
				break
			}
			line := pending[:nl]
			pending = pending[nl+1:]
			if len(line) > lineBytes {
				terminate("process", "line over 4MB")
				return
			}
			hadInit := parser.Init != nil
			parser.Line(decodeUTF8(line))
			if parser.Violation != "" {
				terminate("process", "protocol violation: "+parser.Violation)
				return
			}
			if !hadInit && parser.Init != nil {
				if p := IsolationProblem(parser.Init); p != "" {
					terminate("isolation", "agent isolation check failed: "+p)
					return
				}
			}
		}
		if len(pending) > lineBytes {
			terminate("process", "line over 4MB")
		}
	}
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			onChunk(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			break
		}
	}
	<-stderrDone
	waitErr := cmd.Wait()
	timer.Stop()
	mu.Lock()
	if killTimer != nil {
		killTimer.Stop()
	}
	mu.Unlock()
	close(stopWatch)
	code := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	reaped := ReapGroup(pid)
	mu.Lock()
	st := stop
	mu.Unlock()
	if len(pending) > 0 && jsstr.Trim(decodeUTF8(pending)) != "" && st == nil {
		parser.Line(decodeUTF8(pending))
		if parser.Violation != "" {
			st = &struct{ typ, msg string }{"process", "protocol violation: " + parser.Violation}
		}
	}
	errText := stderr.String()
	if errText != "" {
		c.saveStderr(errText)
	}
	res := parser.Result
	resultSession := ""
	if s, ok := get(res, "session_id").(string); ok && res != nil {
		resultSession = s
	}
	cost := 0.0
	if f, ok := get(res, "total_cost_usd").(float64); ok && res != nil {
		cost = f
	}
	usage, apiMs := ResultStats(res)
	if res != nil && resultSession != "" {
		cost, apiMs = c.perCall(o.SessionID != "", resultSession, cost, apiMs)
	}
	fail := func(typ, msg string) CallResult {
		return CallResult{Type: typ, Message: msg, SessionID: resultSession, CostUsd: cost, Ms: msSinceRounded(t0), Usage: usage, APIMs: apiMs}
	}
	if !reaped {
		return fail("process", "process group still alive after SIGKILL")
	}
	if st != nil {
		return fail(st.typ, st.msg)
	}
	if ctx.Err() != nil {
		return fail("cancelled", "cancelled")
	}
	if res == nil {
		if o.SessionID != "" && resumeRE.MatchString(errText) {
			return fail("resume", "could not resume session")
		}
		if parser.Init != nil {
			return fail("process", "ended without a result event")
		}
		return fail("process", fmt.Sprintf("empty output (exit code %d)", code))
	}
	sid, isStr := get(res, "session_id").(string)
	initSid, _ := get(parser.Init, "session_id").(string)
	if !isStr || sid != initSid {
		r := fail("process", "init and result session_id differ")
		r.SessionID = ""
		return r
	}
	if o.SessionID != "" && sid != o.SessionID {
		r := fail("resume", "continued a different session than "+o.SessionID)
		r.SessionID = ""
		return r
	}
	subtype := jsStringOrEmpty(get(res, "subtype"))
	errorsVal, hasErrors := res.Get("errors")
	errs := `""`
	if hasErrors && errorsVal != nil {
		errs = jsjson.MustStringify(errorsVal)
	}
	text := subtype + " " + jsStringOrEmpty(get(res, "result")) + " " + errs
	if get(res, "is_error") == true || get(res, "subtype") != "success" {
		if budgetRE.MatchString(subtype) {
			return fail("budget", "call budget reached")
		}
		if rateRE.MatchString(text) {
			return fail("rate_limit", "API rate limit or overload")
		}
		if o.SessionID != "" && resumeRE.MatchString(text+errText) {
			return fail("resume", "could not resume session")
		}
		return fail("error", jsstr.Trim("result error: "+subtype))
	}
	if code != 0 {
		return fail("process", fmt.Sprintf("result succeeded but exit code was %d", code))
	}
	structured, ok := res.Get("structured_output")
	if !ok {
		structured = jsjson.Undefined
	}
	return CallResult{OK: true, SessionID: sid, Structured: structured, CostUsd: cost, Ms: msSinceRounded(t0), Usage: usage, APIMs: apiMs}
}

// jsStringOrEmpty is String(v ?? ”).
func jsStringOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return jsString(v)
}

// decodeUTF8 decodes like StringDecoder (invalid bytes become U+FFFD).
func decodeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

func (c *ClaudeRunner) saveStderr(text string) {
	if sensitive.SecretShape(text) != "" || sensitive.SecretAssignment(text) {
		text = "(not logged: looks like it contains a secret)"
	}
	dir := filepath.Join(c.LogDir, "agent-stderr")
	if os.MkdirAll(dir, 0o777) != nil {
		return
	}
	now := time.Now().UTC()
	f, err := os.OpenFile(filepath.Join(dir, now.Format("2006-01-02")+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("--- " + now.Format("2006-01-02T15:04:05.000Z") + "\n" + text + "\n")
}

var claudeSessionRE = regexp.MustCompile(`^[0-9a-f-]{36}$`)

type sessionTotals struct {
	CostUsd float64 `json:"cost_usd"`
	APIMs   float64 `json:"api_ms"`
}

// perCall turns the session totals of a result into this call's share and stores the new totals.
// A resumed session without stored totals (started before totals were kept) keeps the totals as they are.
func (c *ClaudeRunner) perCall(resumed bool, sid string, totalCost float64, totalAPI *float64) (float64, *float64) {
	if c.TotalsDir == "" || !claudeSessionRE.MatchString(sid) {
		return totalCost, totalAPI
	}
	file := filepath.Join(c.TotalsDir, "claude-"+sid+".json")
	var prev sessionTotals
	if resumed {
		if b, err := os.ReadFile(file); err == nil {
			_ = json.Unmarshal(b, &prev)
		}
	}
	next := sessionTotals{CostUsd: totalCost}
	cost := math.Max(0, totalCost-prev.CostUsd)
	api := totalAPI
	if totalAPI != nil {
		next.APIMs = *totalAPI
		v := math.Max(0, *totalAPI-prev.APIMs)
		api = &v
	}
	if os.MkdirAll(c.TotalsDir, 0o700) == nil {
		if b, err := json.Marshal(next); err == nil {
			tmp := file + ".tmp"
			if os.WriteFile(tmp, b, 0o600) == nil {
				_ = os.Rename(tmp, file)
			}
		}
	}
	return cost, api
}
