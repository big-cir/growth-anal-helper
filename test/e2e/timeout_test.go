package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setRun writes run settings into workspace.json.
func setRun(dir string, run map[string]any) {
	var cfg map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "workspace.json"))
	_ = json.Unmarshal(b, &cfg)
	cfg["run"] = run
	b, _ = json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "workspace.json"), b, 0o644)
}

// A background recompute uses the background limit: it fails at 1ms and keeps the previous result.
func TestRecomputeTimeout(t *testing.T) {
	e := newEnv(t, func(dir string) { setRun(dir, map[string]any{"backgroundQueryTimeoutMs": 1}) })
	editor := e.login("editor1")
	e.script(panelAction("first_week_activation", ""))
	id, _ := e.ask(editor, "Board participation")
	st := e.call("GET", "/api/conversations/"+id, editor, nil, nil)
	saved := e.call("POST", "/api/panels", editor, map[string]any{"conversation_id": id, "request_id": st.str("preview", "request_id"), "preview_hash": st.str("preview", "preview_hash"), "title": "Board participation", "summary": ""}, nil)
	pid := saved.str("panel", "id")
	if pid == "" {
		t.Fatalf("save: %s", saved.Text)
	}
	collectAt(t, e.ws, "2024-06-10 12:00:00.000000")
	wait(t, "recompute failure", func() bool {
		return e.call("GET", "/api/panels/"+pid, editor, nil, nil).str("panel", "status") == "recompute_failed"
	})
	g := e.call("GET", "/api/panels/"+pid, editor, nil, nil)
	if msg := g.str("panel", "last_error", "message"); !strings.Contains(msg, "time limit (0.001s)") {
		t.Errorf("message: %q", msg)
	}
	if asOf := g.str("panel", "last_result", "as_of"); asOf != cutoff {
		t.Errorf("previous result not kept: %s", asOf)
	}
}

// Conversation queries use the conversation limit: a probe and then a panel over 1ms end the request.
func TestConversationQueryTimeout(t *testing.T) {
	e := newEnv(t, func(dir string) { setRun(dir, map[string]any{"queryTimeoutMs": 1}) })
	editor := e.login("editor1")
	e.script(probeAction("SELECT count(*) AS n FROM d_member"), panelAction("first_week_activation", ""),
		map[string]any{"structured": map[string]any{"action": "refuse", "reason": "stop", "alternatives": []string{}}})
	_, evs := e.ask(editor, "Board participation")
	last := evs[len(evs)-1]
	if last.Type != "failed" || last.Data["data"].(map[string]any)["reason"] != "query_timeout" {
		t.Fatalf("events: %v, last %v", types(evs), last.Data)
	}
	rewrite := false
	for _, ev := range evs {
		if ev.Type == "step" && ev.Data["data"].(map[string]any)["text"] == "Query did not finish within 0.001s → rewriting a lighter query" {
			rewrite = true
		}
	}
	if !rewrite {
		t.Errorf("no rewrite step: %v", evs)
	}
}
