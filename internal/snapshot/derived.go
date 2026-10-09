package snapshot

import (
	"growth-lab/internal/jsstr"
	"regexp"
	"strings"

	"growth-lab/internal/collect"
	"growth-lab/internal/sqlitec"
)

const schemaTable = "sqlite_master"

func isDerived(name *string) bool { return name != nil && strings.HasPrefix(*name, "d_") }

// DerivedAuthorizer lets derived.sql create and write d_* tables only (no views); other tables are read-only.
func DerivedAuthorizer(code int, a1, a2, dbName *string) int {
	if dbName != nil && *dbName != "main" {
		return sqlitec.Deny
	}
	switch code {
	case sqlitec.Select, sqlitec.Read, sqlitec.Function, sqlitec.Recursive, sqlitec.Transaction, sqlitec.Savepoint:
		return sqlitec.OK
	case sqlitec.Insert, sqlitec.Update, sqlitec.Delete:
		if isDerived(a1) || (a1 != nil && *a1 == schemaTable) {
			return sqlitec.OK
		}
		return sqlitec.Deny
	case sqlitec.CreateTable, sqlitec.DropTable:
		if isDerived(a1) {
			return sqlitec.OK
		}
		return sqlitec.Deny
	case sqlitec.CreateIndex, sqlitec.DropIndex:
		if isDerived(a2) {
			return sqlitec.OK
		}
		return sqlitec.Deny
	case sqlitec.Reindex:
		return sqlitec.OK
	}
	return sqlitec.Deny
}

var (
	commentRE = regexp.MustCompile(`--[^\n]*|/\*(?s:.)*?\*/`)
	targetRE  = regexp.MustCompile(`(?i)^` + jsstr.Space + `*(?:CREATE` + jsstr.Space + `+(?:TEMP\w*` + jsstr.Space + `+)?TABLE(?:` + jsstr.Space + `+IF` + jsstr.Space + `+NOT` + jsstr.Space + `+EXISTS)?|INSERT` + jsstr.Space + `+(?:OR` + jsstr.Space + `+\w+` + jsstr.Space + `+)?INTO|REPLACE` + jsstr.Space + "+INTO|UPDATE(?:" + jsstr.Space + `+OR` + jsstr.Space + `+\w+)?)` + jsstr.Space + "+[\"`\\[]?(d_[A-Za-z0-9_]*)")
)

// SplitStatements splits on ; outside strings, identifiers and comments.
func SplitStatements(sql string) []string {
	var out []string
	start, i := 0, 0
	at := func(k int) byte {
		if k < len(sql) {
			return sql[k]
		}
		return 0
	}
	for i < len(sql) {
		c, n := sql[i], at(i+1)
		switch {
		case c == '-' && n == '-':
			if e := strings.IndexByte(sql[i:], '\n'); e < 0 {
				i = len(sql)
			} else {
				i += e + 1
			}
		case c == '/' && n == '*':
			if e := strings.Index(sql[i+2:], "*/"); e < 0 {
				i = len(sql)
			} else {
				i += 2 + e + 2
			}
		case c == '\'' || c == '"' || c == '`' || c == '[':
			cl := c
			if c == '[' {
				cl = ']'
			}
			j := i + 1
			for j < len(sql) && !(sql[j] == cl && at(j+1) != cl) {
				if sql[j] == cl {
					j += 2
				} else {
					j++
				}
			}
			i = j + 1
		case c == ';':
			out = append(out, sql[start:i+1])
			start = i + 1
			i++
		default:
			i++
		}
	}
	if start < len(sql) && jsstr.Trim(sql[start:]) != "" {
		out = append(out, sql[start:])
	}
	kept := out[:0]
	for _, x := range out {
		body := jsstr.Trim(strings.TrimSuffix(jsstr.Trim(commentRE.ReplaceAllString(x, "")), ";"))
		if body != "" {
			kept = append(kept, x)
		}
	}
	return kept
}

// targetTable is the d_ table a statement writes (CREATE TABLE, INSERT INTO, UPDATE, REPLACE INTO).
func targetTable(stmt string) string {
	m := targetRE.FindStringSubmatch(commentRE.ReplaceAllString(stmt, " "))
	if m == nil {
		return ""
	}
	return m[1]
}

// TaintError: a table that reads private columns has public columns.
type TaintError struct{ Msg string }

func (e *TaintError) Error() string { return e.Msg }

// ExecDerivedSQL runs derived SQL statement by statement, recording the private columns each target table reads.
// A table that reads any private column must declare all its columns private.
func ExecDerivedSQL(db *sqlitec.DB, sql string, roles *collect.Roles) error {
	var readsPrivate []string
	db.SetAuthorizer(func(code int, a1, a2, dbName, _ *string) int {
		r := DerivedAuthorizer(code, a1, a2, dbName)
		if r == sqlitec.OK && code == sqlitec.Read && roles != nil && a1 != nil && a2 != nil && *a1 != "" && *a2 != "" {
			if role, ok := roles.Get(*a1 + "." + *a2); ok && role == collect.Private {
				readsPrivate = append(readsPrivate, *a1+"."+*a2)
			}
		}
		return r
	})
	type taint struct {
		table string
		from  []string
	}
	var tainted []*taint
	err := func() error {
		defer db.SetAuthorizer(nil)
		for _, stmt := range SplitStatements(sql) {
			readsPrivate = nil
			if err := db.Exec(stmt); err != nil {
				return err
			}
			t := targetTable(stmt)
			if t == "" || len(readsPrivate) == 0 {
				continue
			}
			var e *taint
			for _, x := range tainted {
				if x.table == t {
					e = x
				}
			}
			if e == nil {
				e = &taint{table: t}
				tainted = append(tainted, e)
			}
			for _, r := range readsPrivate {
				if !contains(e.from, r) {
					e.from = append(e.from, r)
				}
			}
		}
		return nil
	}()
	if err != nil || roles == nil {
		return err
	}
	var problems []string
	for _, x := range tainted {
		rows, err := db.Query(`PRAGMA table_info("` + x.table + `")`)
		if err != nil {
			return err
		}
		var pub []string
		for _, r := range rows {
			col := r[1].Text
			if role, ok := roles.Get(x.table + "." + col); !ok || role != collect.Private {
				pub = append(pub, x.table+"."+col)
			}
		}
		if len(pub) > 0 {
			problems = append(problems, x.table+" reads private columns ("+strings.Join(x.from, ", ")+"), so all its columns must be private: "+strings.Join(pub, ", "))
		}
	}
	if len(problems) > 0 {
		return &TaintError{strings.Join(problems, " / ")}
	}
	return nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
