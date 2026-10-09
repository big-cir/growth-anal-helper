package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"growth-lab/internal/snapshot"
)

func panelAction(metric any, sql string) map[string]any {
	if sql == "" {
		sql = "SELECT signup_week AS x, sum(board_state = 'reached') AS numerator, count(*) AS denominator FROM d_member_first_week GROUP BY 1 ORDER BY 1"
	}
	return map[string]any{"structured": map[string]any{
		"action": "panel", "plan": "Counted participation by signup week",
		"panel": map[string]any{
			"metric": metric, "title": "Board participation by signup week", "question": "What share joined a board within 7 days, by signup week?",
			"sql":        sql,
			"display":    map[string]any{"type": "line", "x": "x", "numerator": "numerator", "denominator": "denominator", "series": nil, "headline": nil},
			"definition": [][]string{{"Population", "Members 7+ days after signup"}}, "caveats": []string{},
			"answers": []map[string]any{{"question": "Window", "answer": "7 days", "defaulted": true}},
		},
	}}
}

func probeAction(sql string) map[string]any {
	return map[string]any{"structured": map[string]any{"action": "probe", "plan": "Check counts", "purpose": "Check", "sql": sql}}
}

var askAction = map[string]any{"structured": map[string]any{"action": "ask", "questions": []map[string]any{{"id": "win", "text": "Window?",
	"options": []map[string]any{{"label": "7 days", "is_default": true}, {"label": "14 days", "is_default": false}}, "allow_free_text": true}}}}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestStaticFilesAndRequestChecks(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"/", "/index.html", "/app.js", "/charts.js", "/i18n.js", "/styles.css"} {
		r := e.call("GET", p, "", nil, nil)
		if r.Status != 200 || r.Header.Get("Content-Security-Policy") == "" || r.Header.Get("X-Content-Type-Options") != "nosniff" || r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %v", p, r.Status, r.Header)
		}
	}
	if r := e.call("GET", "/", "", nil, nil); !strings.Contains(r.Text, `<html lang="en">`) {
		t.Error("index.html language not set")
	}
	cases := []struct {
		method, path string
		headers      map[string]string
		status       int
	}{
		{"GET", "/nope", nil, 404},
		{"POST", "/app.js", nil, 405},
		{"GET", "/api/me", map[string]string{"Host": "evil.example:80"}, 403},
		{"POST", "/api/login", map[string]string{"Origin": "http://evil.example"}, 403},
		{"POST", "/api/login", map[string]string{"X-Growth-Lab": "0"}, 403},
		{"GET", "/api/nope", nil, 401},
	}
	for _, c := range cases {
		if r := e.call(c.method, c.path, "", nil, c.headers); r.Status != c.status {
			t.Errorf("%s %s %v: got %d want %d (%s)", c.method, c.path, c.headers, r.Status, c.status, r.Text)
		}
	}
}

func TestSignInLockoutAndBodies(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.call("GET", "/api/me", "", nil, nil); r.Status != 401 {
		t.Fatalf("me signed out: %d", r.Status)
	}
	bad := []struct {
		body    string
		headers map[string]string
		status  int
	}{
		{"not json", nil, 400},
		{"[]", nil, 400},
		{"{}", map[string]string{"Content-Type": "text/plain"}, 415},
		{`{"username":"` + strings.Repeat("x", 70*1024) + `"}`, nil, 413},
	}
	for _, b := range bad {
		if r := e.call("POST", "/api/login", "", b.body, b.headers); r.Status != b.status {
			t.Errorf("login body %.20q: got %d want %d", b.body, r.Status, b.status)
		}
	}
	cookie := e.login("editor1")
	me := e.call("GET", "/api/me", cookie, nil, nil)
	if me.str("username") != "editor1" || me.str("role") != "editor" {
		t.Fatalf("me: %s", me.Text)
	}
	if r := e.call("POST", "/api/logout", cookie, nil, nil); r.Status != 200 || !strings.Contains(r.Header.Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("logout: %d %s", r.Status, r.Header.Get("Set-Cookie"))
	}
	if r := e.call("GET", "/api/me", cookie, nil, nil); r.Status != 401 {
		t.Errorf("me after logout: %d", r.Status)
	}
	if r := e.call("POST", "/api/login", "", map[string]string{"username": "editor2", "password": "wrong-password-123"}, nil); r.Status != 401 {
		t.Fatalf("wrong password: %d", r.Status)
	}
	r := e.call("POST", "/api/login", "", map[string]string{"username": "editor2", "password": password}, nil)
	if r.Status != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("retry right after a failure must wait: %d %v", r.Status, r.Header)
	}
}

func TestEveryPrivateRouteNeedsSignIn(t *testing.T) {
	e := newEnv(t, nil)
	id := "aaaaaaaaaaaa"
	routes := [][2]string{
		{"GET", "/api/state"}, {"POST", "/api/conversations"}, {"GET", "/api/conversations"}, {"GET", "/api/conversations/" + id},
		{"POST", "/api/conversations/" + id + "/messages"}, {"POST", "/api/conversations/" + id + "/stop"}, {"POST", "/api/conversations/" + id + "/answers"},
		{"POST", "/api/conversations/" + id + "/offdict"}, {"GET", "/api/conversations/" + id + "/events"}, {"POST", "/api/conversations/" + id + "/save-draft"},
		{"GET", "/api/panels/events"}, {"GET", "/api/panels"}, {"POST", "/api/panels"}, {"GET", "/api/panels/" + id}, {"DELETE", "/api/panels/" + id},
		{"POST", "/api/panels/" + id + "/recompute"}, {"POST", "/api/panels/" + id + "/recompute/cancel"}, {"POST", "/api/panels/" + id + "/regenerate"},
		{"POST", "/api/panels/" + id + "/resummarize"}, {"GET", "/api/quality"},
	}
	for _, r := range routes {
		if got := e.call(r[0], r[1], "", nil, nil); got.Status != 401 {
			t.Errorf("%s %s without sign-in: %d", r[0], r[1], got.Status)
		}
	}
	viewer := e.login("viewer1")
	if r := e.call("POST", "/api/conversations", viewer, nil, map[string]string{"Origin": "http://evil.example"}); r.Status != 403 {
		t.Errorf("cross-origin POST: %d", r.Status)
	}
}

func TestRoles(t *testing.T) {
	e := newEnv(t, nil)
	viewer, editor, admin := e.login("viewer1"), e.login("editor1"), e.login("admin1")
	st := e.call("GET", "/api/state", viewer, nil, nil)
	if st.Status != 200 || st.get("agent") != nil || st.get("snapshot", "as_of") != cutoff {
		t.Errorf("viewer state: %s", st.Text)
	}
	if st := e.call("GET", "/api/state", editor, nil, nil); st.str("agent", "state") != "ok" || len(st.get("suggestions").([]any)) == 0 {
		t.Errorf("editor state: %s", st.Text)
	}
	checks := []struct {
		cookie, method, path string
		status               int
	}{
		{viewer, "POST", "/api/conversations", 403},
		{viewer, "GET", "/api/quality", 403},
		{editor, "GET", "/api/quality", 403},
		{admin, "GET", "/api/quality", 200},
		{admin, "PUT", "/api/panels", 405},
		{admin, "GET", "/api/panels/NOT-AN-ID", 400},
		{admin, "GET", "/api/panels/aaaaaaaaaaaa", 404},
		{admin, "GET", "/api/conversations/aaaaaaaaaaaa", 404},
		{viewer, "GET", "/api/panels", 200},
	}
	for _, c := range checks {
		if r := e.call(c.method, c.path, c.cookie, nil, nil); r.Status != c.status {
			t.Errorf("%s %s: got %d want %d (%s)", c.method, c.path, r.Status, c.status, r.Text)
		}
	}
}

func TestProbePanelSaveAndDashboard(t *testing.T) {
	e := newEnv(t, nil)
	editor, other, viewer := e.login("editor1"), e.login("editor2"), e.login("viewer1")
	e.script(probeAction("SELECT signup_week, count(*) AS n FROM d_member_first_week GROUP BY 1"), panelAction("first_week_activation", ""))
	id, evs := e.ask(editor, "What share joined a board within 7 days?")
	got := types(evs)
	for _, want := range []string{"user_input", "request_started", "plan", "preview", "done"} {
		if !contains(got, want) {
			t.Fatalf("events %v missing %s", got, want)
		}
	}
	if r := e.call("GET", "/api/conversations/"+id, other, nil, nil); r.Status != 404 {
		t.Errorf("another editor sees the conversation: %d", r.Status)
	}
	st := e.call("GET", "/api/conversations/"+id, editor, nil, nil)
	reqID, hash := st.str("preview", "request_id"), st.str("preview", "preview_hash")
	if st.str("request", "state") != "done" || len(hash) != 64 || len(st.get("preview", "rows").([]any)) == 0 {
		t.Fatalf("state: %s", st.Text)
	}
	if r := e.call("POST", "/api/conversations/"+id+"/save-draft", editor, map[string]string{"request_id": reqID, "preview_hash": strings.Repeat("0", 64)}, nil); r.Status != 409 {
		t.Errorf("save-draft with a stale hash: %d", r.Status)
	}
	save := map[string]any{"conversation_id": id, "request_id": reqID, "preview_hash": hash, "title": "Board participation", "description": "d", "summary": ""}
	saved := e.call("POST", "/api/panels", editor, save, nil)
	if saved.Status != 201 || saved.get("created") != true || saved.str("panel", "status") != "ok" {
		t.Fatalf("save: %d %s", saved.Status, saved.Text)
	}
	pid := saved.str("panel", "id")
	if r := e.call("POST", "/api/panels", editor, save, nil); r.Status != 200 || r.get("created") != false || r.str("panel", "id") != pid {
		t.Errorf("saving the same preview again: %d %s", r.Status, r.Text)
	}
	secret := map[string]any{"conversation_id": id, "request_id": reqID, "preview_hash": hash, "title": "password: hunter2", "summary": ""}
	if r := e.call("POST", "/api/panels", editor, secret, nil); r.Status != 400 {
		t.Errorf("secret-looking title: %d", r.Status)
	}
	list := e.call("GET", "/api/panels", viewer, nil, nil)
	ps := list.get("panels").([]any)
	if len(ps) != 1 || ps[0].(map[string]any)["full"] != false {
		t.Errorf("viewer list: %s", list.Text)
	}
	if g := e.call("GET", "/api/panels/"+pid, editor, nil, nil); g.get("panel", "full") != true || g.str("panel", "sql") == "" {
		t.Errorf("owner view: %s", g.Text)
	}
	if r := e.call("DELETE", "/api/panels/"+pid, viewer, nil, nil); r.Status != 403 {
		t.Errorf("viewer delete: %d", r.Status)
	}
	if r := e.call("DELETE", "/api/panels/"+pid, other, nil, nil); r.Status != 403 {
		t.Errorf("other editor delete: %d", r.Status)
	}
	if r := e.call("POST", "/api/panels/"+pid+"/recompute", editor, nil, nil); r.Status != 409 {
		t.Errorf("recompute of an up-to-date panel: %d", r.Status)
	}
	if r := e.call("DELETE", "/api/panels/"+pid, editor, nil, nil); r.Status != 200 {
		t.Errorf("delete: %d %s", r.Status, r.Text)
	}
	if r := e.call("GET", "/api/panels", viewer, nil, nil); len(r.get("panels").([]any)) != 0 {
		t.Errorf("after delete: %s", r.Text)
	}
	if _, err := os.Stat(filepath.Join(e.ws.Config.OutDir, "conversations", id+".jsonl")); err != nil {
		t.Errorf("conversation log: %v", err)
	}
}

func TestAskAnswerOffdictAndRefusal(t *testing.T) {
	e := newEnv(t, nil)
	editor := e.login("editor1")
	e.script(askAction, panelAction("first_week_activation", ""))
	id, evs := e.ask(editor, "Activation by week?")
	if types(evs)[len(evs)-1] != "question" {
		t.Fatalf("expected a question: %v", types(evs))
	}
	st := e.call("GET", "/api/conversations/"+id, editor, nil, nil)
	reqID := st.str("question", "request_id")
	turn := st.get("question", "turn_no")
	if r := e.call("POST", "/api/conversations/"+id+"/answers", editor, map[string]any{"request_id": reqID, "turn_no": turn, "answers": map[string]string{"Bad-Key": "x"}}, nil); r.Status != 400 {
		t.Errorf("bad answer key: %d", r.Status)
	}
	if r := e.call("POST", "/api/conversations/"+id+"/answers", editor, map[string]any{"request_id": reqID, "turn_no": 99, "answers": map[string]string{"win": "14 days"}}, nil); r.Status != 409 {
		t.Errorf("wrong turn: %d", r.Status)
	}
	if r := e.call("POST", "/api/conversations/"+id+"/answers", editor, map[string]any{"request_id": reqID, "turn_no": turn, "answers": map[string]string{"win": "14 days"}}, nil); r.Status != 202 {
		t.Fatalf("answer: %d %s", r.Status, r.Text)
	}
	evs = e.events("/api/conversations/"+id+"/events?since=0", editor, nil, []string{"done", "failed"}, 20*time.Second)
	if !contains(types(evs), "answers") || !contains(types(evs), "preview") {
		t.Errorf("after the answer: %v", types(evs))
	}

	e.script(panelAction(nil, ""))
	id, evs = e.ask(editor, "Something not in the dictionary")
	if types(evs)[len(evs)-1] != "offdict" {
		t.Fatalf("expected an approval request: %v", types(evs))
	}
	st = e.call("GET", "/api/conversations/"+id, editor, nil, nil)
	if st.get("preview") != nil || st.get("offdict", "spec") == nil {
		t.Errorf("before approval no result is shown: %s", st.Text)
	}
	if r := e.call("POST", "/api/conversations/"+id+"/offdict", editor, map[string]any{"request_id": st.str("offdict", "request_id"), "turn_no": st.get("offdict", "turn_no"), "approve": true}, nil); r.Status != 202 {
		t.Fatalf("approve: %d %s", r.Status, r.Text)
	}
	evs = e.events("/api/conversations/"+id+"/events?since=0", editor, nil, []string{"done", "failed"}, 20*time.Second)
	if !contains(types(evs), "preview") {
		t.Errorf("after approval: %v", types(evs))
	}

	e.script(map[string]any{"structured": map[string]any{"action": "refuse", "reason": "Cannot answer", "alternatives": []string{"Try weekly counts"}}})
	_, evs = e.ask(editor, "List every member by name")
	if !contains(types(evs), "refused") {
		t.Errorf("refusal: %v", types(evs))
	}
}

func TestSensitiveLockStopAndReplay(t *testing.T) {
	e := newEnv(t, nil)
	editor := e.login("editor1")
	e.script(map[string]any{"structured": map[string]any{"action": "refuse", "reason": "x", "alternatives": []string{}}})
	id, evs := e.ask(editor, "What is the admin password?")
	if !contains(types(evs), "refused") {
		t.Fatalf("sensitive question: %v", types(evs))
	}
	if st := e.call("GET", "/api/conversations/"+id, editor, nil, nil); st.get("locked") != true {
		t.Errorf("not locked: %s", st.Text)
	}
	data, _ := os.ReadFile(filepath.Join(e.ws.Config.OutDir, "conversations", id+".jsonl"))
	if strings.Contains(string(data), "password") {
		t.Error("the sensitive question was written to the log")
	}
	// a locked conversation refuses everything, without the agent
	for i := 0; i < 170; i++ {
		if r := e.call("POST", "/api/conversations/"+id+"/messages", editor, map[string]string{"text": "weekly signups " + strconv.Itoa(i)}, nil); r.Status != 202 {
			t.Fatalf("message: %d", r.Status)
		}
	}
	wait(t, "events", func() bool {
		st := e.call("GET", "/api/conversations/"+id, editor, nil, nil)
		n, _ := st.get("last_event_id").(float64)
		return n >= 510
	})
	evs = e.events("/api/conversations/"+id+"/events", editor, map[string]string{"Last-Event-ID": "1"}, []string{"resync"}, 3*time.Second)
	if len(evs) == 0 || evs[0].Type != "resync" {
		t.Errorf("old Last-Event-ID must resync: %v", types(evs))
	}
	evs = e.events("/api/conversations/"+id+"/events?since=505", editor, nil, []string{"done"}, 3*time.Second)
	if len(evs) == 0 || evs[0].ID != "506" {
		t.Errorf("replay after 505: %v", evs)
	}

	e.script(map[string]any{"structured": panelAction("first_week_activation", "")["structured"], "delayMs": 4000})
	slow, _ := e.ask(editor, "slow request", "request_started")
	r := e.call("POST", "/api/conversations/"+slow+"/stop", editor, nil, nil)
	if r.Status != 200 || r.get("stopped") != true {
		t.Fatalf("stop: %d %s", r.Status, r.Text)
	}
	evs = e.events("/api/conversations/"+slow+"/events?since=0", editor, nil, []string{"cancelled"}, 5*time.Second)
	if !contains(types(evs), "cancelled") {
		t.Errorf("stop events: %v", types(evs))
	}
	if r := e.call("POST", "/api/conversations/"+slow+"/stop", editor, nil, nil); r.get("stopped") != false {
		t.Errorf("stop with nothing running: %s", r.Text)
	}
}

func TestPrivateColumnsStayHidden(t *testing.T) {
	e := newEnv(t, func(dir string) {
		var roles map[string]any
		b, _ := os.ReadFile(filepath.Join(dir, "derived-columns.json"))
		_ = json.Unmarshal(b, &roles)
		roles["d_member.country"] = "private"
		b, _ = json.Marshal(roles)
		_ = os.WriteFile(filepath.Join(dir, "derived-columns.json"), b, 0o644)
	})
	editor := e.login("editor1")
	e.script(probeAction("SELECT country, count(*) AS n FROM d_member GROUP BY 1"),
		panelAction("first_week_activation", "SELECT country AS x, count(*) AS numerator, count(*) AS denominator FROM d_member GROUP BY 1"),
		map[string]any{"structured": map[string]any{"action": "refuse", "reason": "stop", "alternatives": []string{}}})
	id, evs := e.ask(editor, "Members by country")
	if contains(types(evs), "preview") {
		t.Fatalf("a panel over a private column was shown: %v", types(evs))
	}
	st := e.call("GET", "/api/conversations/"+id, editor, nil, nil)
	if st.get("preview") != nil {
		t.Errorf("preview: %s", st.Text)
	}
	for _, ev := range evs {
		b, _ := json.Marshal(ev.Data)
		for _, country := range []string{`"KR"`, `"US"`, `"JP"`} {
			if strings.Contains(string(b), country) {
				t.Errorf("a private value reached the screen: %s", b)
			}
		}
	}
}

func TestRecomputeOnNewSnapshot(t *testing.T) {
	e := newEnv(t, nil)
	editor, admin := e.login("editor1"), e.login("admin1")
	wait(t, "quality checks", func() bool {
		q := e.call("GET", "/api/quality", admin, nil, nil)
		items, _ := q.get("items").([]any)
		return q.str("status") == "idle" && len(items) > 0
	})
	q := e.call("GET", "/api/quality", admin, nil, nil)
	ids := []string{}
	for _, it := range q.get("items").([]any) {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	if !contains(ids, "q_seed_panels") || !contains(ids, "q_orphans") {
		t.Errorf("quality items: %v", ids)
	}

	e.script(panelAction("first_week_activation", ""))
	id, _ := e.ask(editor, "Board participation")
	st := e.call("GET", "/api/conversations/"+id, editor, nil, nil)
	saved := e.call("POST", "/api/panels", editor, map[string]any{"conversation_id": id, "request_id": st.str("preview", "request_id"), "preview_hash": st.str("preview", "preview_hash"), "title": "Board participation", "summary": ""}, nil)
	pid := saved.str("panel", "id")
	if pid == "" {
		t.Fatalf("save: %s", saved.Text)
	}

	// new data, same rules: recomputed automatically
	collectAt(t, e.ws, "2024-06-10 12:00:00.000000")
	wait(t, "auto recompute", func() bool {
		g := e.call("GET", "/api/panels/"+pid, editor, nil, nil)
		return g.str("panel", "last_result", "mode") == "auto" && g.str("panel", "last_result", "as_of") == "2024-06-10 12:00:00.000000"
	})

	// changed rules: needs review, then a manual recompute
	b, _ := os.ReadFile(filepath.Join(e.dir, "derived.sql"))
	_ = os.WriteFile(filepath.Join(e.dir, "derived.sql"), append(b, []byte("\n-- changed rule\n")...), 0o644)
	if _, err := snapshot.RunDerive(e.ws); err != nil {
		t.Fatal(err)
	}
	wait(t, "review", func() bool {
		return e.call("GET", "/api/panels/"+pid, editor, nil, nil).str("panel", "status") == "review"
	})
	g := e.call("GET", "/api/panels/"+pid, editor, nil, nil)
	if changed, _ := g.get("panel", "changed_rules").([]any); len(changed) == 0 {
		t.Errorf("changed rules: %s", g.Text)
	}
	if r := e.call("POST", "/api/panels/"+pid+"/recompute", editor, nil, nil); r.Status != 202 {
		t.Fatalf("recompute: %d %s", r.Status, r.Text)
	}
	wait(t, "manual recompute", func() bool {
		g := e.call("GET", "/api/panels/"+pid, editor, nil, nil)
		return g.str("panel", "status") == "ok" && g.str("panel", "last_result", "mode") == "manual_rule"
	})
	evs := e.events("/api/panels/events", editor, nil, []string{"panel_status_all"}, 3*time.Second)
	if len(evs) == 0 || evs[0].Type != "panel_status_all" {
		t.Errorf("dashboard events: %v", types(evs))
	}
}

func TestConversationSurvivesRestart(t *testing.T) {
	e := newEnv(t, nil)
	editor := e.login("editor1")
	e.script(askAction, panelAction("first_week_activation", ""))
	id, _ := e.ask(editor, "Activation by week?")
	st := e.call("GET", "/api/conversations/"+id, editor, nil, nil)
	_ = e.call("POST", "/api/conversations/"+id+"/answers", editor, map[string]any{"request_id": st.str("question", "request_id"), "turn_no": st.get("question", "turn_no"), "answers": map[string]string{"win": "14 days"}}, nil)
	e.events("/api/conversations/"+id+"/events?since=0", editor, nil, []string{"done", "failed"}, 20*time.Second)
	before := e.call("GET", "/api/conversations/"+id, editor, nil, nil)

	// a request left running is reported as stopped by the restart
	e.script(map[string]any{"structured": panelAction("first_week_activation", "")["structured"], "delayMs": 3000})
	running, _ := e.ask(editor, "slow request", "request_started")
	e.restart()
	editor = e.loginOn("editor1", cookiePart(editor, "gl_device"))

	after := e.call("GET", "/api/conversations/"+id, editor, nil, nil)
	if after.str("preview", "preview_hash") == "" || after.str("preview", "preview_hash") != before.str("preview", "preview_hash") {
		t.Fatalf("preview after restart: %s", after.Text)
	}
	saved := e.call("POST", "/api/panels", editor, map[string]any{"conversation_id": id, "request_id": after.str("preview", "request_id"), "preview_hash": after.str("preview", "preview_hash"), "title": "Restored", "summary": ""}, nil)
	if saved.Status != 201 {
		t.Errorf("save from a restored preview: %d %s", saved.Status, saved.Text)
	}
	if r := e.call("GET", "/api/conversations", editor, nil, nil); r.str("conversation_id") == "" {
		t.Errorf("latest conversation: %s", r.Text)
	}
	// a graceful stop cancels the running request
	logText, _ := os.ReadFile(filepath.Join(e.ws.Config.OutDir, "conversations", running+".jsonl"))
	if !strings.Contains(string(logText), `"type":"cancelled"`) {
		t.Errorf("running request not cancelled on stop: %s", logText)
	}
	// after a crash a started request is reported as stopped by the restart
	crashed := "crashedconv1"
	_ = os.WriteFile(filepath.Join(e.ws.Config.OutDir, "conversations", crashed+".jsonl"),
		[]byte(`{"t":"2024-06-03T00:00:00.000Z","type":"owner","user":"editor1"}`+"\n"+`{"t":"2024-06-03T00:00:01.000Z","type":"user_input","request_id":"aaaaaaaaaaaa","text":"x"}`+"\n"+`{"t":"2024-06-03T00:00:01.000Z","type":"request_started","request_id":"aaaaaaaaaaaa"}`+"\n"), 0o644)
	if st := e.call("GET", "/api/conversations/"+crashed, editor, nil, nil); st.str("failed", "reason") != "server_restart" {
		t.Errorf("interrupted request: %s", st.Text)
	}
}

// "keep": the same question again shows the current panel without rewriting or rerunning it.
func TestKeepCurrentPanel(t *testing.T) {
	e := newEnv(t, nil)
	editor := e.login("editor1")
	keep := map[string]any{"structured": map[string]any{"action": "keep", "plan": "Same question; the current panel answers it"}}
	e.script(panelAction("first_week_activation", ""), keep)
	id, _ := e.ask(editor, "What share joined a board within 7 days?")
	first := e.call("GET", "/api/conversations/"+id, editor, nil, nil)
	hash, firstReq := first.str("preview", "preview_hash"), first.str("preview", "request_id")
	last := strconv.Itoa(int(first.get("last_event_id").(float64)))
	if r := e.call("POST", "/api/conversations/"+id+"/messages", editor, map[string]string{"text": "How many joined a board in their first week?"}, nil); r.Status != 202 {
		t.Fatalf("message: %d %s", r.Status, r.Text)
	}
	evs := e.events("/api/conversations/"+id+"/events?since="+last, editor, nil, []string{"done", "failed"}, 20*time.Second)
	kinds := []string{}
	var pv map[string]any
	for _, ev := range evs {
		if ev.Type == "step" {
			kinds = append(kinds, ev.Data["data"].(map[string]any)["kind"].(string))
		}
		if ev.Type == "preview" {
			pv = ev.Data["data"].(map[string]any)["preview"].(map[string]any)
		}
	}
	if !contains(kinds, "kept") || contains(kinds, "checking") || pv == nil {
		t.Fatalf("keep events: %v %v", types(evs), kinds)
	}
	if pv["preview_hash"] != hash || pv["request_id"] == firstReq {
		t.Fatalf("kept preview: hash %v (want %s), request %v", pv["preview_hash"], hash, pv["request_id"])
	}
	saved := e.call("POST", "/api/panels", editor, map[string]any{"conversation_id": id, "request_id": pv["request_id"], "preview_hash": hash, "title": "Kept", "summary": ""}, nil)
	if saved.Status != 201 {
		t.Errorf("save the kept preview: %d %s", saved.Status, saved.Text)
	}

	// without a current panel, keep is sent back and the agent builds a panel
	e.script(keep, panelAction("first_week_activation", ""))
	_, evs = e.ask(editor, "Board joins in the first week?")
	if got := types(evs); !contains(got, "preview") {
		t.Fatalf("keep without a panel: %v", got)
	}
}
