package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"growth-lab/internal/auth"
	"growth-lab/internal/server"
	"growth-lab/internal/snapshot"
	"growth-lab/internal/workspace"
)

func copyAny(t *testing.T, from, to string) {
	t.Helper()
	info, err := os.Stat(from)
	if err != nil {
		return
	}
	if info.IsDir() {
		_ = os.MkdirAll(to, info.Mode().Perm())
		entries, _ := os.ReadDir(from)
		for _, e := range entries {
			copyAny(t, filepath.Join(from, e.Name()), filepath.Join(to, e.Name()))
		}
		_ = os.Chmod(to, info.Mode().Perm())
		return
	}
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}

// Opt-in: GROWTH_LAB_REAL_WORKSPACE=<dir> starts the Go server on a copy of an existing workspace (the source is only read)
// and checks that saved panels, conversations, accounts and the snapshot already in the workspace load.
func TestExistingWorkspaceLoads(t *testing.T) {
	src := os.Getenv("GROWTH_LAB_REAL_WORKSPACE")
	if src == "" {
		t.Skip("GROWTH_LAB_REAL_WORKSPACE not set")
	}
	srcWS, err := workspace.Load(src)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := snapshot.ReadCurrent(snapshot.Dir(srcWS.Config.OutDir))
	if err != nil || cur == nil {
		t.Fatal("no current snapshot", err)
	}
	dir := t.TempDir()
	for _, f := range []string{"workspace.json", "tables.json", "derived.sql", "derived-columns.json", "metrics.json", "guide.md", "ga4-reports.json", "verify.json", "seed-panels", "quality", "panels", "conversations", "auth"} {
		copyAny(t, filepath.Join(src, f), filepath.Join(dir, f))
	}
	snaps := filepath.Join(dir, "snapshots")
	_ = os.MkdirAll(snaps, 0o755)
	f := snapshot.SnapshotFiles(filepath.Dir(cur.File), cur.SnapshotID)
	for _, p := range []string{f.Real, f.Agent, f.Map} {
		copyAny(t, p, filepath.Join(snaps, filepath.Base(p)))
	}
	copyAny(t, filepath.Join(filepath.Dir(cur.File), "current.json"), filepath.Join(snaps, "current.json"))

	// the agent is the fake one; nothing reaches Claude, GA4 or the source database
	fakeDir := filepath.Join(dir, "fake")
	_ = os.MkdirAll(fakeDir, 0o755)
	wrapper := filepath.Join(fakeDir, "claude")
	_ = os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nFAKE_CLAUDE_SCRIPT=%q FAKE_CLAUDE_DIR=%q exec %q \"$@\"\n", filepath.Join(fakeDir, "script.json"), fakeDir, fakeBin)), 0o755)
	_ = os.WriteFile(filepath.Join(fakeDir, "script.json"), []byte(`[{"structured":{"action":"refuse","reason":"ok","alternatives":[]}}]`), 0o644)
	var cfg map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "workspace.json"))
	_ = json.Unmarshal(b, &cfg)
	cfg["agent"] = map[string]any{"bin": wrapper, "callTimeoutMs": 10000}
	cfg["server"] = map[string]any{"auth": true}
	b, _ = json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "workspace.json"), b, 0o644)
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := auth.ReadAccounts(ws.Config.OutDir)
	if err != nil {
		t.Fatalf("existing accounts file: %v", err)
	}
	if err := auth.AddAccount(ws.Config.OutDir, "gotest1", "admin", password); err != nil {
		t.Fatal(err)
	}

	app, err := server.NewApp(ws)
	if err != nil {
		t.Fatal(err)
	}
	s, err := server.Start(app, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	app.StartIsolationCheck()
	app.WaitAgent()
	e := &env{t: t, dir: dir, ws: ws, srv: s, base: fmt.Sprintf("http://127.0.0.1:%d", s.Port)}
	admin := e.login("gotest1")

	st := e.call("GET", "/api/state", admin, nil, nil)
	if st.Status != 200 || st.str("snapshot", "snapshot_id") != cur.SnapshotID {
		t.Fatalf("state: %s", st.Text)
	}
	list := e.call("GET", "/api/panels", admin, nil, nil)
	panelsList, _ := list.get("panels").([]any)
	entries, _ := os.ReadDir(filepath.Join(src, "panels"))
	files := 0
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".json") {
			files++
		}
	}
	if list.Status != 200 || len(panelsList) != files {
		t.Fatalf("panels: %d files, %d listed (%s)", files, len(panelsList), list.Text[:min(300, len(list.Text))])
	}
	for _, p := range panelsList {
		id := p.(map[string]any)["id"].(string)
		g := e.call("GET", "/api/panels/"+id, admin, nil, nil)
		if g.Status != 200 || g.get("panel", "sql") == nil {
			t.Errorf("panel %s: %d", id, g.Status)
		}
		t.Logf("panel %s: status %v, rows %d", id, g.get("panel", "status"), len(g.get("panel", "last_result", "rows").([]any)))
	}
	convs, _ := os.ReadDir(filepath.Join(src, "conversations"))
	loaded, previews, owned := 0, 0, 0
	for _, c := range convs {
		id := strings.TrimSuffix(c.Name(), ".jsonl")
		cv := s.Hub.Get(id)
		if cv == nil {
			t.Errorf("conversation %s did not load", id)
			continue
		}
		state := cv.State()
		loaded++
		if p, _ := state.Get("preview"); p != nil {
			previews++
		}
		// someone else's conversation stays hidden from this admin
		if cv.OwnedBy(auth.User{Username: "gotest1", Role: "admin"}, "") {
			owned++
		} else if g := e.call("GET", "/api/conversations/"+id, admin, nil, nil); g.Status != 404 {
			t.Errorf("conversation %s of another user: %d", id, g.Status)
		}
	}
	t.Logf("accounts in the existing file: %d; conversations loaded %d of %d (%d with a preview, %d visible to the test admin); panels %d", len(existing), loaded, len(convs), previews, owned, len(panelsList))
	evs := e.events("/api/panels/events", admin, nil, []string{"panel_status_all"}, 3*time.Second)
	if len(evs) == 0 {
		t.Error("dashboard events")
	}
	if q := e.call("GET", "/api/quality", admin, nil, nil); q.Status != 200 {
		t.Errorf("quality: %d", q.Status)
	}
	wait(t, "quality checks on the real snapshot", func() bool {
		return e.call("GET", "/api/quality", admin, nil, nil).str("status") == "idle" && e.call("GET", "/api/quality", admin, nil, nil).get("computed_at") != nil
	})
	q := e.call("GET", "/api/quality", admin, nil, nil)
	failedItems := 0
	for _, it := range q.get("items").([]any) {
		if it.(map[string]any)["error"] != nil {
			failedItems++
		}
	}
	t.Logf("quality: %d items, %d with errors", len(q.get("items").([]any)), failedItems)
}
