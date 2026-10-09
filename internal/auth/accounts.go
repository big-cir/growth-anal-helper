package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"

	"growth-lab/internal/jsjson"
)

// Role is an account role.
type Role string

// Roles in order of rights.
var Roles = []Role{"viewer", "editor", "admin"}

// Account is one entry of accounts.json.
type Account struct {
	Username  string
	Role      Role
	Hash      string
	Disabled  bool
	CreatedAt string
	ChangedAt string
}

// AccountError is shown to the operator as is.
type AccountError struct{ Msg string }

func (e *AccountError) Error() string { return e.Msg }

// UsernameRE is the allowed account name.
var UsernameRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{2,31}$`)

// Password limits: characters are code points, the maximum is UTF-8 bytes.
const (
	PasswordMinChars = 12
	PasswordMaxBytes = 256
)

const (
	scryptN       = 1 << 15
	scryptR       = 8
	scryptP       = 1
	scryptKeyLen  = 64
	scryptSaltLen = 16
)

// now is the clock for account timestamps.
var now = time.Now

// AuthDir is <outDir>/auth.
func AuthDir(outDir string) string { return filepath.Join(outDir, "auth") }

// AccountsFile is <outDir>/auth/accounts.json.
func AccountsFile(outDir string) string { return filepath.Join(AuthDir(outDir), "accounts.json") }

// CheckPassword enforces the password limits.
func CheckPassword(password string) error {
	if utf8.RuneCountInString(password) < PasswordMinChars {
		return &AccountError{fmt.Sprintf("Password must be at least %d characters", PasswordMinChars)}
	}
	if len(password) > PasswordMaxBytes {
		return &AccountError{fmt.Sprintf("Password must be at most %d bytes", PasswordMaxBytes)}
	}
	return nil
}

// HashPassword returns `scrypt$N$r$p$salt$key` (standard Base64).
func HashPassword(password string) (string, error) {
	if err := CheckPassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, scryptSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("scrypt$%d$%d$%d$%s$%s", scryptN, scryptR, scryptP, base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(key)), nil
}

var hashRE = regexp.MustCompile(`^scrypt\$(\d+)\$(\d+)\$(\d+)\$([A-Za-z0-9+/=]+)\$([A-Za-z0-9+/=]+)$`)

// VerifyPassword checks a password against a stored hash. A hash with an empty key never matches.
func VerifyPassword(password, hash string) (bool, error) {
	if len(password) > PasswordMaxBytes {
		return false, nil
	}
	m := hashRE.FindStringSubmatch(hash)
	if m == nil {
		return false, nil
	}
	if jsNumber(m[1]) != scryptN || jsNumber(m[2]) != scryptR || jsNumber(m[3]) != scryptP {
		return false, nil
	}
	want := lenientBase64Prefix(m[5])
	if len(want) == 0 {
		return false, nil
	}
	got, err := scrypt.Key([]byte(password), lenientBase64Prefix(m[4]), scryptN, scryptR, scryptP, len(want))
	if err != nil {
		return false, err
	}
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1, nil
}

func uid() int { return os.Getuid() }

func ownerOf(fi os.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// checkDir: a real directory (not a symlink), owned by me, 0700.
func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fsError("lstat", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return &AccountError{dir + ": not a directory (symlinks not allowed)"}
	}
	if uid() >= 0 && ownerOf(fi) != uid() {
		return &AccountError{dir + ": not owned by the current user"}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return &AccountError{dir + ": permissions must be 0700"}
	}
	return nil
}

// EnsureAuthDir creates the auth folder (0700) and checks it.
func EnsureAuthDir(outDir string) (string, error) {
	dir := AuthDir(outDir)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fsError("mkdir", dir, err)
		}
	}
	return dir, checkDir(dir)
}

// JSONSyntaxError is a broken accounts.json (the message comes from the JSON decoder).
type JSONSyntaxError struct{ Err error }

func (e *JSONSyntaxError) Error() string { return e.Err.Error() }

// ParseAccounts validates the account file content.
func ParseAccounts(text string) ([]Account, error) {
	raw, err := jsjson.Parse(text)
	if err != nil {
		return nil, &JSONSyntaxError{err}
	}
	if raw == nil {
		return nil, errors.New("Cannot read properties of null (reading 'version')")
	}
	o, _ := raw.(jsjson.Object)
	version, _ := o.Get("version")
	list, isArr := get(o, "accounts").([]any)
	if version != 1.0 || !isArr {
		return nil, &AccountError{"Invalid account file"}
	}
	seen := map[string]bool{}
	out := make([]Account, 0, len(list))
	for _, a := range list {
		if a == nil {
			return nil, errors.New("Cannot read properties of null (reading 'username')")
		}
		x, _ := a.(jsjson.Object)
		username, uok := get(x, "username").(string)
		role, _ := get(x, "role").(string)
		hash, hok := get(x, "hash").(string)
		disabled, dok := get(x, "disabled").(bool)
		if !uok || !UsernameRE.MatchString(username) || !validRole(Role(role)) || !hok || !dok {
			return nil, &AccountError{"Invalid account file entry"}
		}
		if seen[username] {
			return nil, &AccountError{"Duplicate account: " + username}
		}
		seen[username] = true
		ca, caok := x.Get("created_at")
		cha, chok := x.Get("changed_at")
		out = append(out, Account{Username: username, Role: Role(role), Hash: hash, Disabled: disabled, CreatedAt: jsString(ca, caok), ChangedAt: jsString(cha, chok)})
	}
	return out, nil
}

func get(o jsjson.Object, k string) any {
	v, _ := o.Get(k)
	return v
}

func validRole(r Role) bool {
	for _, x := range Roles {
		if x == r {
			return true
		}
	}
	return false
}

// JS returns the account as written to accounts.json.
func (a Account) JS() jsjson.Object {
	return jsjson.Object{{Key: "username", Value: a.Username}, {Key: "role", Value: string(a.Role)}, {Key: "hash", Value: a.Hash}, {Key: "disabled", Value: a.Disabled}, {Key: "created_at", Value: a.CreatedAt}, {Key: "changed_at", Value: a.ChangedAt}}
}

// FormatAccounts is the accounts.json content.
func FormatAccounts(list []Account) string {
	arr := make([]any, len(list))
	for i, a := range list {
		arr[i] = a.JS()
	}
	return jsjson.Indent(jsjson.Object{{Key: "version", Value: 1}, {Key: "accounts", Value: arr}}, 1) + "\n"
}

// ReadAccounts returns an empty list if the file is missing.
func ReadAccounts(outDir string) ([]Account, error) {
	dir := AuthDir(outDir)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return []Account{}, nil
	}
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	f := AccountsFile(outDir)
	fd, err := os.OpenFile(f, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return []Account{}, nil
		}
		if errors.Is(err, syscall.ELOOP) {
			return nil, &AccountError{f + ": symlinks are not allowed"}
		}
		return nil, fsError("open", f, err)
	}
	defer fd.Close()
	fi, err := fd.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, &AccountError{f + ": not a regular file"}
	}
	if uid() >= 0 && ownerOf(fi) != uid() {
		return nil, &AccountError{f + ": not owned by the current user"}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, &AccountError{f + ": permissions must be 0600"}
	}
	b, err := io.ReadAll(fd)
	if err != nil {
		return nil, err
	}
	return ParseAccounts(strings.ToValidUTF8(string(b), "�"))
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func openLock(lock string) (*os.File, error) {
	return os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

// UpdateAccounts changes the account list under a lock and replaces the file atomically.
func UpdateAccounts(outDir string, change func([]Account) ([]Account, error)) ([]Account, error) {
	dir, err := EnsureAuthDir(outDir)
	if err != nil {
		return nil, err
	}
	lock := filepath.Join(dir, ".lock")
	fd, err := openLock(lock)
	if err != nil {
		if !os.IsExist(err) {
			return nil, fsError("open", lock, err)
		}
		text, rerr := os.ReadFile(lock)
		if rerr != nil {
			return nil, fsError("open", lock, rerr)
		}
		pid := jsNumber(strings.Split(strings.ToValidUTF8(string(text), "�"), " ")[0])
		if pid == math.Trunc(pid) && pid > 0 && pid <= math.MaxInt32 && pidAlive(int(pid)) {
			return nil, &AccountError{"Another account change is in progress"}
		}
		_ = os.Remove(lock)
		if fd, err = openLock(lock); err != nil {
			return nil, fsError("open", lock, err)
		}
	}
	defer os.Remove(lock)
	_, werr := fd.WriteString(strconv.Itoa(os.Getpid()) + " " + isoTime(now()))
	fd.Close()
	if werr != nil {
		return nil, werr
	}
	current, err := ReadAccounts(outDir)
	if err != nil {
		return nil, err
	}
	next, err := change(current)
	if err != nil {
		return nil, err
	}
	target := AccountsFile(outDir)
	tmp := filepath.Join(dir, fmt.Sprintf(".accounts.%d.tmp", os.Getpid()))
	t, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fsError("open", tmp, err)
	}
	_, werr = t.WriteString(FormatAccounts(next))
	if werr == nil {
		werr = t.Sync()
	}
	t.Close()
	if werr != nil {
		return nil, werr
	}
	if err := os.Rename(tmp, target); err != nil {
		return nil, fsError("rename", tmp+"' -> '"+target, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return next, d.Sync()
}

// AddAccount creates an account.
func AddAccount(outDir, username string, role Role, password string) error {
	if !UsernameRE.MatchString(username) {
		return &AccountError{"Name must match ^[a-z][a-z0-9_.-]{2,31}$"}
	}
	if !validRole(role) {
		return &AccountError{"Role must be viewer|editor|admin"}
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	ts := isoTime(now())
	_, err = UpdateAccounts(outDir, func(list []Account) ([]Account, error) {
		for _, a := range list {
			if a.Username == username {
				return nil, &AccountError{"Account already exists: " + username}
			}
		}
		return append(list, Account{Username: username, Role: role, Hash: hash, CreatedAt: ts, ChangedAt: ts}), nil
	})
	return err
}

// Patch changes some fields of an account.
type Patch struct {
	Role     *Role
	Disabled *bool
	Hash     *string
}

// ModifyAccount applies a patch and updates changed_at.
func ModifyAccount(outDir, username string, p Patch) error {
	if p.Role != nil && !validRole(*p.Role) {
		return &AccountError{"Role must be viewer|editor|admin"}
	}
	_, err := UpdateAccounts(outDir, func(list []Account) ([]Account, error) {
		for i, a := range list {
			if a.Username != username {
				continue
			}
			next := append([]Account(nil), list...)
			if p.Role != nil {
				a.Role = *p.Role
			}
			if p.Disabled != nil {
				a.Disabled = *p.Disabled
			}
			if p.Hash != nil {
				a.Hash = *p.Hash
			}
			a.ChangedAt = isoTime(now())
			next[i] = a
			return next, nil
		}
		return nil, &AccountError{"No such account: " + username}
	})
	return err
}
