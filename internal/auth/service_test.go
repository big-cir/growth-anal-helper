package auth

import (
	"bufio"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"growth-lab/internal/contract"
)

const (
	pwA = "correct horse battery"
	pwB = "another long password"
)

func req(ip, cookie, xff string) testRequest {
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.RemoteAddr = ip + ":5555"
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return testRequest{r}
}

type testRequest struct{ Request *http.Request }

func auditEvents(t *testing.T, outDir string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(outDir, "logs", "audit", "audit-*.jsonl"))
	var out []string
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			line := sc.Text()
			i := strings.Index(line, `"event":"`)
			out = append(out, line[i+9:i+9+strings.Index(line[i+9:], `"`)])
		}
		fh.Close()
	}
	return out
}

func newService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	if err := AddAccount(dir, "alice", "admin", pwA); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(dir, "bob", "editor", pwB); err != nil {
		t.Fatal(err)
	}
	s, err := NewService(dir, NewAuditLog(dir), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestServiceNeedsAccounts(t *testing.T) {
	_, err := NewService(t.TempDir(), NewAuditLog(t.TempDir()), -1, true)
	if err == nil || !strings.HasPrefix(err.Error(), "No accounts.") {
		t.Fatal(err)
	}
	if _, err := NewService(t.TempDir(), NewAuditLog(t.TempDir()), -1, false); err != nil {
		t.Fatal(err)
	}
}

func TestServiceSignInLimitsAndRoles(t *testing.T) {
	s, dir := newService(t)
	clock := int64(5_000_000)
	s.Limiter = NewLoginLimiter(func() int64 { return clock })
	r, err := s.Login(req("127.0.0.1", "", "192.0.2.10").Request, "alice", "wrong password here")
	if err != nil || r.OK || r.Status != 401 {
		t.Fatalf("wrong password: %+v %v", r, err)
	}
	// the same name is locked for a while, even with the right password
	r, _ = s.Login(req("127.0.0.1", "", "192.0.2.20").Request, "alice", pwA)
	if r.Status != 429 || r.RetryAfterMs <= 0 {
		t.Fatalf("lockout: %+v", r)
	}
	// and so is the same client address for another name
	r, _ = s.Login(req("127.0.0.1", "", "192.0.2.10").Request, "bob", pwB)
	if r.Status != 429 {
		t.Fatalf("ip lockout: %+v", r)
	}
	// an invalid or unknown name fails like a wrong password; the address is then locked
	if r, _ := s.Login(req("127.0.0.1", "", "192.0.2.30").Request, "No Such", pwA); r.Status != 401 {
		t.Fatalf("invalid name: %+v", r)
	}
	if r, _ := s.Login(req("127.0.0.1", "", "192.0.2.30").Request, "zed", pwA); r.Status != 429 {
		t.Fatalf("locked address: %+v", r)
	}
	ok, err := s.Login(req("::ffff:127.0.0.1", "", "192.0.2.13, 192.0.2.14").Request, "bob", pwB)
	if err != nil || !ok.OK || ok.User != (User{"bob", "editor"}) || ok.SessionID == "" {
		t.Fatalf("bob: %+v %v", ok, err)
	}
	if !AtLeast(ok.User, "viewer") || !AtLeast(ok.User, "editor") || AtLeast(ok.User, "admin") {
		t.Fatal("role order")
	}
	cookie := "a=1; " + Cookie + "=" + ok.SessionID
	x := s.Session(req("::1", cookie, "").Request)
	if x == nil || x.Username != "bob" {
		t.Fatal("session")
	}
	var closed []int
	un := s.RegisterStream(x.Key, func() { closed = append(closed, 1) })
	s.RegisterStream(x.Key, func() { closed = append(closed, 2) })
	if s.StillValid(x.Key) == nil {
		t.Fatal("still valid")
	}
	// a role change in the file revokes the user's sessions and closes their streams in order
	if err := ModifyAccount(dir, "bob", Patch{Role: rolePtr("viewer")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if s.Session(req("::1", cookie, "").Request) != nil || s.StillValid(x.Key) != nil || len(closed) != 2 || closed[0] != 1 || closed[1] != 2 {
		t.Fatalf("after role change: %v", closed)
	}
	un()
	// disabled accounts lose their session at once and cannot sign in
	ok2, _ := s.Login(req("::1", "", "192.0.2.15").Request, "bob", pwB)
	if !ok2.OK || ok2.User.Role != "viewer" {
		t.Fatalf("bob again: %+v", ok2)
	}
	disabled := true
	if err := ModifyAccount(dir, "bob", Patch{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if s.Session(req("::1", Cookie+"="+ok2.SessionID, "").Request) != nil {
		t.Fatal("disabled session")
	}
	if r, _ := s.Login(req("::1", "", "192.0.2.16").Request, "bob", pwB); r.Status != 401 {
		t.Fatalf("disabled login: %+v", r)
	}
	clock += 60_000
	ok3, _ := s.Login(req("::1", "", "192.0.2.17").Request, "alice", pwA)
	if !ok3.OK {
		t.Fatalf("alice: %+v", ok3)
	}
	c3 := Cookie + "=" + ok3.SessionID
	if done, err := s.Logout(req("::1", c3, "192.0.2.17").Request); !done || err != nil {
		t.Fatal("logout")
	}
	if done, _ := s.Logout(req("::1", c3, "").Request); done {
		t.Fatal("second logout")
	}
	if s.Session(req("::1", "", "").Request) != nil {
		t.Fatal("no cookie")
	}
	got := strings.Join(auditEvents(t, dir), ",")
	want := "login_fail,login_fail,login_ok,session_revoked,login_ok,session_revoked,login_fail,login_ok,logout"
	if got != want {
		t.Fatalf("audit:\n got %s\nwant %s", got, want)
	}
}

func rolePtr(r Role) *Role { return &r }

func TestServiceSessionExpiry(t *testing.T) {
	s, _ := newService(t)
	clock := int64(1_000_000)
	s.Sessions = NewSessionStore(s.closeStreams, func() int64 { return clock })
	ok, _ := s.Login(req("::1", "", "").Request, "alice", pwA)
	c := Cookie + "=" + ok.SessionID
	clock += SessionIdleMs - 1
	if s.Session(req("::1", c, "").Request) == nil {
		t.Fatal("before idle limit")
	}
	clock += SessionIdleMs + 1
	if s.Session(req("::1", c, "").Request) != nil {
		t.Fatal("idle session must expire")
	}
	ok, _ = s.Login(req("::1", "", "192.0.2.40").Request, "alice", pwA)
	c = Cookie + "=" + ok.SessionID
	for i := 0; i < 45; i++ {
		clock += SessionIdleMs / 2
		s.Session(req("::1", c, "").Request)
	}
	if s.Session(req("::1", c, "").Request) != nil {
		t.Fatal("absolute limit")
	}
}

// Existing accounts files (stored scrypt hashes) sign in unchanged.
func TestServiceReadsExistingAccounts(t *testing.T) {
	var v struct {
		NodeHashes []struct {
			Password string `json:"password"`
			HashHex  string `json:"hashHex"`
		} `json:"nodeHashes"`
	}
	if err := contract.Load("auth-passwords.json", &v); err != nil {
		t.Fatal(err)
	}
	h, _ := hex.DecodeString(v.NodeHashes[0].HashHex)
	dir := t.TempDir()
	if _, err := EnsureAuthDir(dir); err != nil {
		t.Fatal(err)
	}
	body := "{\n \"version\": 1,\n \"accounts\": [\n  {\n   \"username\": \"carol\",\n   \"role\": \"editor\",\n   \"hash\": \"" + string(h) + "\",\n   \"disabled\": false,\n   \"created_at\": \"2026-10-01T00:00:00.000Z\",\n   \"changed_at\": \"2026-10-01T00:00:00.000Z\"\n  }\n ]\n}\n"
	if err := os.WriteFile(AccountsFile(dir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewService(dir, NewAuditLog(dir), -1, true)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := s.Login(req("::1", "", "").Request, "carol", v.NodeHashes[0].Password); err != nil || !r.OK || r.User.Role != "editor" {
		t.Fatalf("existing account: %+v %v", r, err)
	}
	// a change keeps the file format
	if err := ModifyAccount(dir, "carol", Patch{Role: rolePtr("admin")}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(AccountsFile(dir))
	want := strings.Replace(body, `"role": "editor"`, `"role": "admin"`, 1)
	got := string(b)
	i := strings.Index(got, `"changed_at": "`)
	j := strings.Index(want, `"changed_at": "`)
	if got[:i] != want[:j] || len(got) != len(want) {
		t.Fatalf("format changed:\n%s", got)
	}
}

// Audit writes refuse a directory with loose permissions.
func TestServiceAuditDirPermissions(t *testing.T) {
	s, dir := newService(t)
	audit := filepath.Join(dir, "logs", "audit")
	if err := os.MkdirAll(audit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(audit, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := s.Login(req("::1", "", "").Request, "alice", pwA)
	if err == nil || r.OK || !strings.Contains(err.Error(), "audit log directory must be owned by me with 0700") {
		t.Fatalf("loose audit dir: %+v %v", r, err)
	}
}
