// Package sqllint holds the static SQL checks.
package sqllint

import (
	"growth-lab/internal/jsstr"
	"regexp"
	"strings"
	"unicode/utf8"
)

// RulesVersion is bumped when the rules change.
const RulesVersion = 1

// Kind is a token kind.
type Kind int

// Token kinds.
const (
	Word Kind = iota
	Param
	Semi
	Other
)

// Token is one SQL token; Start and End are byte offsets in the SQL (quotes and brackets included).
type Token struct {
	Kind       Kind
	Text       string
	Start, End int
}

var (
	paramRE = regexp.MustCompile(`^[:@$?][A-Za-z0-9_]*`)
	wordRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
	nameRE  = regexp.MustCompile(`^[A-Za-z_]`)
)

// Tokenize splits SQL into tokens, or returns why it cannot (unterminated string, identifier or comment).
func Tokenize(sql string) ([]Token, string) {
	var toks []Token
	i := 0
	for i < len(sql) {
		c, size := utf8.DecodeRuneInString(sql[i:])
		var n byte
		if i+1 < len(sql) {
			n = sql[i+1]
		}
		switch {
		case jsstr.IsSpace(c):
			i += size
			continue
		case c == '-' && n == '-':
			if e := strings.IndexByte(sql[i:], '\n'); e < 0 {
				i = len(sql)
			} else {
				i += e + 1
			}
			continue
		case c == '/' && n == '*':
			e := strings.Index(sql[i+2:], "*/")
			if e < 0 {
				return nil, "unterminated comment"
			}
			i += 2 + e + 2
			continue
		case c == '\'' || c == '"' || c == '`':
			q := byte(c)
			j := i + 1
			for {
				if j >= len(sql) {
					if q == '\'' {
						return nil, "unterminated string"
					}
					return nil, "unterminated identifier"
				}
				if sql[j] == q {
					if j+1 < len(sql) && sql[j+1] == q {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if q == '\'' {
				toks = append(toks, Token{Other, "'", i, j + 1})
			} else {
				toks = append(toks, Token{Word, sql[i+1 : j], i, j + 1})
			}
			i = j + 1
			continue
		case c == '[':
			e := strings.IndexByte(sql[i:], ']')
			if e < 0 {
				return nil, "unterminated identifier"
			}
			toks = append(toks, Token{Word, sql[i+1 : i+e], i, i + e + 1})
			i += e + 1
			continue
		case c == ':' || c == '@' || c == '$' || c == '?':
			m := paramRE.FindString(sql[i:])
			toks = append(toks, Token{Param, m, i, i + len(m)})
			i += len(m)
			continue
		case c == ';':
			toks = append(toks, Token{Semi, ";", i, i + 1})
			i++
			continue
		}
		if w := wordRE.FindString(sql[i:]); w != "" {
			toks = append(toks, Token{Word, w, i, i + len(w)})
			i += len(w)
			continue
		}
		toks = append(toks, Token{Other, sql[i : i+size], i, i + size})
		i += size
	}
	return toks, ""
}

// Result is the lint outcome: the parameters used, or why the SQL is rejected.
type Result struct {
	OK      bool
	Params  []string
	Message string
}

func fail(msg string) Result { return Result{Message: msg} }

// Lint allows one SELECT/WITH statement (not recursive) using only :as_of and the allowed parameters.
func Lint(sql string, allowedParams []string) Result {
	if jsstr.U16Len(sql) > 64*1024 {
		return fail("SQL too long (64KB limit)")
	}
	toks, errMsg := Tokenize(sql)
	if errMsg != "" {
		return fail(errMsg)
	}
	var semis []int
	for i, t := range toks {
		if t.Kind == Semi {
			semis = append(semis, i)
		}
	}
	if len(semis) > 1 || (len(semis) == 1 && semis[0] != len(toks)-1) {
		return fail("one statement only (`;` only at the end)")
	}
	body := toks
	if len(semis) == 1 {
		body = toks[:len(toks)-1]
	}
	if len(body) == 0 {
		return fail("empty SQL")
	}
	first := ""
	if body[0].Kind == Word {
		first = strings.ToUpper(body[0].Text)
	}
	if first != "SELECT" && first != "WITH" {
		return fail("must start with SELECT or WITH")
	}
	if first == "WITH" && len(body) > 1 && body[1].Kind == Word && strings.ToUpper(body[1].Text) == "RECURSIVE" {
		return fail("WITH RECURSIVE is not allowed (use d_calendar_week for a weekly calendar)")
	}
	allowed := append([]string{"as_of"}, allowedParams...)
	var used []string
	for _, t := range body {
		if t.Kind != Param {
			continue
		}
		name := t.Text[1:]
		if t.Text[0] != ':' || !nameRE.MatchString(name) {
			return fail("parameters must be :name (" + t.Text + ")")
		}
		if !contains(allowed, name) {
			list := make([]string, 0, len(allowed))
			for _, p := range uniq(allowed) {
				list = append(list, ":"+p)
			}
			return fail("parameter not allowed: " + t.Text + " (allowed: " + strings.Join(list, ", ") + ")")
		}
		if !contains(used, name) {
			used = append(used, name)
		}
	}
	if used == nil {
		used = []string{}
	}
	return Result{OK: true, Params: used}
}

// CTENames returns candidate WITH names (lowercase): every `name [(cols…)] AS [NOT] [MATERIALIZED] (` match, ignoring scope.
func CTENames(sql string) []string {
	toks, errMsg := Tokenize(sql)
	var out []string
	if errMsg != "" {
		return out
	}
	at := func(i int) *Token {
		if i < len(toks) {
			return &toks[i]
		}
		return nil
	}
	isWord := func(i int, w string) bool {
		t := at(i)
		return t != nil && t.Kind == Word && strings.ToUpper(t.Text) == w
	}
	for i := range toks {
		if toks[i].Kind != Word {
			continue
		}
		j := i + 1
		if t := at(j); t != nil && t.Text == "(" {
			depth := 0
			for ; j < len(toks); j++ {
				if toks[j].Text == "(" {
					depth++
				} else if toks[j].Text == ")" {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			j++
		}
		if !isWord(j, "AS") {
			continue
		}
		j++
		if isWord(j, "NOT") {
			j++
		}
		if isWord(j, "MATERIALIZED") {
			j++
		}
		if t := at(j); t != nil && t.Text == "(" {
			name := strings.ToLower(toks[i].Text)
			if !contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func uniq(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}
