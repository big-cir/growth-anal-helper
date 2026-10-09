package auth

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"growth-lab/internal/jsjson"
)

// AccountUsage lists the account commands.
const AccountUsage = `account add <name> --role viewer|editor|admin
  account passwd <name>
  account role <name> <role>
  account disable <name> | account enable <name>
  account list
  account import          (apply accounts from accounts.input.json in the output folder and clear its passwords)`

// AccountsInput is the operator's input file in the output folder.
const AccountsInput = "accounts.input.json"

// ReadHidden reads a line typed in the terminal without echo.
type ReadHidden func(prompt string) (string, error)

func askNewPassword(read ReadHidden) (string, error) {
	a, err := read("New password (12+ characters): ")
	if err != nil {
		return "", err
	}
	b, err := read("Again: ")
	if err != nil {
		return "", err
	}
	if a != b {
		return "", &AccountError{"The two entries differ"}
	}
	return a, nil
}

func parseRole(v string, present bool) (Role, error) {
	if present && validRole(Role(v)) {
		return Role(v), nil
	}
	return "", &AccountError{"Role must be viewer|editor|admin"}
}

// Audited writes the audit record before the change (no change if that fails) and *_failed if the change fails.
func Audited(outDir, target string, fn func() error) error {
	log := NewAuditLog(outDir)
	user := os.Getenv("USER")
	if _, ok := os.LookupEnv("USER"); !ok {
		user = "cli"
	}
	if err := log.Write(AuditRecord{Event: "account_changed", User: &user, Target: &target}); err != nil {
		return err
	}
	if err := fn(); err != nil {
		log.TryWrite(AuditRecord{Event: "account_changed_failed", User: &user, Target: &target})
		return err
	}
	return nil
}

// AccountCommand runs `account <sub> …`; read nil uses the terminal.
func AccountCommand(outDir string, args []string, stdout, stderr io.Writer, read ReadHidden) int {
	if read == nil {
		read = TerminalHidden(stdout)
	}
	at := func(i int) (string, bool) {
		if i < len(args) {
			return args[i], true
		}
		return "", false
	}
	sub, _ := at(0)
	name, hasName := at(1)
	rest := []string{}
	if len(args) > 2 {
		rest = args[2:]
	}
	needName := func() error {
		if !hasName || name == "" {
			return &AccountError{"A name is required"}
		}
		return nil
	}
	err := func() error {
		switch sub {
		case "import":
			done, err := ImportAccounts(outDir)
			if err != nil {
				return err
			}
			if len(done) > 0 {
				fmt.Fprintf(stdout, "Applied: %s. Passwords in the input file were cleared\n", joinComma(done))
			} else {
				fmt.Fprintln(stdout, "No accounts with a password in the file")
			}
		case "list":
			list, err := ReadAccounts(outDir)
			if err != nil {
				return err
			}
			for _, a := range list {
				d := ""
				if a.Disabled {
					d = "\t(disabled)"
				}
				fmt.Fprintf(stdout, "%s\t%s%s\n", a.Username, a.Role, d)
			}
		case "add":
			var rv string
			has := false
			for i, x := range rest {
				if x == "--role" {
					if i+1 < len(rest) {
						rv, has = rest[i+1], true
					}
					break
				}
			}
			r, err := parseRole(rv, has)
			if err != nil {
				return err
			}
			if err := needName(); err != nil {
				return err
			}
			pw, err := askNewPassword(read)
			if err != nil {
				return err
			}
			if err := Audited(outDir, name, func() error { return AddAccount(outDir, name, r, pw) }); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Account created: %s (%s)\n", name, r)
		case "passwd":
			if err := needName(); err != nil {
				return err
			}
			pw, err := askNewPassword(read)
			if err != nil {
				return err
			}
			hash, err := HashPassword(pw)
			if err != nil {
				return err
			}
			if err := Audited(outDir, name, func() error { return ModifyAccount(outDir, name, Patch{Hash: &hash}) }); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "Password changed. All of that user's sessions are signed out")
		case "role":
			if err := needName(); err != nil {
				return err
			}
			v, has := "", len(rest) > 0
			if has {
				v = rest[0]
			}
			if err := Audited(outDir, name, func() error {
				r, err := parseRole(v, has)
				if err != nil {
					return err
				}
				return ModifyAccount(outDir, name, Patch{Role: &r})
			}); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "Role changed. All of that user's sessions are signed out")
		case "disable", "enable":
			if err := needName(); err != nil {
				return err
			}
			d := sub == "disable"
			if err := Audited(outDir, name, func() error { return ModifyAccount(outDir, name, Patch{Disabled: &d}) }); err != nil {
				return err
			}
			if d {
				fmt.Fprintln(stdout, "Account disabled. All of that user's sessions are signed out")
			} else {
				fmt.Fprintln(stdout, "Account enabled")
			}
		default:
			fmt.Fprintf(stderr, "Usage:\n  %s\n", AccountUsage)
			return errUsage
		}
		return nil
	}()
	if errors.Is(err, errUsage) {
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "account failed: %s\n", err.Error())
		return 1
	}
	return 0
}

var errUsage = errors.New("usage")

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

type inputEntry struct {
	Username string
	Role     Role
	Password string
}

// readInputFile: a regular file owned by me with 0600, no symlinks.
func readInputFile(file string) ([]inputEntry, error) {
	fd, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &AccountError{"Input file not found: " + file}
		}
		return nil, &AccountError{file + ": cannot open (symlinks not allowed)"}
	}
	defer fd.Close()
	st, err := fd.Stat()
	if err != nil {
		return nil, err
	}
	me := uid()
	if !st.Mode().IsRegular() || (me >= 0 && ownerOf(st) != me) || st.Mode().Perm()&0o077 != 0 {
		return nil, &AccountError{file + ": must be a file owned by me with 0600 (chmod 600)"}
	}
	b, err := io.ReadAll(fd)
	if err != nil {
		return nil, err
	}
	raw, err := jsjson.Parse(string(b))
	if err != nil {
		return nil, &JSONSyntaxError{err}
	}
	obj, _ := raw.(jsjson.Object)
	list, ok := get(obj, "accounts").([]any)
	if !ok {
		return nil, &AccountError{file + `: expected { "accounts": [...] }`}
	}
	out := make([]inputEntry, len(list))
	for i, a := range list {
		x, _ := a.(jsjson.Object)
		u, uok := get(x, "username").(string)
		p, pok := get(x, "password").(string)
		r, rok := get(x, "role").(string)
		if !uok || !pok || !rok || !validRole(Role(r)) {
			return nil, &AccountError{fmt.Sprintf("%s: accounts[%d] must be { username, role (viewer|editor|admin), password }", file, i)}
		}
		out[i] = inputEntry{u, Role(r), p}
	}
	return out, nil
}

// PendingInputPasswords reports whether the input file still has passwords (checked before the server starts).
func PendingInputPasswords(outDir string) bool {
	file := filepath.Join(outDir, AccountsInput)
	if _, err := os.Stat(file); err != nil {
		return false
	}
	list, err := readInputFile(file)
	if err != nil {
		return true
	}
	for _, a := range list {
		if a.Password != "" {
			return true
		}
	}
	return false
}

// ImportAccounts applies accounts with a password (new ones added, existing ones get role and password), then clears the passwords in the file.
func ImportAccounts(outDir string) ([]string, error) {
	file := filepath.Join(outDir, AccountsInput)
	input, err := readInputFile(file)
	if err != nil {
		return nil, err
	}
	var todo []inputEntry
	for _, a := range input {
		if a.Password != "" {
			todo = append(todo, a)
		}
	}
	for _, a := range todo {
		if err := CheckPassword(a.Password); err != nil {
			return nil, err
		}
	}
	done := []string{}
	for _, a := range todo {
		list, err := ReadAccounts(outDir)
		if err != nil {
			return nil, err
		}
		exists := false
		for _, x := range list {
			if x.Username == a.Username {
				exists = true
			}
		}
		if exists {
			hash, err := HashPassword(a.Password)
			if err != nil {
				return nil, err
			}
			role := a.Role
			if err := Audited(outDir, a.Username, func() error { return ModifyAccount(outDir, a.Username, Patch{Hash: &hash, Role: &role}) }); err != nil {
				return nil, err
			}
		} else if err := Audited(outDir, a.Username, func() error { return AddAccount(outDir, a.Username, a.Role, a.Password) }); err != nil {
			return nil, err
		}
		done = append(done, a.Username+"("+string(a.Role)+")")
	}
	cleared := make([]any, len(input))
	for i, a := range input {
		cleared[i] = jsjson.Object{{Key: "username", Value: a.Username}, {Key: "role", Value: string(a.Role)}, {Key: "password", Value: ""}}
	}
	tmp := file + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := writeNew(tmp, jsjson.Indent(jsjson.Object{{Key: "accounts", Value: cleared}}, 2)+"\n"); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, file); err != nil {
		return nil, fsError("rename", tmp, err)
	}
	return done, nil
}

// writeNew writes like writeFileSync(…, { mode: 0o600 }): the mode applies only when the file is created.
func writeNew(path, body string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fsError("open", path, err)
	}
	defer f.Close()
	_, err = f.WriteString(body)
	return err
}
