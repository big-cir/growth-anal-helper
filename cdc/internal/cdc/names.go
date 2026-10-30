package cdc

import (
	"sort"
	"strings"
)

// TableNames is the sorted "db.table" list of the followed tables.
func TableNames(database string, tables []string) []string {
	out := make([]string, len(tables))
	for i, t := range tables {
		out[i] = database + "." + t
	}
	sort.Strings(out)
	return out
}

// QuoteName quotes an identifier for MySQL.
func QuoteName(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
