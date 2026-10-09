package verify

import (
	"errors"
	"regexp"
	"strings"

	"growth-lab/internal/sqllint"
)

// sourceDeny: writes (MySQL allows WITH … UPDATE/DELETE), file writes, locks, delays, external reads.
var sourceDeny = map[string]bool{"UPDATE": true, "DELETE": true, "INTO": true, "OUTFILE": true, "DUMPFILE": true, "LOCK": true, "LOAD_FILE": true, "SLEEP": true, "BENCHMARK": true, "GET_LOCK": true, "HANDLER": true, "CALL": true}

// sourceSyntaxProblem blocks syntax where the source dialect and the tokenizer could disagree on statement boundaries.
func sourceSyntaxProblem(sql string) string {
	if strings.Contains(sql, `\`) {
		return "backslashes are not allowed"
	}
	var q byte
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if q != 0 {
			if c == q {
				if i+1 < len(sql) && sql[i+1] == q {
					i++
				} else {
					q = 0
				}
			}
			continue
		}
		switch {
		case c == '\'' || c == '"' || c == '`':
			q = c
		case c == '#' || (c == '-' && i+1 < len(sql) && sql[i+1] == '-') || (c == '/' && i+1 < len(sql) && sql[i+1] == '*'):
			return "comments are not allowed"
		case c == '[' || c == ']':
			return "brackets are not allowed"
		case c == '$':
			return "$ is not allowed"
		}
	}
	return ""
}

var (
	valueRE   = regexp.MustCompile(`^[0-9A-Za-z :._-]*$`)
	shareRE   = regexp.MustCompile(`(?i)^SHARE$`)
	trailSemi = regexp.MustCompile(`;[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*$`)
)

// BindSourceSQL checks cross-check SQL for the source DB and replaces parameters with string literals of engine values.
func BindSourceSQL(sql string, values map[string]string) (string, error) {
	if p := sourceSyntaxProblem(sql); p != "" {
		return "", errors.New(p)
	}
	keys := make([]string, 0, len(values))
	for _, k := range []string{"week_start", "week_end", "as_of"} {
		if _, ok := values[k]; ok {
			keys = append(keys, k)
		}
	}
	if lint := sqllint.Lint(sql, keys); !lint.OK {
		return "", errors.New(lint.Message)
	}
	toks, _ := sqllint.Tokenize(sql)
	for i, t := range toks {
		if t.Kind != sqllint.Word || sql[t.Start] == '"' || sql[t.Start] == '`' || sql[t.Start] == '[' {
			continue
		}
		w := strings.ToUpper(t.Text)
		if sourceDeny[w] {
			return "", errors.New("word not allowed in source cross-check SQL: " + t.Text)
		}
		if w == "FOR" && i+1 < len(toks) && toks[i+1].Kind == sqllint.Word && shareRE.MatchString(toks[i+1].Text) {
			return "", errors.New("word not allowed in source cross-check SQL: FOR " + toks[i+1].Text)
		}
	}
	var out strings.Builder
	at := 0
	for _, t := range toks {
		if t.Kind != sqllint.Param {
			continue
		}
		v := values[t.Text[1:]]
		if !valueRE.MatchString(v) {
			return "", errors.New("invalid parameter value: " + t.Text)
		}
		out.WriteString(sql[at:t.Start] + "'" + v + "'")
		at = t.End
	}
	out.WriteString(sql[at:])
	return trailSemi.ReplaceAllString(out.String(), ""), nil
}
