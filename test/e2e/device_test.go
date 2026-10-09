package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two browsers signed in to the same account see only their own conversations.
func TestSharedAccountKeepsConversationsPerBrowser(t *testing.T) {
	e := newEnv(t, nil)
	laptopA := e.login("editor1")
	laptopB := e.login("editor1")
	if cookiePart(laptopA, "gl_device") == "" || cookiePart(laptopA, "gl_device") == cookiePart(laptopB, "gl_device") {
		t.Fatalf("each browser needs its own device cookie: %q / %q", laptopA, laptopB)
	}
	e.script(panelAction("first_week_activation", ""))
	id, _ := e.ask(laptopA, "Activation by week?")

	if r := e.call("GET", "/api/conversations", laptopB, nil, nil); r.Status != 200 || r.Body["conversation_id"] != nil {
		t.Errorf("the other laptop must not reopen it: %d %s", r.Status, r.Text)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/conversations/" + id},
		{"POST", "/api/conversations/" + id + "/messages"},
		{"POST", "/api/conversations/" + id + "/stop"},
		{"GET", "/api/conversations/" + id + "/events"},
	} {
		if r := e.call(c.method, c.path, laptopB, map[string]string{"text": "hi"}, nil); r.Status != 404 {
			t.Errorf("%s %s from the other laptop: %d", c.method, c.path, r.Status)
		}
	}
	if r := e.call("GET", "/api/conversations/"+id, laptopA, nil, nil); r.Status != 200 {
		t.Errorf("own conversation: %d", r.Status)
	}

	// signing in again on the same laptop continues the conversation
	again := e.loginOn("editor1", cookiePart(laptopA, "gl_device"))
	if r := e.call("GET", "/api/conversations", again, nil, nil); r.Body["conversation_id"] != id {
		t.Errorf("same laptop after sign-in: %s", r.Text)
	}
	if r := e.call("GET", "/api/conversations/"+id, again, nil, nil); r.Status != 200 {
		t.Errorf("same laptop after sign-in: %d", r.Status)
	}

	// the device is recorded with the owner, hashed
	log, _ := os.ReadFile(filepath.Join(e.dir, ".out", "conversations", id+".jsonl"))
	var owner map[string]any
	_ = json.Unmarshal([]byte(strings.SplitN(string(log), "\n", 2)[0]), &owner)
	if owner["type"] != "owner" || owner["device"] == nil || strings.Contains(string(log), strings.TrimPrefix(cookiePart(laptopA, "gl_device"), "gl_device=")) {
		t.Errorf("owner line: %v", owner)
	}

	// older conversations without a device stay open to the account on any browser
	legacy := "aaaaaaaaaaaa"
	_ = os.WriteFile(filepath.Join(e.dir, ".out", "conversations", legacy+".jsonl"), []byte(`{"t":"2026-01-01T00:00:00.000Z","type":"owner","user":"editor1"}`+"\n"+`{"t":"2026-01-01T00:00:01.000Z","type":"user_input","request_id":"bbbbbbbbbbbb","text":"old"}`+"\n"), 0o644)
	for _, c := range []string{laptopA, laptopB} {
		if r := e.call("GET", "/api/conversations/"+legacy, c, nil, nil); r.Status != 200 {
			t.Errorf("legacy conversation: %d", r.Status)
		}
	}
	// another account still cannot see it
	if r := e.call("GET", "/api/conversations/"+id, e.login("editor2"), nil, nil); r.Status != 404 {
		t.Errorf("other account: %d", r.Status)
	}
}

// With sign-in off everyone is the admin "local"; browsers are still kept apart.
func TestSignInOffKeepsConversationsPerBrowser(t *testing.T) {
	e := newEnv(t, func(dir string) {
		p := filepath.Join(dir, "workspace.json")
		b, _ := os.ReadFile(p)
		var cfg map[string]any
		_ = json.Unmarshal(b, &cfg)
		cfg["server"] = map[string]any{"auth": false}
		b, _ = json.Marshal(cfg)
		_ = os.WriteFile(p, b, 0o644)
	})
	browser := func() string {
		r := e.call("GET", "/api/me", "", nil, nil)
		return strings.SplitN(r.Header.Get("Set-Cookie"), ";", 2)[0]
	}
	a, b := browser(), browser()
	if a == "" || a == b {
		t.Fatalf("device cookies: %q %q", a, b)
	}
	e.script(panelAction("first_week_activation", ""))
	id, _ := e.ask(a, "Activation by week?")
	if r := e.call("GET", "/api/conversations/"+id, b, nil, nil); r.Status != 404 {
		t.Errorf("other browser: %d", r.Status)
	}
	if r := e.call("GET", "/api/conversations/"+id, a, nil, nil); r.Status != 200 {
		t.Errorf("own browser: %d", r.Status)
	}
}
