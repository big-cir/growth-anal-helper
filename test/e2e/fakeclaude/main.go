// Command fakeclaude prints scripted Claude Code stream-json for tests.
// FAKE_CLAUDE_SCRIPT is a JSON array of steps (one per call, the last repeats), FAKE_CLAUDE_DIR holds the call counter and logs.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type step struct {
	// wrapped in {"step": …} unless rawStructured
	Structured       json.RawMessage   `json:"structured"`
	RawStructured    bool              `json:"rawStructured"`
	Events           []json.RawMessage `json:"events"`
	Raw              []string          `json:"raw"`
	SessionID        string            `json:"sessionId"`
	Cost             *float64          `json:"cost"`
	IsError          bool              `json:"isError"`
	Subtype          string            `json:"subtype"`
	ResultText       string            `json:"resultText"`
	Tools            json.RawMessage   `json:"tools"`
	MCPServers       json.RawMessage   `json:"mcpServers"`
	Chunk            int               `json:"chunk"`
	ChunkBytes       int               `json:"chunkBytes"`
	DelayMs          int               `json:"delayMs"`
	ExitCode         int               `json:"exitCode"`
	Stderr           string            `json:"stderr"`
	SpawnChild       bool              `json:"spawnChild"`
	ChildIgnoresTerm bool              `json:"childIgnoresTerm"`
	ResultSessionID  string            `json:"resultSessionId"`
	Hang             bool              `json:"hang"`
	Usage            json.RawMessage   `json:"usage"`
	APIMs            *float64          `json:"apiMs"`
}

func main() {
	// child mode: a long-running process the fake leaves behind
	if len(os.Args) > 1 && os.Args[1] == "--child" {
		if len(os.Args) > 2 && os.Args[2] == "ignore-term" {
			signal.Ignore(syscall.SIGTERM)
		}
		select {}
	}
	dir := os.Getenv("FAKE_CLAUDE_DIR")
	b, _ := os.ReadFile(os.Getenv("FAKE_CLAUDE_SCRIPT"))
	var script []step
	_ = json.Unmarshal(b, &script)
	stateFile := filepath.Join(dir, "state.json")
	var state struct{ Calls int }
	if sb, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(sb, &state)
	}
	n := state.Calls
	sb, _ := json.Marshal(map[string]int{"calls": n + 1})
	_ = os.WriteFile(stateFile, sb, 0o644)
	argv, _ := json.Marshal(os.Args[1:])
	appendLine(filepath.Join(dir, "argv.jsonl"), string(argv))
	if len(script) == 0 {
		os.Exit(1)
	}
	st := script[min(n, len(script)-1)]

	var lines []string
	switch {
	case st.Raw != nil:
		lines = st.Raw
	case st.Events != nil:
		for _, e := range st.Events {
			lines = append(lines, string(e))
		}
	default:
		lines = defaultLines(st)
	}

	if st.SpawnChild {
		self, _ := os.Executable()
		args := []string{"--child"}
		if st.ChildIgnoresTerm {
			args = append(args, "ignore-term")
		}
		child := exec.Command(self, args...)
		if child.Start() == nil {
			appendLine(filepath.Join(dir, "children.txt"), fmt.Sprint(child.Process.Pid))
		}
	}
	if st.Stderr != "" {
		os.Stderr.WriteString(st.Stderr)
	}

	out := strings.Join(lines, "\n") + "\n"
	var pieces []string
	switch {
	case st.ChunkBytes > 0:
		for i := 0; i < len(out); i += st.ChunkBytes {
			pieces = append(pieces, out[i:min(i+st.ChunkBytes, len(out))])
		}
	case st.Chunk > 0:
		// chunk counts characters (UTF-16 units in the original); runes are close enough for tests
		r := []rune(out)
		for i := 0; i < len(r); i += st.Chunk {
			pieces = append(pieces, string(r[i:min(i+st.Chunk, len(r))]))
		}
	default:
		pieces = []string{out}
	}
	for _, p := range pieces {
		os.Stdout.WriteString(p)
		if st.DelayMs > 0 {
			time.Sleep(time.Duration(st.DelayMs) * time.Millisecond)
		}
	}
	if st.Hang {
		select {}
	}
	os.Exit(st.ExitCode)
}

func defaultLines(st step) []string {
	sid := st.SessionID
	if sid == "" {
		sid = "sess-1"
	}
	tools := json.RawMessage(`["StructuredOutput"]`)
	if st.Tools != nil {
		tools = st.Tools
	}
	mcp := json.RawMessage(`[]`)
	if st.MCPServers != nil {
		mcp = st.MCPServers
	}
	subtype := st.Subtype
	if subtype == "" {
		subtype = "success"
		if st.IsError {
			subtype = "error_during_execution"
		}
	}
	resultSID := sid
	if st.ResultSessionID != "" {
		resultSID = st.ResultSessionID
	}
	cost := 0.01
	if st.Cost != nil {
		cost = *st.Cost
	}
	init, _ := json.Marshal(map[string]any{"type": "system", "subtype": "init", "session_id": sid, "tools": tools, "mcp_servers": mcp, "model": "fake"})
	assistant, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "..."}}}, "session_id": sid})
	result := map[string]any{"type": "result", "subtype": subtype, "is_error": st.IsError, "result": st.ResultText, "session_id": resultSID, "total_cost_usd": cost}
	if st.Usage != nil {
		result["usage"] = st.Usage
	}
	if st.APIMs != nil {
		result["duration_api_ms"] = *st.APIMs
	}
	switch {
	case len(st.Structured) == 0:
	case st.RawStructured:
		result["structured_output"] = st.Structured
	default:
		result["structured_output"] = map[string]any{"step": st.Structured}
	}
	resultLine, _ := json.Marshal(result)
	return []string{string(init), string(assistant), string(resultLine)}
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line + "\n")
}
