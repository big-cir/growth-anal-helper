package cdc

import (
	"strconv"
	"strings"
)

// Savepoint reports SAVEPOINT, RELEASE SAVEPOINT and ROLLBACK [WORK] TO, which change no data.
func Savepoint(q string) bool {
	f := strings.Fields(q)
	switch {
	case len(f) >= 2 && f[0] == "SAVEPOINT":
		return true
	case len(f) >= 3 && f[0] == "RELEASE" && f[1] == "SAVEPOINT":
		return true
	case len(f) >= 3 && f[0] == "ROLLBACK" && (f[1] == "TO" || f[1] == "WORK" && f[2] == "TO"):
		return true
	}
	return false
}

// SkipComments drops leading whitespace and comments so the first word is the statement's own.
// An executable comment /*!NNNNN … */ is read when the server runs it (no version, or one up to the server's).
func SkipComments(q string, version int) string {
	for {
		q = strings.TrimSpace(q)
		if strings.HasPrefix(q, "/*!") {
			body := strings.TrimLeft(q[3:], "0123456789")
			if digits := q[3 : len(q)-len(body)]; digits == "" || atoiOr(digits, version+1) <= version {
				q = body
				continue
			}
		}
		switch {
		case strings.HasPrefix(q, "/*"):
			end := strings.Index(q, "*/")
			if end < 0 {
				return ""
			}
			q = q[end+2:]
		case strings.HasPrefix(q, "--"), strings.HasPrefix(q, "#"):
			end := strings.IndexByte(q, '\n')
			if end < 0 {
				return ""
			}
			q = q[end+1:]
		default:
			return q
		}
	}
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
