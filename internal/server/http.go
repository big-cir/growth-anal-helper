package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"growth-lab/internal/auth"
	"growth-lab/internal/i18n"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/sensitive"
	"growth-lab/internal/trace"
	"growth-lab/web"
)

// Request limits.
const (
	limitBody        = 64 * 1024
	limitText        = 2000
	limitTitle       = 60
	limitDescription = 300
	limitSummary     = 2000
)

var securityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'self'; object-src 'none'; manifest-src 'none'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
	"X-Content-Type-Options":  "nosniff",
	"Referrer-Policy":         "no-referrer",
	"Cache-Control":           "no-store",
}

// HTTPError is an error with a status.
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

func herr(status int, en, ko string) error { return &HTTPError{status, i18n.Tr(en, ko)} }

var staticFiles = []struct{ path, file, typ string }{
	{"/", "index.html", "text/html; charset=utf-8"},
	{"/index.html", "index.html", "text/html; charset=utf-8"},
	{"/app.js", "app.js", "text/javascript; charset=utf-8"},
	{"/charts.js", "charts.js", "text/javascript; charset=utf-8"},
	{"/i18n.js", "i18n.js", "text/javascript; charset=utf-8"},
	{"/styles.css", "styles.css", "text/css; charset=utf-8"},
}

type staticFile struct {
	body []byte
	typ  string
}

type ctx struct {
	w      http.ResponseWriter
	r      *http.Request
	params []string
	user   *auth.Session
	ip     string
	device string
	body   func() (jsjson.Object, error)
}

type route struct {
	method  string
	pattern *regexp.Regexp
	access  string // public, viewer, editor, admin
	handler func(c *ctx) error
}

// Server is the running web server.
type Server struct {
	App    *App
	Hub    *Hub
	Panels *PanelService
	Auth   *auth.Service
	Port   int

	httpServer *http.Server
	listener   net.Listener
	files      map[string]staticFile
	headers    map[string]string
	hostsMu    sync.Mutex
	hosts      map[string]bool
	origins    map[string]bool
	external   string
	routes     []route
	streamsWG  sync.WaitGroup
	closing    chan struct{}
	closeOnce  sync.Once
}

var localUser = &auth.Session{User: auth.User{Username: "local", Role: "admin"}, Key: "local"}

func (s *Server) allowLoopback(p int) {
	s.hostsMu.Lock()
	defer s.hostsMu.Unlock()
	for _, h := range []string{"127.0.0.1", "localhost"} {
		s.hosts[fmt.Sprintf("%s:%d", h, p)] = true
		s.origins[fmt.Sprintf("http://%s:%d", h, p)] = true
	}
}

// Start listens on 127.0.0.1:port and starts the background services.
func Start(app *App, port int) (*Server, error) {
	cfg := app.WS.Config
	s := &Server{App: app, hosts: map[string]bool{}, origins: map[string]bool{}, files: map[string]staticFile{}, closing: make(chan struct{})}
	for _, f := range staticFiles {
		b, err := fs.ReadFile(web.Files, f.file)
		if err != nil {
			return nil, err
		}
		if f.file == "index.html" {
			b = []byte(strings.Replace(string(b), `<html lang="ko">`, `<html lang="`+cfg.Language+`">`, 1))
		}
		s.files[f.path] = staticFile{b, f.typ}
	}
	audit := auth.NewAuditLog(cfg.OutDir)
	_ = audit.Prune(cfg.Server.AuditRetentionDays)
	app.Audit = audit
	tl := trace.NewLog(cfg.OutDir)
	tl.Prune(cfg.Server.AuditRetentionDays)
	app.Trace = tl
	hops := -1
	if cfg.Server.PublicOrigin != nil {
		hops = cfg.Server.ProxyHops
		s.external = *cfg.Server.PublicOrigin
	}
	as, err := auth.NewService(cfg.OutDir, audit, hops, cfg.Server.Auth)
	if err != nil {
		return nil, err
	}
	s.Auth = as
	if s.Hub, err = NewHub(app); err != nil {
		return nil, err
	}
	if s.Panels, err = NewPanelService(app); err != nil {
		return nil, err
	}
	s.headers = map[string]string{}
	for k, v := range securityHeaders {
		s.headers[k] = v
	}
	if s.external != "" {
		u, _ := url.Parse(s.external)
		s.hosts[u.Host] = true
		s.origins[s.external] = true
		s.headers["Strict-Transport-Security"] = "max-age=31536000"
	} else {
		s.allowLoopback(port)
	}
	s.routes = s.makeRoutes()
	if err := checkRoutes(s.routes); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	s.listener = ln
	s.Port = ln.Addr().(*net.TCPAddr).Port
	if s.Port != port && s.external == "" {
		s.allowLoopback(s.Port)
	}
	s.httpServer = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = s.httpServer.Serve(ln) }()
	s.Panels.Start(2 * time.Second)
	s.Auth.Start(2 * time.Second)
	slog.Info("server started", "port", s.Port, "workspace", cfg.Name, "auth", cfg.Server.Auth)
	return s, nil
}

// Close stops the services and the server (once).
func (s *Server) Close() {
	s.closeOnce.Do(s.close)
}

func (s *Server) close() {
	s.Auth.Stop()
	close(s.closing)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.Hub.StopAll() }()
	go func() { defer wg.Done(); s.Panels.Stop() }()
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
	_ = s.httpServer.Close()
	slog.Info("server stopped", "port", s.Port)
}

func checkRoutes(routes []route) error {
	seen := map[string]bool{}
	publicRE := regexp.MustCompile(`^\^\\/api\\/(login|logout|me)\$$`)
	for _, r := range routes {
		switch r.access {
		case "public", "viewer", "editor", "admin":
		default:
			return fmt.Errorf("Route has no access level: %s %s", r.method, r.pattern)
		}
		k := r.method + " " + r.pattern.String()
		if seen[k] {
			return fmt.Errorf("Duplicate route: %s", k)
		}
		seen[k] = true
		if r.access == "public" && !publicRE.MatchString(r.pattern.String()) {
			return fmt.Errorf("Only login/logout/me may be public: %s", r.pattern)
		}
	}
	return nil
}

func (s *Server) sendJSON(w http.ResponseWriter, status int, body any, extra map[string]string) {
	text := jsjson.MustStringify(body)
	h := w.Header()
	for k, v := range s.headers {
		h.Set(k, v)
	}
	for k, v := range extra {
		if k == "Set-Cookie" {
			h.Add(k, v)
		} else {
			h.Set(k, v)
		}
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

func errBody(msg string) jsjson.Object { return jsjson.Object{{Key: "error", Value: msg}} }

func readJSON(r *http.Request) (jsjson.Object, error) {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if ct != "application/json" {
		return nil, herr(415, "Content-Type must be application/json", "Content-Type은 application/json이어야 함")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, limitBody+1))
	if err != nil {
		return nil, herr(400, "Invalid JSON", "JSON 형식 오류")
	}
	if len(b) > limitBody {
		return nil, herr(413, "Body is larger than 64KB", "본문이 64KB를 넘음")
	}
	text := string(b)
	if text == "" {
		text = "{}"
	}
	v, err := jsjson.Parse(text)
	if err != nil {
		return nil, herr(400, "Invalid JSON", "JSON 형식 오류")
	}
	o, ok := v.(jsjson.Object)
	if !ok {
		return nil, herr(400, "Body must be a JSON object", "본문은 JSON 객체여야 함")
	}
	return o, nil
}

func field(o jsjson.Object, k string) (any, bool) { return o.Get(k) }

func nonEmpty(v any, name string, max int) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", herr(400, name+": must be a non-empty string", name+": 비어 있지 않은 문자열")
	}
	if utf8.RuneCountInString(s) > max {
		return "", herr(400, fmt.Sprintf("%s: %d characters or fewer", name, max), fmt.Sprintf("%s: %d자 이하", name, max))
	}
	return s, nil
}

func optional(v any, present bool, name string, max int) (string, error) {
	if !present || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", herr(400, name+": must be a string", name+": 문자열")
	}
	if utf8.RuneCountInString(s) > max {
		return "", herr(400, fmt.Sprintf("%s: %d characters or fewer", name, max), fmt.Sprintf("%s: %d자 이하", name, max))
	}
	return strings.TrimSpace(s), nil
}

func idField(v any, name string) (string, error) {
	s, ok := v.(string)
	if !ok || !IDRE.MatchString(s) {
		return "", herr(400, name+": invalid format", name+" 형식 오류")
	}
	return s, nil
}

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func hashField(v any) (string, error) {
	s, ok := v.(string)
	if !ok || !hashRE.MatchString(s) {
		return "", herr(400, "preview_hash: invalid format", "preview_hash 형식 오류")
	}
	return s, nil
}

func isInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

// secretLike: text shown on screen or sent back to the agent looks like a secret.
func secretLike(text string) bool {
	return sensitive.SecretShape(text) != "" || sensitive.SecretAssignment(text)
}

func specSecretLike(spec panels.Spec) bool { return secretLike(jsjson.MustStringify(spec.JS())) }

func ptr(s string) *string { return &s }

// sessionOf: with sign-in off every request is the admin "local".
func (s *Server) sessionOf(r *http.Request) *auth.Session {
	if !s.App.WS.Config.Server.Auth {
		return localUser
	}
	return s.Auth.Session(r)
}

// audited writes the audit record before the change; if that fails, nothing changes.
func (s *Server) audited(rec auth.AuditRecord, fn func() error) error {
	if err := s.App.Audit.Write(rec); err != nil {
		slog.Error("audit log write failed", "err", err, "event", rec.Event, "user", deref(rec.User), "target", deref(rec.Target))
		return herr(500, "Request refused: the audit log could not be written", "감사 기록을 남기지 못해 요청을 거부했어요")
	}
	if err := fn(); err != nil {
		failed := rec
		failed.Event = rec.Event + "_failed"
		s.App.Audit.TryWrite(failed)
		return err
	}
	return nil
}

// stream holds an SSE response open; it ends when the client leaves, the session ends or the server stops.
func (s *Server) stream(c *ctx, start func(write func(string)), next func() <-chan struct{}, drain func() []string, cleanup func()) error {
	h := c.w.Header()
	for k, v := range s.headers {
		h.Set(k, v)
	}
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Connection", "keep-alive")
	c.w.WriteHeader(200)
	fl, _ := c.w.(http.Flusher)
	var mu sync.Mutex
	write := func(text string) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = io.WriteString(c.w, text)
		if fl != nil {
			fl.Flush()
		}
	}
	write("retry: 2000\n\n")
	ended := make(chan struct{})
	var once sync.Once
	end := func() { once.Do(func() { close(ended) }) }
	unregister := s.Auth.RegisterStream(c.user.Key, end)
	defer func() {
		unregister()
		cleanup()
	}()
	start(write)
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-ended:
			return nil
		case <-s.closing:
			return nil
		case <-c.r.Context().Done():
			return nil
		case <-hb.C:
			if s.App.WS.Config.Server.Auth && s.Auth.StillValid(c.user.Key) == nil {
				return nil
			}
			write(": heartbeat\n\n")
		case <-next():
			for _, t := range drain() {
				write(t)
			}
		}
	}
}

func (s *Server) ownedConversation(c *ctx, id string) (*Conversation, error) {
	cv := s.Hub.Get(id)
	if cv == nil || !cv.OwnedBy(c.user.User, c.device) {
		return nil, herr(404, "No such conversation", "없는 대화")
	}
	return cv, nil
}

func canWrite(u *auth.Session, p *panels.SavedPanel) bool {
	by, _ := p.Fields.Get("created_by")
	return u.Role == "admin" || (u.Role == "editor" && by == u.Username)
}

func (s *Server) ownedPanelForWrite(u *auth.Session, id string) (*panels.SavedPanel, error) {
	p, err := s.Panels.MustGet(id)
	if err != nil {
		return nil, err
	}
	if !canWrite(u, p) {
		return nil, herr(403, "You can only change panels you saved", "자기가 저장한 패널만 바꿀 수 있어요")
	}
	return p, nil
}

func (s *Server) panelView(u *auth.Session, p *panels.SavedPanel) jsjson.Object {
	return s.Panels.View(p, s.App.Snapshot(), canWrite(u, p))
}

func (s *Server) metricNames() []any {
	dict, _, err := s.App.Metrics()
	out := []any{}
	if err != nil {
		return out
	}
	for _, m := range dict.Metrics {
		out = append(out, jsjson.Object{{Key: "id", Value: m.ID}, {Key: "name", Value: m.Name}})
	}
	return out
}

func eventText(e Event) string {
	return fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, jsjson.MustStringify(e.JS()))
}

func (s *Server) makeRoutes() []route {
	cfg := s.App.WS.Config
	re := regexp.MustCompile
	return []route{
		{"POST", re(`^\/api\/login$`), "public", func(c *ctx) error {
			if !cfg.Server.Auth {
				return herr(400, "Sign-in is turned off", "인증이 꺼져 있어요")
			}
			b, err := c.body()
			if err != nil {
				return err
			}
			user, _ := field(b, "username")
			pw, _ := field(b, "password")
			us, _ := user.(string)
			ps, _ := pw.(string)
			res, err := s.Auth.Login(c.r, us, ps)
			if err != nil {
				slog.Error("sign-in failed", "err", err, "ip", c.ip)
				return herr(500, "Sign-in refused: the audit log could not be written", "감사 기록을 남기지 못해 로그인을 거부했어요")
			}
			if !res.OK {
				if res.Status == 429 {
					ms := res.RetryAfterMs
					if ms == 0 {
						ms = 1000
					}
					s.sendJSON(c.w, 429, errBody(i18n.Tr("Please try again in a moment", "잠시 뒤 다시 시도해 주세요")), map[string]string{"Retry-After": strconv.FormatInt((ms+999)/1000, 10)})
					return nil
				}
				s.sendJSON(c.w, 401, errBody(i18n.Tr("Wrong username or password", "아이디 또는 비밀번호가 맞지 않아요")), nil)
				return nil
			}
			s.sendJSON(c.w, 200, jsjson.Object{{Key: "username", Value: res.User.Username}, {Key: "role", Value: string(res.User.Role)}}, map[string]string{"Set-Cookie": auth.SessionCookie(res.SessionID, s.external != "")})
			return nil
		}},
		{"POST", re(`^\/api\/logout$`), "public", func(c *ctx) error {
			if _, err := s.Auth.Logout(c.r); err != nil {
				slog.Error("audit log write failed", "err", err, "event", "logout", "ip", c.ip)
				return herr(500, "Request refused: the audit log could not be written", "감사 기록을 남기지 못해 요청을 거부했어요")
			}
			s.sendJSON(c.w, 200, jsjson.Object{{Key: "ok", Value: true}}, map[string]string{"Set-Cookie": auth.ClearCookie(s.external != "")})
			return nil
		}},
		{"GET", re(`^\/api\/me$`), "public", func(c *ctx) error {
			u := s.sessionOf(c.r)
			if u == nil {
				s.sendJSON(c.w, 401, errBody(i18n.Tr("Sign-in required", "로그인이 필요해요")), nil)
				return nil
			}
			s.sendJSON(c.w, 200, jsjson.Object{{Key: "username", Value: u.Username}, {Key: "role", Value: string(u.Role)}, {Key: "auth", Value: cfg.Server.Auth}}, nil)
			return nil
		}},
		{"GET", re(`^\/api\/state$`), "viewer", func(c *ctx) error {
			snap := s.App.Snapshot()
			canAgent := auth.AtLeast(c.user.User, "editor")
			var snapJS, agentJS any
			if snap != nil {
				snapJS = jsjson.Object{{Key: "snapshot_id", Value: snap.ID}, {Key: "as_of", Value: snap.AsOf}}
			}
			suggestions := []any{}
			if canAgent {
				agentJS = s.App.AgentStatus().JS()
				seeds, _, _ := LoadSeedPanels(s.App.WS.Dir)
				for i, sd := range seeds {
					if i == 3 {
						break
					}
					suggestions = append(suggestions, sd.Spec.Question)
				}
			}
			list, _ := s.Panels.Store.List()
			s.sendJSON(c.w, 200, jsjson.Object{{Key: "snapshot", Value: snapJS}, {Key: "agent", Value: agentJS}, {Key: "data_mode", Value: cfg.Agent.DataMode},
				{Key: "workspace", Value: cfg.Name}, {Key: "dashboard_count", Value: len(list)}, {Key: "suggestions", Value: suggestions}, {Key: "metrics", Value: s.metricNames()}}, nil)
			return nil
		}},
		{"POST", re(`^\/api\/conversations$`), "editor", func(c *ctx) error {
			if _, err := c.body(); err != nil {
				return err
			}
			s.sendJSON(c.w, 201, jsjson.Object{{Key: "conversation_id", Value: s.Hub.Create(c.user.Username, c.device).ID}}, nil)
			return nil
		}},
		{"GET", re(`^\/api\/conversations$`), "editor", func(c *ctx) error {
			var id any
			if l := s.Hub.LatestFor(c.user.User, c.device); l != nil {
				id = *l
			}
			s.sendJSON(c.w, 200, jsjson.Object{{Key: "conversation_id", Value: id}}, nil)
			return nil
		}},
		{"GET", re(`^\/api\/conversations\/([^/]+)$`), "editor", func(c *ctx) error {
			cv, err := s.ownedConversation(c, c.params[0])
			if err != nil {
				return err
			}
			s.sendJSON(c.w, 200, cv.State(), nil)
			return nil
		}},
		{"POST", re(`^\/api\/conversations\/([^/]+)\/messages$`), "editor", func(c *ctx) error {
			cv, err := s.ownedConversation(c, c.params[0])
			if err != nil {
				return err
			}
			b, err := c.body()
			if err != nil {
				return err
			}
			tv, _ := field(b, "text")
			text, err := nonEmpty(tv, "text", limitText)
			if err != nil {
				return err
			}
			var id string
			if err := s.audited(auth.AuditRecord{Event: "agent_request", User: ptr(c.user.Username), IP: ptr(c.ip), Target: ptr(cv.ID)}, func() error {
				id = cv.Submit(text)
				return nil
			}); err != nil {
				return err
			}
			s.sendJSON(c.w, 202, jsjson.Object{{Key: "request_id", Value: id}}, nil)
			return nil
		}},
		{"POST", re(`^\/api\/conversations\/([^/]+)\/stop$`), "editor", func(c *ctx) error {
			cv, err := s.ownedConversation(c, c.params[0])
			if err != nil {
				return err
			}
			if _, err := c.body(); err != nil {
				return err
			}
			s.sendJSON(c.w, 200, jsjson.Object{{Key: "stopped", Value: cv.Stop()}}, nil)
			return nil
		}},
		{"POST", re(`^\/api\/conversations\/([^/]+)\/answers$`), "editor", func(c *ctx) error {
			cv, err := s.ownedConversation(c, c.params[0])
			if err != nil {
				return err
			}
			b, err := c.body()
			if err != nil {
				return err
			}
			rv, _ := field(b, "request_id")
			requestID, err := idField(rv, "request_id")
			if err != nil {
				return err
			}
			tv, _ := field(b, "turn_no")
			turn, ok := isInt(tv)
			if !ok {
				return herr(400, "turn_no must be an integer", "turn_no는 정수")
			}
			raw, _ := field(b, "answers")
			obj, ok := raw.(jsjson.Object)
			if !ok {
				return herr(400, "answers must be an object", "answers는 객체")
			}
			answers := map[string]string{}
			var order []string
			keyRE := regexp.MustCompile(`^[a-z_]{1,32}$`)
			for _, m := range obj {
				if !keyRE.MatchString(m.Key) {
					return herr(400, "Invalid answers key: "+m.Key, "answers 키 형식 오류: "+m.Key)
				}
				if m.Value == nil || m.Value == "" {
					continue
				}
				v, err := nonEmpty(m.Value, "answers."+m.Key, 300)
				if err != nil {
					return err
				}
				answers[m.Key] = v
				order = append(order, m.Key)
			}
			if !cv.Answer(requestID, turn, answers, order) {
				return herr(409, "Not expecting an answer now (already answered or another request)", "지금 받을 수 있는 답이 아님(이미 답했거나 다른 요청)")
			}
			s.sendJSON(c.w, 202, jsjson.Object{{Key: "ok", Value: true}}, nil)
			return nil
		}},
		{"POST", re(`^\/api\/conversations\/([^/]+)\/offdict$`), "editor", func(c *ctx) error {
			cv, err := s.ownedConversation(c, c.params[0])
			if err != nil {
				return err
			}
			b, err := c.body()
			if err != nil {
				return err
			}
			tv, _ := field(b, "turn_no")
			turn, ok := isInt(tv)
			if !ok {
				return herr(400, "turn_no must be an integer", "turn_no는 정수")
			}
			av, _ := field(b, "approve")
			approve, ok := av.(bool)
			if !ok {
				return herr(400, "approve must be true or false", "approve는 true/false")
			}
			rv, _ := field(b, "request_id")
			requestID, err := idField(rv, "request_id")
			if err != nil {
				return err
			}
			run := func() error {
				if !cv.Offdict(requestID, turn, approve) {
					return herr(409, "Not expecting an approval now (already answered or another request)", "지금 받을 수 있는 승인이 아님(이미 답했거나 다른 요청)")
				}
				return nil
			}
			if approve {
				err = s.audited(auth.AuditRecord{Event: "offdict_approved", User: ptr(c.user.Username), IP: ptr(c.ip), Target: ptr(cv.ID)}, run)
			} else {
				err = run()
			}
			if err != nil {
				return err
			}
			s.sendJSON(c.w, 202, jsjson.Object{{Key: "ok", Value: true}}, nil)
			return nil
		}},
		{"GET", re(`^\/api\/conversations\/([^/]+)\/events$`), "editor", func(c *ctx) error {
			cv, err := s.ownedConversation(c, c.params[0])
			if err != nil {
				return err
			}
			raw := c.r.Header.Get("Last-Event-ID")
			if raw == "" {
				raw = c.r.URL.Query().Get("since")
			}
			last, perr := strconv.Atoi(raw)
			has := perr == nil && last >= 0 && raw != ""
			sub, replay, resync, unsubscribe := cv.Subscribe(last, has)
			return s.stream(c, func(write func(string)) {
				if resync {
					write("event: resync\ndata: {}\n\n")
				} else {
					for _, e := range replay {
						write(eventText(e))
					}
				}
			}, sub.Notify, func() []string {
				var out []string
				for _, e := range sub.Take() {
					out = append(out, eventText(e))
				}
				return out
			}, unsubscribe)
		}},
		{"POST", re(`^\/api\/conversations\/([^/]+)\/save-draft$`), "editor", func(c *ctx) error {
			cv, err := s.ownedConversation(c, c.params[0])
			if err != nil {
				return err
			}
			b, err := c.body()
			if err != nil {
				return err
			}
			rv, _ := field(b, "request_id")
			requestID, err := idField(rv, "request_id")
			if err != nil {
				return err
			}
			hv, _ := field(b, "preview_hash")
			hash, err := hashField(hv)
			if err != nil {
				return err
			}
			p, agentRows, ok := cv.CompletedPreview(requestID, hash)
			if !ok {
				return herr(409, "This is not the current preview (a newer request finished or changed it)", "현재 미리보기가 아니에요(새 요청이 끝났거나 바뀜)")
			}
			if specSecretLike(p.Spec) {
				return herr(409, "This tool cannot handle this data", "이 도구가 다룰 수 없는 데이터예요")
			}
			desc := []rune(p.Spec.Question)
			if len(desc) > limitDescription {
				desc = desc[:limitDescription]
			}
			base := jsjson.Object{{Key: "title", Value: p.Spec.Title}, {Key: "description", Value: string(desc)}}
			reply := func(summary, status string, msg *string) {
				o := append(append(jsjson.Object{}, base...), jsjson.Member{Key: "summary", Value: summary}, jsjson.Member{Key: "summary_status", Value: status})
				if msg != nil {
					o = append(o, jsjson.Member{Key: "message", Value: *msg})
				}
				s.sendJSON(c.w, 200, o, nil)
			}
			if cfg.Agent.DataMode == "schema_only" {
				reply("", "disabled", nil)
				return nil
			}
			if agentRows == nil {
				reply("", "failed", ptr(i18n.Tr("No pseudonymized result, so no description can be written", "가명 결과가 없어 설명을 만들 수 없어요")))
				return nil
			}
			cv.mu.Lock()
			if cv.Drafting {
				cv.mu.Unlock()
				return herr(409, "The description is being written", "설명을 만드는 중이에요")
			}
			cv.Drafting = true
			cv.mu.Unlock()
			defer func() {
				cv.mu.Lock()
				cv.Drafting = false
				cv.mu.Unlock()
			}()
			r := s.App.Summarize(p.Spec, p.Columns, agentRows)
			if r.OK {
				reply(r.Text, "ok", nil)
			} else {
				reply("", "failed", &r.Message)
			}
			return nil
		}},
		{"GET", re(`^\/api\/panels\/events$`), "viewer", func(c *ctx) error {
			admin := c.user.Role == "admin"
			text := func(e PanelEvent) string {
				if e.Type == "quality_status" && !admin {
					return ""
				}
				return fmt.Sprintf("event: %s\ndata: %s\n\n", e.Type, jsjson.MustStringify(e.Data))
			}
			sub, unsubscribe := s.Panels.Subscribe()
			return s.stream(c, func(write func(string)) {
				write(text(PanelEvent{"panel_status_all", jsjson.Object{{Key: "panels", Value: s.Panels.AllStatus()}}}))
			}, func() <-chan struct{} { return sub.notify }, func() []string {
				var out []string
				for _, e := range sub.take() {
					if t := text(e); t != "" {
						out = append(out, t)
					}
				}
				return out
			}, unsubscribe)
		}},
		{"GET", re(`^\/api\/panels$`), "viewer", func(c *ctx) error {
			list, err := s.Panels.Store.List()
			if err != nil {
				return err
			}
			out := []any{}
			for _, p := range list {
				out = append(out, s.panelView(c.user, p))
			}
			s.sendJSON(c.w, 200, jsjson.Object{{Key: "panels", Value: out}}, nil)
			return nil
		}},
		{"POST", re(`^\/api\/panels$`), "editor", func(c *ctx) error {
			b, err := c.body()
			if err != nil {
				return err
			}
			cvID, _ := field(b, "conversation_id")
			id, err := idField(cvID, "conversation_id")
			if err != nil {
				return err
			}
			cv, err := s.ownedConversation(c, id)
			if err != nil {
				return err
			}
			rv, _ := field(b, "request_id")
			requestID, err := idField(rv, "request_id")
			if err != nil {
				return err
			}
			hv, _ := field(b, "preview_hash")
			hash, err := hashField(hv)
			if err != nil {
				return err
			}
			p, _, ok := cv.CompletedPreview(requestID, hash)
			if !ok {
				return herr(409, "This is not the current preview (a newer request finished or changed it)", "현재 미리보기가 아니에요(새 요청이 끝났거나 바뀜)")
			}
			sv, sp := field(b, "summary")
			summary, err := optional(sv, sp, "summary", limitSummary)
			if err != nil {
				return err
			}
			tv, _ := field(b, "title")
			title, err := nonEmpty(tv, "title", limitTitle)
			if err != nil {
				return err
			}
			title = strings.TrimSpace(title)
			dv, dp := field(b, "description")
			description, err := optional(dv, dp, "description", limitDescription)
			if err != nil {
				return err
			}
			if secretLike(title+"\n"+description+"\n"+summary) || specSecretLike(p.Spec) {
				return herr(400, "Not saved: something looks like a secret", "비밀처럼 보이는 값이 있어 저장하지 않았어요")
			}
			var summarySnap any
			if summary != "" {
				summarySnap = p.SnapshotID
			}
			versions := append(append(jsjson.Object{}, p.Versions...), jsjson.Member{Key: "pattern_contract_version", Value: float64(panels.PatternContractVersion)}, jsjson.Member{Key: "renderer_version", Value: float64(panels.RendererVersion)})
			genModel := "default"
			if cfg.Agent.Model != nil {
				genModel = *cfg.Agent.Model
			}
			fields := jsjson.Object{
				{Key: "title", Value: title}, {Key: "description", Value: description}, {Key: "summary", Value: summary}, {Key: "summary_snapshot_id", Value: summarySnap},
				{Key: "prompt", Value: cv.FirstPrompt()}, {Key: "spec", Value: p.Spec.JS()}, {Key: "versions", Value: versions}, {Key: "generated_model", Value: genModel},
				{Key: "preview_hash", Value: p.Hash}, {Key: "last_result", Value: MakeResult(p.Spec, p.SnapshotID, p.AsOf, "preview", p.Columns, p.Rows, p.Tables, s.App.Lang)},
				{Key: "created_by", Value: c.user.Username},
			}
			var panel *panels.SavedPanel
			var created bool
			if err := s.audited(auth.AuditRecord{Event: "panel_saved", User: ptr(c.user.Username), IP: ptr(c.ip), Target: ptr(cv.ID)}, func() error {
				var e error
				panel, created, e = s.Panels.Create(fields, p.Spec)
				return e
			}); err != nil {
				return err
			}
			var replaces any
			if cv.Origin != nil {
				if old, _ := s.Panels.Store.Get(cv.Origin.PanelID); old != nil && old.Str("id") != panel.Str("id") && canWrite(c.user, old) {
					replaces = old.Str("id")
				}
			}
			status := 200
			if created {
				status = 201
			}
			s.sendJSON(c.w, status, jsjson.Object{{Key: "panel", Value: s.panelView(c.user, panel)}, {Key: "created", Value: created}, {Key: "replaces", Value: replaces}}, nil)
			return nil
		}},
		{"GET", re(`^\/api\/panels\/([^/]+)$`), "viewer", func(c *ctx) error {
			p, err := s.Panels.MustGet(c.params[0])
			if err != nil {
				return err
			}
			s.sendJSON(c.w, 200, jsjson.Object{{Key: "panel", Value: s.panelView(c.user, p)}}, nil)
			return nil
		}},
		{"DELETE", re(`^\/api\/panels\/([^/]+)$`), "editor", func(c *ctx) error {
			p, err := s.ownedPanelForWrite(c.user, c.params[0])
			if err != nil {
				return err
			}
			if err := s.audited(auth.AuditRecord{Event: "panel_deleted", User: ptr(c.user.Username), IP: ptr(c.ip), Target: ptr(p.Str("id"))}, func() error { return s.Panels.Delete(p.Str("id")) }); err != nil {
				return err
			}
			s.sendJSON(c.w, 200, jsjson.Object{{Key: "deleted", Value: true}}, nil)
			return nil
		}},
		{"POST", re(`^\/api\/panels\/([^/]+)\/recompute$`), "editor", func(c *ctx) error {
			if _, err := c.body(); err != nil {
				return err
			}
			p, err := s.ownedPanelForWrite(c.user, c.params[0])
			if err != nil {
				return err
			}
			var st string
			if err := s.audited(auth.AuditRecord{Event: "recompute", User: ptr(c.user.Username), IP: ptr(c.ip), Target: ptr(p.Str("id"))}, func() error {
				var e error
				st, e = s.Panels.Recompute(p.Str("id"))
				return e
			}); err != nil {
				return err
			}
			s.sendJSON(c.w, 202, jsjson.Object{{Key: "job_status", Value: st}}, nil)
			return nil
		}},
		{"POST", re(`^\/api\/panels\/([^/]+)\/recompute\/cancel$`), "editor", func(c *ctx) error {
			if _, err := c.body(); err != nil {
				return err
			}
			p, err := s.ownedPanelForWrite(c.user, c.params[0])
			if err != nil {
				return err
			}
			st, err := s.Panels.Cancel(p.Str("id"))
			if err != nil {
				return err
			}
			s.sendJSON(c.w, 200, jsjson.Object{{Key: "job_status", Value: st}}, nil)
			return nil
		}},
		{"POST", re(`^\/api\/panels\/([^/]+)\/regenerate$`), "editor", func(c *ctx) error {
			if _, err := c.body(); err != nil {
				return err
			}
			p, err := s.ownedPanelForWrite(c.user, c.params[0])
			if err != nil {
				return err
			}
			var parts []string
			for _, a := range p.Spec.Answers {
				parts = append(parts, a.Question+" "+a.Answer)
			}
			answersText := strings.Join(parts, " ")
			prompt := p.Str("prompt")
			if sensitive.Topic(prompt+" "+answersText) != "" || secretLike(prompt+"\n"+answersText) {
				s.App.Audit.TryWrite(auth.AuditRecord{Event: "sensitive_blocked", User: ptr(c.user.Username), IP: ptr(c.ip), Target: ptr(p.Str("id"))})
				return herr(409, "This tool does not handle credentials, tokens, connection details, settings or personal contacts", "이 도구는 인증 정보·토큰·연결 정보·설정·개인 연락처를 다루지 않아요")
			}
			var id string
			if err := s.audited(auth.AuditRecord{Event: "regenerate", User: ptr(c.user.Username), IP: ptr(c.ip), Target: ptr(p.Str("id"))}, func() error {
				cv := s.Hub.Create(c.user.Username, c.device)
				cv.SetOrigin(p.Str("id"), prompt)
				var defs []string
				for _, a := range p.Spec.Answers {
					defs = append(defs, "- "+a.Question+": "+a.Answer)
				}
				text := prompt + "\n\n" + i18n.Tr("Rebuild it using the previously chosen definitions as defaults.", "이전에 정한 정의를 기본값으로 다시 만들어 주세요.")
				if len(defs) > 0 {
					text += "\n" + strings.Join(defs, "\n")
				}
				r := []rune(text)
				if len(r) > limitText {
					r = r[:limitText]
				}
				cv.Submit(string(r))
				id = cv.ID
				return nil
			}); err != nil {
				return err
			}
			s.sendJSON(c.w, 201, jsjson.Object{{Key: "conversation_id", Value: id}}, nil)
			return nil
		}},
		{"POST", re(`^\/api\/panels\/([^/]+)\/resummarize$`), "editor", func(c *ctx) error {
			if _, err := c.body(); err != nil {
				return err
			}
			p0, err := s.ownedPanelForWrite(c.user, c.params[0])
			if err != nil {
				return err
			}
			if specSecretLike(p0.Spec) {
				return herr(409, "This tool cannot handle this data", "이 도구가 다룰 수 없는 데이터예요")
			}
			if cfg.Agent.DataMode == "schema_only" {
				return herr(409, "No description in this mode: result values are not sent", "결과 값을 보내지 않는 모드라 설명을 만들지 않아요")
			}
			var out jsjson.Object
			if err := s.audited(auth.AuditRecord{Event: "resummarize", User: ptr(c.user.Username), IP: ptr(c.ip), Target: ptr(p0.Str("id"))}, func() error {
				var e error
				out, e = s.Panels.Exclusive(p0.Str("id"), func(p *panels.SavedPanel) (jsjson.Object, error) {
					cols, agentRows, err := s.Panels.VerifyLastResult(p)
					if err != nil {
						return nil, err
					}
					r := s.App.Summarize(p.Spec, cols, agentRows)
					if !r.OK {
						return jsjson.Object{{Key: "summary_status", Value: "failed"}, {Key: "message", Value: r.Message}}, nil
					}
					sid, _ := p.Obj("last_result").Get("snapshot_id")
					next, err := s.Panels.UpdateSummary(p.Str("id"), r.Text, str(sid))
					if err != nil {
						return nil, err
					}
					return jsjson.Object{{Key: "summary_status", Value: "ok"}, {Key: "panel", Value: s.panelView(c.user, next)}}, nil
				})
				return e
			}); err != nil {
				return err
			}
			s.sendJSON(c.w, 200, out, nil)
			return nil
		}},
		{"GET", re(`^\/api\/quality$`), "admin", func(c *ctx) error {
			s.sendJSON(c.w, 200, s.Panels.Quality(), nil)
			return nil
		}},
	}
}

// tracked notes whether the response has started.
type tracked struct {
	http.ResponseWriter
	wrote bool
}

func (t *tracked) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *tracked) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

func (t *tracked) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) serve(rw http.ResponseWriter, r *http.Request) {
	w := &tracked{ResponseWriter: rw}
	err := s.handle(w, r)
	if err == nil || w.wrote {
		return
	}
	var h *HTTPError
	var cf *Conflict
	var nf *NotFound
	switch {
	case errors.As(err, &h):
		s.sendJSON(w, h.Status, errBody(h.Msg), nil)
	case errors.As(err, &cf):
		s.sendJSON(w, 409, errBody(cf.Msg), nil)
	case errors.As(err, &nf):
		s.sendJSON(w, 404, errBody(nf.Msg), nil)
	default:
		slog.Error("request failed", "err", err, "method", r.Method, "path", r.URL.Path)
		s.sendJSON(w, 500, errBody(i18n.Tr("Server error", "서버 오류")), nil)
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) error {
	s.hostsMu.Lock()
	hostOK := s.hosts[r.Host]
	originOK := s.origins[r.Header.Get("Origin")]
	s.hostsMu.Unlock()
	if !hostOK {
		return herr(403, "Host not allowed", "Host 거부")
	}
	method := r.Method
	if method != "GET" && method != "HEAD" {
		if !originOK || r.Header.Get("X-Growth-Lab") != "1" {
			return herr(403, "Not a same-origin request", "같은 출처 요청이 아님")
		}
	}
	path := r.URL.EscapedPath()
	if !strings.HasPrefix(path, "/api/") {
		f, ok := s.files[path]
		if !ok {
			return herr(404, "No such path", "없는 경로")
		}
		if method != "GET" {
			return herr(405, "Method not allowed", "허용되지 않는 메서드")
		}
		h := w.Header()
		for k, v := range s.headers {
			h.Set(k, v)
		}
		h.Set("Content-Type", f.typ)
		h.Set("Content-Length", strconv.Itoa(len(f.body)))
		w.WriteHeader(200)
		_, _ = w.Write(f.body)
		return nil
	}
	var parsed jsjson.Object
	var parseErr error
	done := false
	c := &ctx{w: w, r: r, ip: s.Auth.IP(r), device: s.device(w, r)}
	c.body = func() (jsjson.Object, error) {
		if !done {
			parsed, parseErr = readJSON(r)
			done = true
		}
		return parsed, parseErr
	}
	for _, rt := range s.routes {
		if rt.access == "public" && rt.method == method && rt.pattern.MatchString(path) {
			return rt.handler(c)
		}
	}
	user := s.sessionOf(r)
	if user == nil {
		return herr(401, "Sign-in required", "로그인이 필요해요")
	}
	var matches []route
	var subs [][]string
	for _, rt := range s.routes {
		if rt.access == "public" {
			continue
		}
		if m := rt.pattern.FindStringSubmatch(path); m != nil {
			matches = append(matches, rt)
			subs = append(subs, m)
		}
	}
	if len(matches) == 0 {
		return herr(404, "No such path", "없는 경로")
	}
	hit := -1
	for i, rt := range matches {
		if rt.method == method {
			hit = i
			break
		}
	}
	if hit < 0 {
		return herr(405, "Method not allowed", "허용되지 않는 메서드")
	}
	if !auth.AtLeast(user.User, auth.Role(matches[hit].access)) {
		return herr(403, "Permission denied", "권한이 없어요")
	}
	params := make([]string, 0, len(subs[hit])-1)
	for _, p := range subs[hit][1:] {
		d, err := url.PathUnescape(p)
		if err != nil {
			return herr(400, "Bad path encoding", "잘못된 경로 인코딩")
		}
		if !IDRE.MatchString(d) {
			return herr(400, "Invalid ID", "ID 형식 오류")
		}
		params = append(params, d)
	}
	c.params = params
	c.user = user
	return matches[hit].handler(c)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
