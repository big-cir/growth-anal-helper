package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
)

func TestSessionsContract(t *testing.T) {
	var v struct {
		Cookies []struct {
			Header string
			Pairs  [][2]string
		}
		UndefinedHeader [][2]string
		SetCookies      []struct {
			Secure         bool
			Session, Clear string
		}
		Ips       []struct{ Input, Out string }
		ClientIps []struct {
			Socket string
			Xff    json.RawMessage
			Hops   *int
			Out    string
		}
		Stores []struct {
			Script  [][]json.RawMessage
			Results []struct {
				Value   json.RawMessage
				Revoked []string
			}
		}
		Limiters []struct {
			Script  [][]json.RawMessage
			Results []struct {
				Value json.RawMessage
				State json.RawMessage
			}
		}
	}
	if err := contract.Load("auth-sessions.json", &v); err != nil {
		t.Fatal(err)
	}
	pairs := func(ps []CookiePair) string {
		arr := []any{}
		for _, p := range ps {
			arr = append(arr, []any{p.Name, p.Value})
		}
		return jsjson.MustStringify(arr)
	}
	want := func(x any) string { return jsjson.MustStringify(toJS(x)) }
	for _, c := range v.Cookies {
		if got, w := pairs(ParseCookies(c.Header)), want(c.Pairs); got != w {
			t.Errorf("cookies %q: got %s, want %s", c.Header, got, w)
		}
	}
	if got := pairs(ParseCookies("")); got != want(v.UndefinedHeader) {
		t.Errorf("undefined header: %s", got)
	}
	for _, c := range v.SetCookies {
		if SessionCookie("abc-_", c.Secure) != c.Session || ClearCookie(c.Secure) != c.Clear {
			t.Errorf("set-cookie secure=%v", c.Secure)
		}
	}
	for _, c := range v.Ips {
		if got := NormalizeIP(c.Input); got != c.Out {
			t.Errorf("ip %q: got %q, want %q", c.Input, got, c.Out)
		}
	}
	for _, c := range v.ClientIps {
		hops := -1
		if c.Hops != nil {
			hops = *c.Hops
		}
		var xff []string
		has := true
		var s string
		switch {
		case string(c.Xff) == "null":
			has = false
		case json.Unmarshal(c.Xff, &s) == nil:
			xff = []string{s}
		default:
			must(t, json.Unmarshal(c.Xff, &xff))
		}
		if got := ClientIP(c.Socket, xff, has, hops); got != c.Out {
			t.Errorf("client ip %+v: got %q, want %q", c, got, c.Out)
		}
	}
	for si, s := range v.Stores {
		runStore(t, si, s.Script, func(i int) (json.RawMessage, []string) { return s.Results[i].Value, s.Results[i].Revoked })
	}
	for li, l := range v.Limiters {
		var tm int64
		lim := NewLoginLimiter(func() int64 { return tm })
		for i, op := range l.Script {
			var name string
			must(t, json.Unmarshal(op[0], &name))
			value := "null"
			switch name {
			case "now":
				must(t, json.Unmarshal(op[1], &tm))
			case "check":
				var keys []string
				must(t, json.Unmarshal(op[1], &keys))
				if w := lim.Check(keys); w >= 0 {
					value = strconv.FormatInt(w, 10)
				}
			case "begin":
				lim.Begin()
			case "end":
				var keys []string
				var ok bool
				must(t, json.Unmarshal(op[1], &keys))
				must(t, json.Unmarshal(op[2], &ok))
				lim.End(keys, ok)
			case "endMany":
				var n int
				var ok bool
				must(t, json.Unmarshal(op[1], &n))
				must(t, json.Unmarshal(op[2], &ok))
				keys := make([]string, n)
				for k := range keys {
					keys[k] = "k" + strconv.Itoa(k)
				}
				lim.End(keys, ok)
			}
			r := l.Results[i]
			if w := canon(t, r.Value); value != w {
				t.Errorf("limiter %d op %d (%s): got %s, want %s", li, i, name, value, w)
			}
			if got, w := limiterState(lim), canon(t, r.State); got != w {
				t.Errorf("limiter %d op %d (%s): state %s, want %s", li, i, name, got, w)
			}
		}
	}
}

func toJS(x any) any {
	b, _ := json.Marshal(x)
	v, _ := jsjson.Parse(string(b))
	return v
}

func limiterState(l *LoginLimiter) string {
	entries, recent, active := l.State()
	row := func(e LimiterEntry) any { return []any{e.Key, e.Fails, float64(e.Until), float64(e.At)} }
	head, tail := []any{}, []any{}
	for i, e := range entries {
		if i < 3 {
			head = append(head, row(e))
		}
		if len(entries) > 3 && i >= len(entries)-3 {
			tail = append(tail, row(e))
		}
	}
	return jsjson.MustStringify(jsjson.Object{{Key: "n", Value: len(entries)}, {Key: "head", Value: head}, {Key: "tail", Value: tail}, {Key: "recent", Value: recent}, {Key: "active", Value: active}})
}

func runStore(t *testing.T, si int, script [][]json.RawMessage, result func(int) (json.RawMessage, []string)) {
	var tm int64
	var ids []string
	ref := func(s string) string {
		for i, id := range ids {
			if s == id {
				return "id" + strconv.Itoa(i)
			}
		}
		for i, id := range ids {
			if s == keyOf(id) {
				return "key(id" + strconv.Itoa(i) + ")"
			}
		}
		return s
	}
	resolve := func(s string) string {
		if m := regexp.MustCompile(`^key\(id(\d+)\)$`).FindStringSubmatch(s); m != nil {
			n, _ := strconv.Atoi(m[1])
			return keyOf(ids[n])
		}
		if m := regexp.MustCompile(`^id(\d+)$`).FindStringSubmatch(s); m != nil {
			n, _ := strconv.Atoi(m[1])
			return ids[n]
		}
		return s
	}
	var revoked []string
	store := NewSessionStore(func(k string) { revoked = append(revoked, ref(k)) }, func() int64 { return tm })
	sess := func(x *Session) string {
		if x == nil {
			return "null"
		}
		return jsjson.MustStringify(jsjson.Object{{Key: "username", Value: x.Username}, {Key: "role", Value: string(x.Role)}, {Key: "key", Value: ref(x.Key)}, {Key: "created", Value: float64(x.Created)}, {Key: "lastSeen", Value: float64(x.LastSeen)}})
	}
	str := func(raw json.RawMessage) string {
		var s *string
		must(t, json.Unmarshal(raw, &s))
		if s == nil {
			return ""
		}
		return resolve(*s)
	}
	for i, op := range script {
		revoked = []string{}
		var name string
		must(t, json.Unmarshal(op[0], &name))
		value := "null"
		switch name {
		case "now":
			must(t, json.Unmarshal(op[1], &tm))
		case "create":
			var u, r string
			must(t, json.Unmarshal(op[1], &u))
			must(t, json.Unmarshal(op[2], &r))
			id := store.Create(User{u, Role(r)})
			ids = append(ids, id)
			value = jsjson.MustStringify(jsjson.Object{{Key: "idLength", Value: len(id)}, {Key: "base64url", Value: regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(id)}})
		case "get":
			var touch bool
			must(t, json.Unmarshal(op[2], &touch))
			value = sess(store.Get(str(op[1]), touch))
		case "validKey":
			value = sess(store.ValidKey(str(op[1])))
		case "destroy":
			value = strconv.FormatBool(store.Destroy(str(op[1])))
		case "revokeUser":
			var u string
			must(t, json.Unmarshal(op[1], &u))
			value = strconv.Itoa(store.RevokeUser(u))
		}
		wv, wr := result(i)
		if w := canon(t, wv); value != w {
			t.Errorf("store %d op %d (%s): got %s, want %s", si, i, name, value, w)
		}
		if got, w := jsjson.MustStringify(toJS(revoked)), jsjson.MustStringify(toJS(wr)); got != w {
			t.Errorf("store %d op %d (%s): revoked %s, want %s", si, i, name, got, w)
		}
	}
}

func TestAuditContract(t *testing.T) {
	var v struct {
		Lines []struct {
			Record struct {
				Event            string
				User, IP, Target *string
			}
			Line  string
			Files []string
		}
		AuditDirs []struct {
			Name string
			Out  out
		}
		Prunes []struct {
			Days int
			Left []string
		}
	}
	if err := contract.Load("auth-audit.json", &v); err != nil {
		t.Fatal(err)
	}
	today := "audit-" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	tRE := regexp.MustCompile(`"t":"[^"]*"`)
	for _, c := range v.Lines {
		out := t.TempDir()
		r := c.Record
		must(t, NewAuditLog(out).Write(AuditRecord{Event: r.Event, User: r.User, IP: r.IP, Target: r.Target}))
		dir := filepath.Join(out, "logs", "audit")
		entries, _ := os.ReadDir(dir)
		var files []string
		for _, e := range entries {
			n := e.Name()
			if n == today {
				n = "<today>"
			}
			files = append(files, n)
		}
		b, _ := os.ReadFile(filepath.Join(dir, today))
		if got := tRE.ReplaceAllString(string(b), `"t":"<t>"`); got != c.Line || strings.Join(files, ",") != strings.Join(c.Files, ",") {
			t.Errorf("audit line %+v: got %q %v, want %q %v", r, got, files, c.Line, c.Files)
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Errorf("audit dir mode %v", fi.Mode().Perm())
		}
	}
	for _, c := range v.AuditDirs {
		out := t.TempDir()
		buildAuditDir(t, out, c.Name, today)
		err := NewAuditLog(out).Write(AuditRecord{Event: "logout"})
		checkOut(t, "audit dir "+c.Name, c.Out, "null", err, [2]string{out, "<out>"}, [2]string{today, "<today>"})
	}
	for _, c := range v.Prunes {
		out := t.TempDir()
		dir := filepath.Join(out, "logs", "audit")
		must(t, os.MkdirAll(dir, 0o700))
		for _, f := range []string{"audit-2000-01-01.jsonl", "audit-1999-12-31.jsonl", "audit-2999-01-01.jsonl", "audit-today.jsonl", "audit-2000-01-01.json", "other.txt", "audit-2000-1-01.jsonl"} {
			if f == "audit-today.jsonl" {
				f = today
			}
			must(t, os.WriteFile(filepath.Join(dir, f), nil, 0o644))
		}
		must(t, NewAuditLog(out).Prune(c.Days))
		entries, _ := os.ReadDir(dir)
		var left []string
		for _, e := range entries {
			n := e.Name()
			if n == today {
				n = "audit-today.jsonl"
			}
			left = append(left, n)
		}
		sort.Strings(left)
		if strings.Join(left, ",") != strings.Join(c.Left, ",") {
			t.Errorf("prune %d: got %v, want %v", c.Days, left, c.Left)
		}
	}
	must(t, NewAuditLog(t.TempDir()).Prune(90))
}

func buildAuditDir(t *testing.T, out, name, today string) {
	dir := filepath.Join(out, "logs", "audit")
	mk := func(mode os.FileMode) {
		must(t, os.MkdirAll(dir, 0o700))
		must(t, os.Chmod(dir, mode))
	}
	switch name {
	case "missing (created)":
	case "0700":
		mk(0o700)
	case "0750":
		mk(0o750)
	case "0600":
		mk(0o600)
	case "symlink":
		must(t, os.Mkdir(filepath.Join(out, "logs"), 0o755))
		must(t, os.Mkdir(filepath.Join(out, "real"), 0o700))
		must(t, os.Symlink(filepath.Join(out, "real"), dir))
	case "file 0644":
		mk(0o700)
		must(t, os.WriteFile(filepath.Join(dir, today), nil, 0o644))
		must(t, os.Chmod(filepath.Join(dir, today), 0o644))
	case "file symlink":
		mk(0o700)
		must(t, os.WriteFile(filepath.Join(out, "target"), nil, 0o600))
		must(t, os.Symlink(filepath.Join(out, "target"), filepath.Join(dir, today)))
	default:
		t.Fatalf("unknown audit dir scenario %s", name)
	}
}
