// Package query runs one SQL query under the engine's read policy.
package query

import (
	"strings"

	"growth-lab/internal/sqlitec"
)

// AllowedFunctions are the only SQL functions queries may call.
var AllowedFunctions = []string{
	// aggregates
	"count", "sum", "total", "avg", "min", "max", "group_concat",
	// window functions
	"row_number", "rank", "dense_rank", "lag", "lead", "first_value", "last_value", "ntile",
	// scalars (including LIKE and GLOB)
	"abs", "coalesce", "ifnull", "nullif", "iif", "round", "length", "lower", "upper", "substr", "trim", "instr", "replace", "like", "glob",
	// dates
	"date", "time", "datetime", "julianday", "strftime", "unixepoch",
}

var allowedFunctions = func() map[string]bool {
	m := map[string]bool{}
	for _, f := range AllowedFunctions {
		m[f] = true
	}
	return m
}()

// Seen records reads during preparation: allowed tables, denied tables, sensitive denials (table.column).
type Seen struct {
	Reads, Denied, Sensitive map[string]bool
}

// NewSeen returns empty sets.
func NewSeen() *Seen {
	return &Seen{Reads: map[string]bool{}, Denied: map[string]bool{}, Sensitive: map[string]bool{}}
}

// Blocked are the columns ("table.column") and tables denied as sensitive.
type Blocked struct {
	Columns, Tables map[string]bool
}

// Authorizer allows reads of tables with a readable prefix and the allowed functions only.
// Denied reads return IGNORE so preparation continues; the caller rejects the query before running it by checking seen.
// cteOnly: WITH names (lowercase) that are not real schema objects. count(*) over a materialized WITH result makes
// SQLite report a read of that name with column "" and DB NULL; it is not a real table, so it passes.
func Authorizer(readablePrefixes []string, seen *Seen, blocked Blocked, cteOnly map[string]bool) sqlitec.AuthFunc {
	readable := func(t *string) bool {
		if t == nil {
			return false
		}
		for _, p := range readablePrefixes {
			if strings.HasPrefix(*t, p) {
				return true
			}
		}
		return false
	}
	return func(code int, a1, a2, db, _ *string) int {
		switch code {
		case sqlitec.Select:
			return sqlitec.OK
		case sqlitec.Read:
			if a1 != nil && (blocked.Tables[*a1] || (a2 != nil && blocked.Columns[*a1+"."+*a2])) {
				if a2 != nil {
					seen.Sensitive[*a1+"."+*a2] = true
				} else {
					seen.Sensitive[*a1] = true
				}
				return sqlitec.Ignore
			}
			if a1 != nil && a2 != nil && *a2 == "" && db == nil && cteOnly[strings.ToLower(*a1)] {
				return sqlitec.OK
			}
			if readable(a1) {
				seen.Reads[*a1] = true
				return sqlitec.OK
			}
			if a1 == nil {
				return sqlitec.Deny
			}
			seen.Denied[*a1] = true
			return sqlitec.Ignore
		case sqlitec.Function:
			if a2 != nil && allowedFunctions[strings.ToLower(*a2)] {
				return sqlitec.OK
			}
			return sqlitec.Deny
		}
		return sqlitec.Deny
	}
}
