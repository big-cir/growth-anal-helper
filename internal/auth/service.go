package auth

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RoleRank orders the roles.
var RoleRank = map[Role]int{"viewer": 0, "editor": 1, "admin": 2}

// AtLeast reports whether u has role r or higher.
func AtLeast(u User, r Role) bool { return RoleRank[u.Role] >= RoleRank[r] }

// LoginResult: on failure Status is 401 or 429 (RetryAfterMs set for 429).
type LoginResult struct {
	OK           bool
	SessionID    string
	User         User
	Status       int
	RetryAfterMs int64
}

// Service watches the account file, signs users in and out, looks up the request user and closes a session's SSE streams.
type Service struct {
	Sessions  *SessionStore
	Limiter   *LoginLimiter
	ProxyHops int // < 0: direct connections only

	mu        sync.Mutex
	outDir    string
	audit     *AuditLog
	accounts  map[string]Account
	stamp     string
	stop      chan struct{}
	streamsMu sync.Mutex
	streams   map[string][]*func()
	dummyHash string
}

// NewService loads the accounts. With requireAccounts the server needs at least one account to start.
func NewService(outDir string, audit *AuditLog, proxyHops int, requireAccounts bool) (*Service, error) {
	s := &Service{outDir: outDir, audit: audit, ProxyHops: proxyHops, accounts: map[string]Account{}, streams: map[string][]*func(){}}
	s.Sessions = NewSessionStore(s.closeStreams, nil)
	s.Limiter = NewLoginLimiter(nil)
	if err := s.Reload(); err != nil {
		return nil, err
	}
	if requireAccounts && len(s.accounts) == 0 {
		return nil, &AccountError{"No accounts. Create one first with `growth-lab account add <name> --role admin`"}
	}
	h, err := HashPassword(fmt.Sprintf("dummy-%v-password", rand.Float64()))
	if err != nil {
		return nil, err
	}
	s.dummyHash = h
	return s, nil
}

// Start reloads the account file every poll.
func (s *Service) Start(poll time.Duration) {
	s.stop = make(chan struct{})
	go func() {
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				if err := s.Reload(); err != nil {
					slog.Warn("could not reload the account file", "err", err)
				}
			}
		}
	}()
}

// Stop ends the reload loop.
func (s *Service) Stop() {
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
}

func fileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "none"
	}
	st, _ := fi.Sys().(*syscall.Stat_t)
	if st == nil {
		return fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
	}
	return fmt.Sprintf("%d:%d:%d:%d", st.Ino, fi.Size(), fi.ModTime().UnixNano(), ctimeNs(st))
}

// Reload rereads a changed account file and revokes sessions of changed users (removed, disabled, role or password changed).
func (s *Service) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp := fileStamp(AccountsFile(s.outDir))
	if stamp == s.stamp {
		return nil
	}
	list, err := ReadAccounts(s.outDir)
	if err != nil {
		return err
	}
	next := make(map[string]Account, len(list))
	for _, a := range list {
		next[a.Username] = a
	}
	for name, old := range s.accounts {
		now, ok := next[name]
		if !ok || now.Disabled || now.Role != old.Role || now.Hash != old.Hash {
			if s.Sessions.RevokeUser(name) > 0 {
				s.audit.TryWrite(AuditRecord{Event: "session_revoked", User: &name})
			}
		}
	}
	s.accounts = next
	s.stamp = stamp
	return nil
}

// IP is the client address (X-Forwarded-For only behind the configured proxies).
func (s *Service) IP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	xff, has := r.Header["X-Forwarded-For"]
	return ClientIP(host, []string{strings.Join(xff, ", ")}, has, s.ProxyHops)
}

// CookieID is the session cookie value, or "".
func (s *Service) CookieID(r *http.Request) string {
	v, _ := CookieValue(strings.Join(r.Header.Values("Cookie"), "; "), Cookie)
	return v
}

// Session checks the session together with the current account state (enabled, role).
func (s *Service) Session(r *http.Request) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.Sessions.Get(s.CookieID(r), true)
	if x == nil {
		return nil
	}
	a, ok := s.accounts[x.Username]
	if !ok || a.Disabled || a.Role != x.Role {
		s.Sessions.RevokeUser(x.Username)
		return nil
	}
	return x
}

// StillValid checks a session key (for open SSE streams).
func (s *Service) StillValid(key string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.Sessions.ValidKey(key)
	if x == nil {
		return nil
	}
	if a, ok := s.accounts[x.Username]; ok && !a.Disabled && a.Role == x.Role {
		return x
	}
	return nil
}

// Login signs a user in. A failed attempt is limited per name and per IP.
func (s *Service) Login(r *http.Request, username, password string) (LoginResult, error) {
	ip := s.IP(r)
	name := "(invalid)"
	if UsernameRE.MatchString(username) {
		name = username
	}
	keys := []string{"u:" + name, "ip:" + ip}
	s.mu.Lock()
	if wait := s.Limiter.Check(keys); wait >= 0 {
		s.mu.Unlock()
		return LoginResult{Status: 429, RetryAfterMs: wait}, nil
	}
	s.Limiter.Begin()
	a, found := s.accounts[username]
	s.mu.Unlock()
	hash := s.dummyHash
	if found && !a.Disabled {
		hash = a.Hash
	}
	match, verr := VerifyPassword(password, hash)
	ok := verr == nil && match && found && !a.Disabled
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Limiter.End(keys, ok)
	if verr != nil {
		return LoginResult{}, verr
	}
	if !ok {
		s.audit.TryWrite(AuditRecord{Event: "login_fail", User: &name, IP: &ip})
		return LoginResult{Status: 401}, nil
	}
	a = s.accounts[username]
	if err := s.audit.Write(AuditRecord{Event: "login_ok", User: &a.Username, IP: &ip}); err != nil {
		return LoginResult{}, err
	}
	s.Sessions.Destroy(s.CookieID(r))
	u := User{Username: a.Username, Role: a.Role}
	return LoginResult{OK: true, SessionID: s.Sessions.Create(u), User: u}, nil
}

// Logout fails if the audit write fails (the session is kept).
func (s *Service) Logout(r *http.Request) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.CookieID(r)
	if x := s.Sessions.Get(id, false); x != nil {
		ip := s.IP(r)
		if err := s.audit.Write(AuditRecord{Event: "logout", User: &x.Username, IP: &ip}); err != nil {
			return false, err
		}
	}
	return s.Sessions.Destroy(id), nil
}

// RegisterStream binds an SSE connection to a session; close is called (in registration order) when the session is revoked.
// close must not call back into the Service. Returns the unregister function.
func (s *Service) RegisterStream(key string, close func()) func() {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	p := &close
	s.streams[key] = append(s.streams[key], p)
	return func() {
		s.streamsMu.Lock()
		defer s.streamsMu.Unlock()
		list := s.streams[key]
		for i, x := range list {
			if x == p {
				list = append(list[:i:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(s.streams, key)
		} else {
			s.streams[key] = list
		}
	}
}

func (s *Service) closeStreams(key string) {
	s.streamsMu.Lock()
	list := s.streams[key]
	delete(s.streams, key)
	s.streamsMu.Unlock()
	for _, p := range list {
		(*p)()
	}
}
