package agent

import (
	"strings"

	"growth-lab/internal/query"
	"growth-lab/internal/sqlitec"
)

// MaxIndexLines caps the indexed-column list in a timeout reply.
const MaxIndexLines = 60

// IndexedColumns lists the CREATE INDEX indexes of the agent's copy as "table(col, col)" for tables with one of the
// prefixes, leaving out SQLite's own tables and blocked tables or columns. Nil when the file cannot be read.
func IndexedColumns(agentPath string, prefixes []string, blocked query.BlockedList) []string {
	db, err := sqlitec.Open(agentPath, true)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query("SELECT tbl_name, name FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL ORDER BY tbl_name, name")
	if err != nil {
		return nil
	}
	hidden := map[string]bool{}
	for _, t := range blocked.Tables {
		hidden[t] = true
	}
	for _, c := range blocked.Columns {
		hidden[c] = true
	}
	readable := func(t string) bool {
		if query.SystemTable(t) || hidden[t] {
			return false
		}
		for _, p := range prefixes {
			if strings.HasPrefix(t, p) {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	out := []string{}
	for _, r := range rows {
		table, name := r[0].Text, r[1].Text
		if !readable(table) {
			continue
		}
		info, err := db.Query(`PRAGMA index_info("` + strings.ReplaceAll(name, `"`, `""`) + `")`)
		if err != nil {
			return nil
		}
		cols := make([]string, 0, len(info))
		for _, c := range info {
			if c[2].Type != sqlitec.Text || hidden[table+"."+c[2].Text] {
				cols = nil
				break
			}
			cols = append(cols, c[2].Text)
		}
		line := table + "(" + strings.Join(cols, ", ") + ")"
		if len(cols) == 0 || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
		if len(out) == MaxIndexLines {
			break
		}
	}
	return out
}
