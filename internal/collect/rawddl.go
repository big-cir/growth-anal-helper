package collect

import (
	"strings"

	"growth-lab/internal/assets"
)

// EngineTablesSQL creates the tables the engine adds to every snapshot.
var EngineTablesSQL = assets.EngineTablesSQL

var sqlType = map[Kind]string{"int": "INTEGER", "ts": "TEXT", "text": "TEXT", "bool": "INTEGER"}

// RawDDL builds the r_* table from the spec.
func RawDDL(t TableSpec) string {
	cols := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		nn := ""
		if has(t.Key, c.As) {
			nn = " NOT NULL"
		}
		cols[i] = c.As + " " + sqlType[c.Kind] + nn
	}
	return "CREATE TABLE " + t.Target + " (" + strings.Join(cols, ", ") + ", PRIMARY KEY (" + strings.Join(t.Key, ", ") + "))"
}
