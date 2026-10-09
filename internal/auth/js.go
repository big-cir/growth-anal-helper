package auth

import (
	"errors"
	"growth-lab/internal/jsstr"
	"math"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"growth-lab/internal/jsjson"
)

// isoTime formats like Date.prototype.toISOString.
func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

var jsDecimalRE = regexp.MustCompile(`^[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?$`)

// jsNumber converts a string like Number(string); NaN when not a number.
func jsNumber(s string) float64 {
	s = jsstr.Trim(s)
	if s == "" {
		return 0
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base > 0 {
			f := 0.0
			for _, c := range s[2:] {
				d, err := strconv.ParseInt(string(c), base, 64)
				if err != nil {
					return math.NaN()
				}
				f = f*float64(base) + float64(d)
			}
			return f
		}
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if !jsDecimalRE.MatchString(s) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f
		}
		return math.NaN()
	}
	return f
}

// jsString converts a JSON value like String(value); nil means undefined.
func jsString(v any, present bool) string {
	if !present {
		return "undefined"
	}
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if math.IsNaN(x) {
			return "NaN"
		}
		if math.IsInf(x, 1) {
			return "Infinity"
		}
		if math.IsInf(x, -1) {
			return "-Infinity"
		}
		return jsjson.Number(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e, true)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// lenientBase64Prefix decodes [A-Za-z0-9+/=] leniently: stops at the first '=', drops a lone trailing char (accepts every stored hash).
func lenientBase64Prefix(s string) []byte {
	if i := strings.IndexByte(s, '='); i >= 0 {
		s = s[:i]
	}
	return lenientBase64(s)
}

func lenientBase64(s string) []byte {
	value := func(c byte) uint32 {
		switch {
		case c >= 'A' && c <= 'Z':
			return uint32(c - 'A')
		case c >= 'a' && c <= 'z':
			return uint32(c-'a') + 26
		case c >= '0' && c <= '9':
			return uint32(c-'0') + 52
		case c == '+':
			return 62
		}
		return 63
	}
	var out []byte
	var acc uint32
	n := 0
	for i := 0; i < len(s); i++ {
		acc = acc<<6 | value(s[i])
		n++
		if n == 4 {
			out = append(out, byte(acc>>16), byte(acc>>8), byte(acc))
			acc, n = 0, 0
		}
	}
	switch n {
	case 2:
		out = append(out, byte(acc>>4))
	case 3:
		out = append(out, byte(acc>>10), byte(acc>>2))
	}
	return out
}

var uvMessages = map[syscall.Errno][2]string{
	syscall.ENOENT:  {"ENOENT", "no such file or directory"},
	syscall.EACCES:  {"EACCES", "permission denied"},
	syscall.EEXIST:  {"EEXIST", "file already exists"},
	syscall.ENOTDIR: {"ENOTDIR", "not a directory"},
	syscall.EISDIR:  {"EISDIR", "illegal operation on a directory"},
	syscall.ELOOP:   {"ELOOP", "too many symbolic links encountered"},
	syscall.EPERM:   {"EPERM", "operation not permitted"},
}

// fsError words a file system error as `CODE: message, syscall 'path'`.
func fsError(op, path string, err error) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if m, ok := uvMessages[errno]; ok {
			return &FsError{Code: m[0], Msg: m[0] + ": " + m[1] + ", " + op + " '" + path + "'", Err: err}
		}
	}
	return err
}

// FsError is a file system error with a stable message.
type FsError struct {
	Code, Msg string
	Err       error
}

func (e *FsError) Error() string { return e.Msg }
func (e *FsError) Unwrap() error { return e.Err }
