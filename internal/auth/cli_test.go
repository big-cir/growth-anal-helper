package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// typed answers prompts in order, echoing them like the terminal reader.
func typed(out *bytes.Buffer, answers ...string) ReadHidden {
	return func(prompt string) (string, error) {
		out.WriteString(prompt + "\n")
		if len(answers) == 0 {
			return "", &AccountError{"Cancelled"}
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
}

func run(t *testing.T, dir string, args []string, answers ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := AccountCommand(dir, args, &out, &errb, typed(&out, answers...))
	return code, out.String(), errb.String()
}

func TestAccountCommand(t *testing.T) {
	t.Setenv("USER", "tester")
	dir := t.TempDir()
	usage := "Usage:\n  " + AccountUsage + "\n"
	for _, c := range []struct {
		args    []string
		answers []string
		code    int
		out     string
		err     string
	}{
		{[]string{"list"}, nil, 0, "", ""},
		{nil, nil, 2, "", usage},
		{[]string{"nope"}, nil, 2, "", usage},
		{[]string{"add"}, nil, 1, "", "account failed: Role must be viewer|editor|admin\n"},
		{[]string{"add", "--role", "admin"}, nil, 1, "", "account failed: Role must be viewer|editor|admin\n"},
		{[]string{"add", "alice", "--role"}, nil, 1, "", "account failed: Role must be viewer|editor|admin\n"},
		{[]string{"add", "alice", "--role", "admin"}, []string{pwA, pwA}, 0, "New password (12+ characters): \nAgain: \nAccount created: alice (admin)\n", ""},
		{[]string{"add", "alice", "--role", "viewer"}, []string{pwA, pwA}, 1, "New password (12+ characters): \nAgain: \n", "account failed: Account already exists: alice\n"},
		{[]string{"add", "bob", "--role", "editor"}, []string{pwA, pwB}, 1, "New password (12+ characters): \nAgain: \n", "account failed: The two entries differ\n"},
		{[]string{"add", "bob", "--role", "editor"}, []string{"short", "short"}, 1, "New password (12+ characters): \nAgain: \n", "account failed: Password must be at least 12 characters\n"},
		{[]string{"add", "Bad Name", "--role", "viewer"}, []string{pwA, pwA}, 1, "New password (12+ characters): \nAgain: \n", "account failed: Name must match ^[a-z][a-z0-9_.-]{2,31}$\n"},
		{[]string{"add", "carol", "--role", "viewer"}, nil, 1, "New password (12+ characters): \n", "account failed: Cancelled\n"},
		{[]string{"add", "bob", "--role", "editor"}, []string{pwB, pwB}, 0, "New password (12+ characters): \nAgain: \nAccount created: bob (editor)\n", ""},
		{[]string{"role", "bob", "admin"}, nil, 0, "Role changed. All of that user's sessions are signed out\n", ""},
		{[]string{"role", "bob", "boss"}, nil, 1, "", "account failed: Role must be viewer|editor|admin\n"},
		{[]string{"role"}, nil, 1, "", "account failed: A name is required\n"},
		{[]string{"disable", "bob"}, nil, 0, "Account disabled. All of that user's sessions are signed out\n", ""},
		{[]string{"list"}, nil, 0, "alice\tadmin\nbob\tadmin\t(disabled)\n", ""},
		{[]string{"enable", "bob"}, nil, 0, "Account enabled\n", ""},
		{[]string{"disable", "nobody"}, nil, 1, "", "account failed: No such account: nobody\n"},
		{[]string{"passwd", "bob"}, []string{pwA, pwA}, 0, "New password (12+ characters): \nAgain: \nPassword changed. All of that user's sessions are signed out\n", ""},
		{[]string{"passwd", "nobody"}, []string{pwA, pwA}, 1, "New password (12+ characters): \nAgain: \n", "account failed: No such account: nobody\n"},
		{[]string{"passwd"}, nil, 1, "", "account failed: A name is required\n"},
	} {
		code, out, errs := run(t, dir, c.args, c.answers...)
		if code != c.code || out != c.out || errs != c.err {
			t.Errorf("%v: %d %q %q, want %d %q %q", c.args, code, out, errs, c.code, c.out, c.err)
		}
	}
	list, err := ReadAccounts(dir)
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	if ok, _ := VerifyPassword(pwA, list[1].Hash); !ok || list[1].Role != "admin" || list[1].Disabled {
		t.Fatalf("bob: %+v", list[1])
	}
	// every change is audited first (failed ones twice), with the operator's name
	files, _ := filepath.Glob(filepath.Join(dir, "logs", "audit", "*.jsonl"))
	b, _ := os.ReadFile(files[0])
	if n := strings.Count(string(b), `"event":"account_changed","user":"tester"`); n != 12 {
		t.Errorf("account_changed: %d\n%s", n, b)
	}
	if n := strings.Count(string(b), `"event":"account_changed_failed"`); n != 6 {
		t.Errorf("account_changed_failed: %d\n%s", n, b)
	}
	fi, _ := os.Stat(AccountsFile(dir))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("accounts mode %v", fi.Mode())
	}
}

func TestAccountImport(t *testing.T) {
	t.Setenv("USER", "tester")
	dir := t.TempDir()
	f := filepath.Join(dir, AccountsInput)
	write := func(text string, mode os.FileMode) {
		os.Remove(f)
		if err := os.WriteFile(f, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(f, mode)
	}
	if code, _, errs := run(t, dir, []string{"import"}); code != 1 || errs != "account failed: Input file not found: "+f+"\n" {
		t.Fatalf("missing: %d %q", code, errs)
	}
	write(`{"accounts":[]}`, 0o644)
	if code, _, errs := run(t, dir, []string{"import"}); code != 1 || errs != "account failed: "+f+": must be a file owned by me with 0600 (chmod 600)\n" || !PendingInputPasswords(dir) {
		t.Fatalf("loose mode: %d %q", code, errs)
	}
	write(`{"nope":1}`, 0o600)
	if _, _, errs := run(t, dir, []string{"import"}); errs != "account failed: "+f+`: expected { "accounts": [...] }`+"\n" {
		t.Fatal(errs)
	}
	write(`{"accounts":[{"username":"x","role":"boss","password":""}]}`, 0o600)
	if _, _, errs := run(t, dir, []string{"import"}); errs != "account failed: "+f+": accounts[0] must be { username, role (viewer|editor|admin), password }\n" {
		t.Fatal(errs)
	}
	write(`{"accounts":[{"username":"dave","role":"viewer","password":"short"}]}`, 0o600)
	if _, _, errs := run(t, dir, []string{"import"}); errs != "account failed: Password must be at least 12 characters\n" {
		t.Fatal(errs)
	}
	write(`{"accounts":[{"username":"dave","role":"viewer","password":"`+pwA+`"},{"username":"erin","role":"admin","password":""},{"username":"frank","role":"editor","password":"`+pwB+`"}]}`, 0o600)
	if !PendingInputPasswords(dir) {
		t.Fatal("pending before import")
	}
	if code, out, _ := run(t, dir, []string{"import"}); code != 0 || out != "Applied: dave(viewer), frank(editor). Passwords in the input file were cleared\n" {
		t.Fatalf("import: %d %q", code, out)
	}
	b, _ := os.ReadFile(f)
	want := "{\n  \"accounts\": [\n    {\n      \"username\": \"dave\",\n      \"role\": \"viewer\",\n      \"password\": \"\"\n    },\n    {\n      \"username\": \"erin\",\n      \"role\": \"admin\",\n      \"password\": \"\"\n    },\n    {\n      \"username\": \"frank\",\n      \"role\": \"editor\",\n      \"password\": \"\"\n    }\n  ]\n}\n"
	if string(b) != want || PendingInputPasswords(dir) {
		t.Fatalf("cleared file:\n%s", b)
	}
	if _, out, _ := run(t, dir, []string{"import"}); out != "No accounts with a password in the file\n" {
		t.Fatal(out)
	}
	// an existing account gets the new role and password
	write(`{"accounts":[{"username":"dave","role":"admin","password":"`+pwB+`"}]}`, 0o600)
	run(t, dir, []string{"import"})
	list, _ := ReadAccounts(dir)
	if ok, _ := VerifyPassword(pwB, list[0].Hash); !ok || list[0].Role != "admin" || len(list) != 2 {
		t.Fatalf("update: %+v", list)
	}
}

func TestTerminalHiddenNeedsATerminal(t *testing.T) {
	r, w, _ := os.Pipe()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close(); w.Close() }()
	var out bytes.Buffer
	if _, err := TerminalHidden(&out)("p: "); err == nil || err.Error() != "Passwords can only be typed in a terminal" || out.Len() != 0 {
		t.Fatalf("%v %q", err, out.String())
	}
}
