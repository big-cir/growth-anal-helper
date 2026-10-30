package cdc

import (
	"fmt"
	"regexp"
	"strings"
)

// Filter picks what to follow, named after Debezium's database.include.list and table.include.list:
// comma-separated regular expressions matched against the whole name. Databases is required;
// with no Tables, every table of the matched databases is followed.
type Filter struct {
	Databases []string
	Tables    []string

	dbRe, tableRe []*regexp.Regexp
}

// systemDatabases are never followed, even when a pattern matches them.
var systemDatabases = map[string]bool{"mysql": true, "information_schema": true, "performance_schema": true, "sys": true}

// IsSystemDatabase reports one of the MySQL system databases.
func IsSystemDatabase(db string) bool { return systemDatabases[db] }

// ParseList splits a comma-separated flag value, trims each item and drops repeats.
func ParseList(flag, v string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, &Error{flag + ": empty item"}
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// NewFilter compiles the patterns.
func NewFilter(databases, tables []string) (Filter, error) {
	f := Filter{Databases: databases, Tables: tables}
	if len(databases) == 0 {
		return f, &Error{"--database-include-list is required"}
	}
	compile := func(flag string, ps []string) ([]*regexp.Regexp, error) {
		var out []*regexp.Regexp
		for _, p := range ps {
			re, err := regexp.Compile("^(?:" + p + ")$")
			if err != nil {
				return nil, &Error{fmt.Sprintf("%s: %q is not a valid regular expression", flag, p)}
			}
			out = append(out, re)
		}
		return out, nil
	}
	var err error
	if f.dbRe, err = compile("--database-include-list", databases); err != nil {
		return f, err
	}
	f.tableRe, err = compile("--table-include-list", tables)
	return f, err
}

func anyMatch(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// Database reports whether changes in the database are followed.
func (f Filter) Database(db string) bool { return !systemDatabases[db] && anyMatch(f.dbRe, db) }

// Table reports whether the table is followed.
func (f Filter) Table(db, table string) bool {
	return f.Database(db) && (len(f.tableRe) == 0 || anyMatch(f.tableRe, db+"."+table))
}

// TableInfo is what the read needs to know about a followed table.
type TableInfo struct {
	PK      []int // 0-based column positions
	Columns int
}
