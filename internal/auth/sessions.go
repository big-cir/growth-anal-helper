package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"growth-lab/internal/jsstr"
	"math"
	"regexp"
	"strings"
	"time"
)

// User is a signed-in account.
type User struct {
	Username string
	Role     Role
}

// Session is one sign-in; times are milliseconds.
type Session struct {
	User
	Key      string
	Created  int64
	LastSeen int64
}

// Session limits in milliseconds.
const (
	SessionIdleMs     = 8 * 3600_000
	SessionAbsoluteMs = 7 * 24 * 3600_000
)

// Cookie is the session cookie name.
const Cookie = "gl_session"

func keyOf(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])
}

// NowMs is Date.now.
func NowMs() int64 { return time.Now().UnixMilli() }

// SessionStore keeps sessions in memory, in creation order.
type SessionStore struct {
	keys     []string
	items    map[string]*Session
	onRevoke func(key string)
	now      func() int64
	newID    func() string
}

// NewSessionStore returns an empty store; now defaults to NowMs.
func NewSessionStore(onRevoke func(string), now func() int64) *SessionStore {
	if now == nil {
		now = NowMs
	}
	return &SessionStore{items: map[string]*Session{}, onRevoke: onRevoke, now: now, newID: randomID}
}

func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Create returns a new session ID (the cookie value).
func (s *SessionStore) Create(u User) string {
	id := s.newID()
	t := s.now()
	k := keyOf(id)
	if _, ok := s.items[k]; !ok {
		s.keys = append(s.keys, k)
	}
	s.items[k] = &Session{User: u, Key: k, Created: t, LastSeen: t}
	return id
}

func (s *SessionStore) expired(x *Session, t int64) bool {
	return t-x.LastSeen > SessionIdleMs || t-x.Created > SessionAbsoluteMs
}

// Get deletes and returns nil when expired; otherwise updates the last-used time when touch.
func (s *SessionStore) Get(id string, touch bool) *Session {
	if id == "" {
		return nil
	}
	k := keyOf(id)
	x := s.items[k]
	if x == nil {
		return nil
	}
	t := s.now()
	if s.expired(x, t) {
		s.revokeKey(k)
		return nil
	}
	if touch {
		x.LastSeen = t
	}
	return x
}

// ValidKey returns the session of a key unless expired.
func (s *SessionStore) ValidKey(key string) *Session {
	x := s.items[key]
	if x == nil {
		return nil
	}
	if s.expired(x, s.now()) {
		s.revokeKey(key)
		return nil
	}
	return x
}

// Destroy removes a session by ID.
func (s *SessionStore) Destroy(id string) bool {
	if id == "" {
		return false
	}
	return s.revokeKey(keyOf(id))
}

// RevokeUser removes all sessions of a user.
func (s *SessionStore) RevokeUser(username string) int {
	n := 0
	for _, k := range append([]string(nil), s.keys...) {
		if x := s.items[k]; x != nil && x.Username == username && s.revokeKey(k) {
			n++
		}
	}
	return n
}

func (s *SessionStore) revokeKey(k string) bool {
	if _, ok := s.items[k]; !ok {
		return false
	}
	delete(s.items, k)
	for i, x := range s.keys {
		if x == k {
			s.keys = append(s.keys[:i], s.keys[i+1:]...)
			break
		}
	}
	s.onRevoke(k)
	return true
}

// Cookie is one parsed cookie, in header order.
type CookiePair struct{ Name, Value string }

// ParseCookies parses a Cookie header; the first value of a name wins.
func ParseCookies(header string) []CookiePair {
	var out []CookiePair
	seen := map[string]bool{}
	for _, part := range strings.Split(header, ";") {
		i := strings.IndexByte(part, '=')
		if i <= 0 {
			continue
		}
		k := jsstr.Trim(part[:i])
		if !seen[k] {
			seen[k] = true
			out = append(out, CookiePair{k, jsstr.Trim(part[i+1:])})
		}
	}
	return out
}

// CookieValue returns a cookie's value, or "" when absent.
func CookieValue(header, name string) (string, bool) {
	for _, c := range ParseCookies(header) {
		if c.Name == name {
			return c.Value, true
		}
	}
	return "", false
}

// SessionCookie is the Set-Cookie value for a new session.
func SessionCookie(id string, secure bool) string {
	return Cookie + "=" + id + "; HttpOnly; SameSite=Strict; Path=/" + secureAttr(secure)
}

// ClearCookie is the Set-Cookie value that removes the session cookie.
func ClearCookie(secure bool) string {
	return Cookie + "=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0" + secureAttr(secure)
}

func secureAttr(secure bool) string {
	if secure {
		return "; Secure"
	}
	return ""
}

// Sign-in limits.
const (
	LoginMaxDelayMs = 30_000
	LoginConcurrent = 4
	LoginPerMinute  = 60
	LoginMaxEntries = 10_000
	LoginEntryTTLMs = 3600_000
)

type entry struct {
	fails     int
	until, at int64
}

// LoginLimiter has no hard lockout; the wait grows with each consecutive failure (1, 2, 4 … 30 s).
type LoginLimiter struct {
	keys    []string
	entries map[string]*entry
	recent  []int64
	active  int
	now     func() int64
}

// NewLoginLimiter returns a limiter; now defaults to NowMs.
func NewLoginLimiter(now func() int64) *LoginLimiter {
	if now == nil {
		now = NowMs
	}
	return &LoginLimiter{entries: map[string]*entry{}, now: now}
}

// Check returns -1 if an attempt is allowed, otherwise the wait in ms.
func (l *LoginLimiter) Check(keys []string) int64 {
	t := l.now()
	kept := l.recent[:0]
	for _, x := range l.recent {
		if t-x < 60_000 {
			kept = append(kept, x)
		}
	}
	l.recent = kept
	if len(l.recent) >= LoginPerMinute || l.active >= LoginConcurrent {
		return 1000
	}
	var wait int64
	for _, k := range keys {
		if e := l.entries[k]; e != nil && e.until > t && e.until-t > wait {
			wait = e.until - t
		}
	}
	if wait > 0 {
		return wait
	}
	return -1
}

// Begin marks an attempt as running.
func (l *LoginLimiter) Begin() {
	l.active++
	l.recent = append(l.recent, l.now())
}

// End records the outcome of an attempt.
func (l *LoginLimiter) End(keys []string, ok bool) {
	l.active--
	t := l.now()
	for _, k := range keys {
		if ok {
			l.remove(k)
			continue
		}
		e := l.entries[k]
		if e == nil {
			e = &entry{at: t}
		}
		e.fails++
		e.at = t
		e.until = t + int64(math.Min(LoginMaxDelayMs, 1000*math.Pow(2, float64(e.fails-1))))
		l.remove(k)
		l.entries[k] = e
		l.keys = append(l.keys, k)
	}
	l.prune(t)
}

func (l *LoginLimiter) remove(k string) {
	if _, ok := l.entries[k]; !ok {
		return
	}
	delete(l.entries, k)
	for i, x := range l.keys {
		if x == k {
			l.keys = append(l.keys[:i], l.keys[i+1:]...)
			return
		}
	}
}

func (l *LoginLimiter) prune(t int64) {
	for _, k := range append([]string(nil), l.keys...) {
		if t-l.entries[k].at > LoginEntryTTLMs {
			l.remove(k)
		}
	}
	for len(l.keys) > LoginMaxEntries {
		l.remove(l.keys[0])
	}
}

// LimiterEntry is a recorded failure (for tests).
type LimiterEntry struct {
	Key       string
	Fails     int
	Until, At int64
}

// State returns the entries in order, the recent attempt count and the running attempts.
func (l *LoginLimiter) State() ([]LimiterEntry, int, int) {
	out := make([]LimiterEntry, len(l.keys))
	for i, k := range l.keys {
		e := l.entries[k]
		out[i] = LimiterEntry{k, e.fails, e.until, e.at}
	}
	return out, len(l.recent), l.active
}

var mappedRE = regexp.MustCompile(`^::ffff:(\d+\.\d+\.\d+\.\d+)$`)

// NormalizeIP turns IPv4-mapped IPv6 into IPv4.
func NormalizeIP(ip string) string {
	v := strings.ToLower(jsstr.Trim(ip))
	if m := mappedRE.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	if v == "" {
		return "unknown"
	}
	return v
}

// ClientIP is the hops-th address from the right of X-Forwarded-For (hops < 0: direct connection).
func ClientIP(socketIP string, xff []string, hasXFF bool, hops int) string {
	if hops < 0 || !hasXFF {
		return NormalizeIP(socketIP)
	}
	var list []string
	for _, x := range strings.Split(strings.Join(xff, ","), ",") {
		if x = jsstr.Trim(x); x != "" {
			list = append(list, x)
		}
	}
	if i := len(list) - hops; i >= 0 && i < len(list) {
		return NormalizeIP(list[i])
	}
	return NormalizeIP(socketIP)
}
