package auth

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
)

type out struct {
	OK     bool            `json:"ok"`
	Value  json.RawMessage `json:"value"`
	Error  string          `json:"error"`
	Syntax bool            `json:"syntax"`
}

func canon(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return "undefined"
	}
	s, err := jsjson.Compact(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func accountsJS(list []Account) string {
	arr := make([]any, len(list))
	for i, a := range list {
		arr[i] = a.JS()
	}
	return jsjson.MustStringify(arr)
}

// checkOut compares a result with an expected outcome; value is the JSON of the result.
func checkOut(t *testing.T, label string, want out, value string, err error, placeholders ...[2]string) {
	t.Helper()
	if !want.OK {
		if err == nil {
			t.Errorf("%s: got %s, want error %q", label, value, want.Error)
			return
		}
		if want.Syntax {
			var se *JSONSyntaxError
			if !errors.As(err, &se) {
				t.Errorf("%s: got %v, want a JSON syntax error", label, err)
			}
			return
		}
		msg := err.Error()
		for _, p := range placeholders {
			msg = strings.ReplaceAll(msg, p[0], p[1])
		}
		if msg != want.Error {
			t.Errorf("%s: got error %q, want %q", label, msg, want.Error)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: got error %v, want %s", label, err, want.Value)
		return
	}
	if w := canon(t, want.Value); value != w {
		t.Errorf("%s: got %s, want %s", label, value, w)
	}
}

func TestPasswords(t *testing.T) {
	var v struct {
		Check []struct {
			Input string
			Out   out
		}
		NodeHashes []struct {
			Label, Password, HashHex string
			Verify                   []struct {
				Candidate string
				OK        bool
			}
		}
		Variants []struct {
			Label, HashHex  string
			OK, Other       bool
			KnownDifference bool
		}
	}
	if err := contract.Load("auth-passwords.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Check {
		err := CheckPassword(c.Input)
		checkOut(t, "check "+c.Input, c.Out, "undefined", err)
	}
	for _, h := range v.NodeHashes {
		for _, c := range h.Verify {
			ok, err := VerifyPassword(c.Candidate, unhex(t, h.HashHex))
			if err != nil || ok != c.OK {
				t.Errorf("stored hash %s, candidate %q: got %v (%v), want %v", h.Label, c.Candidate, ok, err, c.OK)
			}
		}
	}
	for _, x := range v.Variants {
		ok, err1 := VerifyPassword("correct horse battery", unhex(t, x.HashHex))
		other, err2 := VerifyPassword("wrong password!", unhex(t, x.HashHex))
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		if x.KnownDifference {
			if !x.OK || !x.Other || ok || other {
				t.Errorf("variant %s: recorded %v/%v (accepted by the old engine), now %v/%v (expected rejected)", x.Label, x.OK, x.Other, ok, other)
			}
			continue
		}
		if ok != x.OK || other != x.Other {
			t.Errorf("variant %s: got %v/%v, want %v/%v", x.Label, ok, other, x.OK, x.Other)
		}
	}
	t.Logf("%d checks, %d stored hashes, %d variants", len(v.Check), len(v.NodeHashes), len(v.Variants))
}

// Stored hashes made by this engine verify and keep the expected format.
func TestGoHashes(t *testing.T) {
	var stored []struct{ Label, Password, HashHex string }
	if err := contract.Load("auth-go-hashes.json", &stored); err != nil {
		t.Fatal(err)
	}
	type goHash struct{ Label, Password, Hash string }
	var hashes []goHash
	for _, h := range stored {
		hashes = append(hashes, goHash{h.Label, h.Password, unhex(t, h.HashHex)})
	}
	re := regexp.MustCompile(`^scrypt\$32768\$8\$1\$[A-Za-z0-9+/]{22}==\$[A-Za-z0-9+/]{86}==$`)
	for _, h := range hashes {
		ok, err := VerifyPassword(h.Password, h.Hash)
		bad, _ := VerifyPassword(h.Password+"x", h.Hash)
		if err != nil || !ok || bad || !re.MatchString(h.Hash) {
			t.Errorf("go hash %s: ok %v bad %v err %v format %v", h.Label, ok, bad, err, re.MatchString(h.Hash))
		}
	}
}

func TestAccountFiles(t *testing.T) {
	var v struct {
		Parse []struct {
			Content string
			Out     out
		}
		Format []struct {
			List []struct {
				Username, Role, Hash string
				Disabled             bool
				CreatedAt            string `json:"created_at"`
				ChangedAt            string `json:"changed_at"`
			}
			Text string
		}
		Fs []struct {
			Name string
			Out  out
		}
		Locks []struct {
			State    string
			Out      out
			LockLeft bool
		}
		Ops []struct {
			Label    string
			Out      out
			Accounts json.RawMessage
		}
	}
	if err := contract.Load("auth-accounts.json", &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Parse {
		dir := t.TempDir()
		must(t, os.Mkdir(filepath.Join(dir, "auth"), 0o700))
		must(t, os.WriteFile(filepath.Join(dir, "auth", "accounts.json"), []byte(c.Content), 0o600))
		list, err := ReadAccounts(dir)
		checkOut(t, "parse "+c.Content, c.Out, accountsJS(list), err)
	}
	for i, c := range v.Format {
		dir := t.TempDir()
		var list []Account
		for _, a := range c.List {
			list = append(list, Account{a.Username, Role(a.Role), a.Hash, a.Disabled, a.CreatedAt, a.ChangedAt})
		}
		if _, err := UpdateAccounts(dir, func([]Account) ([]Account, error) { return list, nil }); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(AccountsFile(dir))
		if string(b) != c.Text {
			t.Errorf("format %d: got %q, want %q", i, b, c.Text)
		}
	}
	for _, c := range v.Fs {
		dir := t.TempDir()
		buildFs(t, dir, c.Name)
		list, err := ReadAccounts(dir)
		checkOut(t, "fs "+c.Name, c.Out, accountsJS(list), err, [2]string{dir, "<out>"})
	}
	for _, c := range v.Locks {
		dir := t.TempDir()
		must(t, os.Mkdir(filepath.Join(dir, "auth"), 0o700))
		if text, ok := lockText(t, c.State); ok {
			must(t, os.WriteFile(filepath.Join(dir, "auth", ".lock"), []byte(text), 0o644))
		}
		list, err := UpdateAccounts(dir, func(l []Account) ([]Account, error) { return l, nil })
		checkOut(t, "lock "+c.State, c.Out, strconv.Itoa(len(list)), err, [2]string{dir, "<out>"})
		_, statErr := os.Stat(filepath.Join(dir, "auth", ".lock"))
		if (statErr == nil) != c.LockLeft {
			t.Errorf("lock %s: lock left %v, want %v", c.State, statErr == nil, c.LockLeft)
		}
	}
	dir := t.TempDir()
	role := func(r string) *Role { x := Role(r); return &x }
	yes := true
	hash := "scrypt$1$1$1$AA$AA"
	steps := map[string]func() error{
		"add alice admin":     func() error { return AddAccount(dir, "alice", "admin", "correct horse battery") },
		"add bob viewer":      func() error { return AddAccount(dir, "bob", "viewer", "가나다라마바사아자차카타") },
		"add alice again":     func() error { return AddAccount(dir, "alice", "viewer", "correct horse battery") },
		"add bad name":        func() error { return AddAccount(dir, "A", "viewer", "correct horse battery") },
		"add bad role":        func() error { return AddAccount(dir, "carol", "root", "correct horse battery") },
		"add short password":  func() error { return AddAccount(dir, "carol", "editor", "short") },
		"modify bob role":     func() error { return ModifyAccount(dir, "bob", Patch{Role: role("editor")}) },
		"modify bob disabled": func() error { return ModifyAccount(dir, "bob", Patch{Disabled: &yes}) },
		"modify nobody":       func() error { return ModifyAccount(dir, "nobody", Patch{Disabled: &yes}) },
		"modify bad role":     func() error { return ModifyAccount(dir, "bob", Patch{Role: role("root")}) },
		"modify hash":         func() error { return ModifyAccount(dir, "alice", Patch{Hash: &hash}) },
	}
	for _, c := range v.Ops {
		f := steps[c.Label]
		if f == nil {
			t.Fatalf("unknown step %s", c.Label)
		}
		err := f()
		checkOut(t, c.Label, c.Out, "undefined", err, [2]string{dir, "<out>"})
		list, rerr := ReadAccounts(dir)
		must(t, rerr)
		for i := range list {
			if strings.HasPrefix(list[i].Hash, "scrypt$32768") {
				list[i].Hash = "<hash>"
			}
			list[i].CreatedAt, list[i].ChangedAt = "<ts>", "<ts>"
		}
		if got, want := accountsJS(list), canon(t, c.Accounts); got != want {
			t.Errorf("%s: accounts %s, want %s", c.Label, got, want)
		}
	}
	t.Logf("%d parse, %d format, %d fs, %d locks, %d ops", len(v.Parse), len(v.Format), len(v.Fs), len(v.Locks), len(v.Ops))
}

func TestAccountTimestamps(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 6_000_000, time.UTC)
	now = func() time.Time { return fixed }
	defer func() { now = time.Now }()
	must(t, AddAccount(dir, "alice", "admin", "correct horse battery"))
	list, err := ReadAccounts(dir)
	must(t, err)
	if list[0].CreatedAt != "2024-01-02T03:04:05.006Z" || list[0].ChangedAt != list[0].CreatedAt {
		t.Fatal(list[0])
	}
}

func unhex(t *testing.T, s string) string {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const fsValid = `{"version":1,"accounts":[{"username":"alice","role":"admin","hash":"h","disabled":false,"created_at":"a","changed_at":"b"}]}`

func buildFs(t *testing.T, out, name string) {
	dir := filepath.Join(out, "auth")
	f := filepath.Join(dir, "accounts.json")
	mkdir := func(mode os.FileMode) {
		must(t, os.Mkdir(dir, 0o700))
		must(t, os.Chmod(dir, mode))
	}
	file := func(content string, mode os.FileMode) {
		must(t, os.WriteFile(f, []byte(content), 0o600))
		must(t, os.Chmod(f, mode))
	}
	switch name {
	case "no auth dir":
	case "empty auth dir":
		mkdir(0o700)
	case "valid file":
		mkdir(0o700)
		file(fsValid, 0o600)
	case "dir group-readable":
		mkdir(0o750)
		file(fsValid, 0o600)
	case "dir 0701":
		mkdir(0o701)
		file(fsValid, 0o600)
	case "file group-readable":
		mkdir(0o700)
		file(fsValid, 0o640)
	case "file 0400":
		mkdir(0o700)
		file(fsValid, 0o400)
	case "file is a directory":
		mkdir(0o700)
		must(t, os.Mkdir(f, 0o755))
	case "file is a symlink":
		mkdir(0o700)
		must(t, os.WriteFile(filepath.Join(dir, "target.json"), []byte(fsValid), 0o600))
		must(t, os.Symlink(filepath.Join(dir, "target.json"), f))
	case "dir is a symlink":
		must(t, os.Mkdir(filepath.Join(out, "real-auth"), 0o700))
		must(t, os.Symlink(filepath.Join(out, "real-auth"), dir))
	case "auth is a file":
		must(t, os.WriteFile(dir, []byte("x"), 0o644))
	case "invalid content":
		mkdir(0o700)
		file(`{"version":3}`, 0o600)
	default:
		t.Fatalf("unknown fs scenario %s", name)
	}
}

func deadPid(t *testing.T) int {
	cmd := exec.Command("true")
	must(t, cmd.Run())
	return cmd.ProcessState.Pid()
}

func lockText(t *testing.T, state string) (string, bool) {
	switch state {
	case "none":
		return "", false
	case "self":
		return fmt.Sprintf("%d 2024-01-01T00:00:00.000Z", os.Getpid()), true
	case "dead":
		return fmt.Sprintf("%d 2024-01-01T00:00:00.000Z", deadPid(t)), true
	case "garbage":
		return "not a pid", true
	case "empty":
		return "", true
	case "hex-dead":
		return fmt.Sprintf("0x%x x", deadPid(t)), true
	}
	t.Fatalf("unknown lock state %s", state)
	return "", false
}
