package snapshot

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"growth-lab/internal/collect"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/sqlitec"
)

// PseudonymizeError is a pseudonymization or verification failure.
type PseudonymizeError struct{ Msg string }

func (e *PseudonymizeError) Error() string { return e.Msg }

// Pseudonym range: large random integers, apart from real IDs.
const (
	PseudoMin = 1_000_000_000
	PseudoMax = 1 << 47
)

// EngineColumns are the tables and columns the engine creates (all ordinary).
var EngineColumns = map[string][]string{
	"snapshot_meta":     {"snapshot_id", "source_cutoff_at", "collection_started_at", "collection_finished_at", "raw_hash", "derived_hash", "roles_hash", "params_hash", "spec_hash", "ga4_spec_hash"},
	"snapshot_params":   {"key", "value"},
	"r_collect_log":     {"table_name", "rows", "ms", "collected_at", "dropped_after_cutoff", "nulled_after_cutoff"},
	"d_calendar_week":   {"week_start", "week_end"},
	"r_ga4_collect_log": {"report", "table_name", "rows", "row_count", "dropped_small", "dropped_unobserved", "dropped_unmapped", "dropped_collision", "suppressed_cells", "subject_to_thresholding", "data_loss_from_other_row", "sampled", "sampling_summary", "truncated", "schema_restricted", "empty_reason", "time_zone", "data_through", "calls", "quota_day_remaining", "quota_hour_remaining", "collected_at"},
}

// AgentExcludedTables are engine tables left out of the agent's copy.
var AgentExcludedTables = map[string]bool{"snapshot_meta": true, "snapshot_params": true, "r_collect_log": true, "r_ga4_collect_log": true}

type idCol struct{ table, column, domain string }

func q(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func userTables(db *sqlitec.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r[0].Text
	}
	return out, nil
}

type colInfo struct{ name, typ string }

func tableInfo(db *sqlitec.DB, table string) ([]colInfo, error) {
	rows, err := db.Query("PRAGMA table_info(" + q(table) + ")")
	if err != nil {
		return nil, err
	}
	out := make([]colInfo, len(rows))
	for i, r := range rows {
		out[i] = colInfo{r[1].Text, r[2].Text}
	}
	return out, nil
}

// CheckRoles matches every r_*·d_* column with a role declaration and returns the identifier columns.
func CheckRoles(db *sqlitec.DB, roles *collect.Roles) ([]idCol, error) {
	tables, err := userTables(db)
	if err != nil {
		return nil, err
	}
	var unknown []string
	for _, t := range tables {
		if _, eng := EngineColumns[t]; !eng && !strings.HasPrefix(t, "r_") && !strings.HasPrefix(t, "d_") {
			unknown = append(unknown, t)
		}
	}
	if len(unknown) > 0 {
		return nil, &PseudonymizeError{"only r_*, d_* and engine tables are allowed (cannot tell how to pseudonymize): " + strings.Join(unknown, ", ")}
	}
	actual := map[string]bool{}
	var missing []string
	var ids []idCol
	for _, t := range tables {
		cols, err := tableInfo(db, t)
		if err != nil {
			return nil, err
		}
		if want, eng := EngineColumns[t]; eng {
			names := make([]string, len(cols))
			for i, c := range cols {
				names[i] = c.name
			}
			if strings.Join(names, ",") != strings.Join(want, ",") {
				return nil, &PseudonymizeError{"columns of engine table " + t + " changed (derived SQL cannot change engine tables)"}
			}
			continue
		}
		for _, c := range cols {
			name := t + "." + c.name
			actual[name] = true
			role, ok := roles.Get(name)
			if !ok {
				missing = append(missing, name)
			} else if role.IsIdentifier() {
				ids = append(ids, idCol{t, c.name, role.Identifier})
			}
		}
	}
	var extra []string
	for _, k := range roles.Keys() {
		if !actual[k] {
			extra = append(extra, k)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "columns without a declared role: "+strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			parts = append(parts, "declarations for missing columns: "+strings.Join(extra, ", "))
		}
		return nil, &PseudonymizeError{strings.Join(parts, " / ")}
	}
	return ids, nil
}

func randomInt(min, max int64) int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(max-min))
	if err != nil {
		panic(err)
	}
	return min + n.Int64()
}

// Stats are the mapped values per domain and the copied rows per table.
type Stats struct {
	Domains map[string]int
	Tables  map[string]int
}

// Pseudonymize writes the agent's copy (identifier columns replaced 1:1 by random values per domain,
// private columns and engine bookkeeping tables removed) and the mapping file.
func Pseudonymize(srcPath, agentPath, mapPath string, roles *collect.Roles) (Stats, error) {
	src, err := sqlitec.Open(srcPath, true)
	if err != nil {
		return Stats{}, err
	}
	defer src.Close()
	ids, err := CheckRoles(src, roles)
	if err != nil {
		return Stats{}, err
	}

	var domains []string
	values := map[string][]int64{}
	seen := map[string]map[int64]bool{}
	for _, c := range ids {
		if seen[c.domain] == nil {
			seen[c.domain] = map[int64]bool{}
			domains = append(domains, c.domain)
		}
		st, err := src.Prepare(fmt.Sprintf("SELECT DISTINCT %s AS v FROM %s WHERE %s IS NOT NULL", q(c.column), q(c.table), q(c.column)))
		if err != nil {
			return Stats{}, err
		}
		for {
			ok, err := st.Step()
			if err != nil {
				st.Finalize()
				return Stats{}, err
			}
			if !ok {
				break
			}
			js, err := st.Value(0).JS()
			if err != nil {
				st.Finalize()
				return Stats{}, err
			}
			f, isNum := js.(float64)
			if !isNum || f != math.Trunc(f) || math.Abs(f) > sqlitec.MaxSafe {
				st.Finalize()
				return Stats{}, &PseudonymizeError{c.table + "." + c.column + ": non-integer ID value"}
			}
			v := int64(f)
			if !seen[c.domain][v] {
				seen[c.domain][v] = true
				values[c.domain] = append(values[c.domain], v)
			}
		}
		st.Finalize()
	}
	allReal := map[int64]bool{}
	for _, set := range seen {
		for v := range set {
			allReal[v] = true
		}
	}
	used := map[int64]bool{}
	perm := map[string]map[int64]int64{}
	for _, d := range domains {
		vals := values[d]
		for i := len(vals) - 1; i > 0; i-- {
			j := randomInt(0, int64(i+1))
			vals[i], vals[j] = vals[j], vals[i]
		}
		m := make(map[int64]int64, len(vals))
		for _, real := range vals {
			var p int64
			for {
				p = randomInt(PseudoMin, PseudoMax)
				if !used[p] && !allReal[p] {
					break
				}
			}
			used[p] = true
			m[real] = p
		}
		perm[d] = m
	}

	if err := writeMap(mapPath, domains, values, perm); err != nil {
		return Stats{}, err
	}
	tables, err := writeAgentCopy(src, agentPath, roles, ids, perm)
	if err != nil {
		return Stats{}, err
	}
	stats := Stats{Domains: map[string]int{}, Tables: tables}
	for d, m := range perm {
		stats.Domains[d] = len(m)
	}
	return stats, nil
}

func writeMap(path string, domains []string, values map[string][]int64, perm map[string]map[int64]int64) error {
	m, err := sqlitec.Open(path, false)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Exec("PRAGMA journal_mode = DELETE; CREATE TABLE pseudo_map (domain TEXT NOT NULL, real INTEGER NOT NULL, pseudo INTEGER NOT NULL, PRIMARY KEY (domain, real))"); err != nil {
		return err
	}
	if err := m.Exec("BEGIN"); err != nil {
		return err
	}
	ins, err := m.Prepare("INSERT INTO pseudo_map VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer ins.Finalize()
	for _, d := range domains {
		for _, real := range values[d] {
			if _, err := ins.Run(d, float64(real), float64(perm[d][real])); err != nil {
				return err
			}
		}
	}
	return m.Exec("COMMIT")
}

func writeAgentCopy(src *sqlitec.DB, agentPath string, roles *collect.Roles, ids []idCol, perm map[string]map[int64]int64) (map[string]int, error) {
	dst, err := sqlitec.Open(agentPath, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if dst.InTransaction() {
			_ = dst.Exec("ROLLBACK")
		}
		dst.Close()
	}()
	if err := dst.Exec("PRAGMA journal_mode = DELETE"); err != nil {
		return nil, err
	}
	isPublic := func(t, c string) bool {
		if _, eng := EngineColumns[t]; eng {
			return true
		}
		r, _ := roles.Get(t + "." + c)
		return r != collect.Private
	}
	tables, err := userTables(src)
	if err != nil {
		return nil, err
	}
	var keptOrder []string
	kept := map[string][]colInfo{}
	for _, t := range tables {
		if AgentExcludedTables[t] {
			continue
		}
		cols, err := tableInfo(src, t)
		if err != nil {
			return nil, err
		}
		var pub []colInfo
		for _, c := range cols {
			if isPublic(t, c.name) {
				pub = append(pub, c)
			}
		}
		if len(pub) > 0 {
			kept[t] = pub
			keptOrder = append(keptOrder, t)
		}
	}
	for _, t := range keptOrder {
		defs := make([]string, len(kept[t]))
		for i, c := range kept[t] {
			defs[i] = q(c.name)
			if c.typ != "" {
				defs[i] += " " + c.typ
			}
		}
		if err := dst.Exec("CREATE TABLE " + q(t) + " (" + strings.Join(defs, ", ") + ")"); err != nil {
			return nil, err
		}
	}
	ixRows, err := src.Query("SELECT name, tbl_name, sql FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL")
	if err != nil {
		return nil, err
	}
	var indexes []string
	for _, ix := range ixRows {
		cols, ok := kept[ix[1].Text]
		if !ok {
			continue
		}
		info, err := src.Query("PRAGMA index_info(" + q(ix[0].Text) + ")")
		if err != nil {
			return nil, err
		}
		all := true
		for _, c := range info {
			if c[2].Type == sqlitec.Null || !hasCol(cols, c[2].Text) {
				all = false
			}
		}
		if all {
			indexes = append(indexes, ix[2].Text)
		}
	}
	counts := map[string]int{}
	if err := dst.Exec("BEGIN"); err != nil {
		return nil, err
	}
	for _, t := range keptOrder {
		cols := kept[t]
		names := make([]string, len(cols))
		marks := make([]string, len(cols))
		mapAt := make([]map[int64]int64, len(cols))
		for i, c := range cols {
			names[i] = q(c.name)
			marks[i] = "?"
			for _, id := range ids {
				if id.table == t && id.column == c.name {
					mapAt[i] = perm[id.domain]
				}
			}
		}
		ins, err := dst.Prepare("INSERT INTO " + q(t) + " VALUES (" + strings.Join(marks, ", ") + ")")
		if err != nil {
			return nil, err
		}
		sel, err := src.Prepare("SELECT " + strings.Join(names, ", ") + " FROM " + q(t))
		if err != nil {
			ins.Finalize()
			return nil, err
		}
		n := 0
		err = func() error {
			defer ins.Finalize()
			defer sel.Finalize()
			for {
				ok, err := sel.Step()
				if err != nil {
					return err
				}
				if !ok {
					return nil
				}
				// values pass through as JavaScript values (numbers bind as REAL), so stored storage classes stay the same
				out := make([]any, len(cols))
				for i := range cols {
					v, err := sel.Value(i).JS()
					if err != nil {
						return err
					}
					if mapAt[i] != nil && v != nil {
						f, isNum := v.(float64)
						p, ok := mapAt[i][int64(f)]
						if !isNum || f != math.Trunc(f) || !ok {
							return &PseudonymizeError{t + "." + cols[i].name + ": value not in the mapping (wrong declared domain)"}
						}
						out[i] = float64(p)
						continue
					}
					out[i] = v
				}
				if _, err := ins.Run(out...); err != nil {
					return err
				}
				n++
			}
		}()
		if err != nil {
			return nil, err
		}
		counts[t] = n
	}
	if err := dst.Exec("COMMIT"); err != nil {
		return nil, err
	}
	for _, ix := range indexes {
		if err := dst.Exec(ix); err != nil {
			return nil, err
		}
	}
	if err := verifyCopy(src, dst, ids, counts); err != nil {
		return nil, err
	}
	return counts, nil
}

func hasCol(cols []colInfo, name string) bool {
	for _, c := range cols {
		if c.name == name {
			return true
		}
	}
	return false
}

func verifyCopy(src, dst *sqlitec.DB, ids []idCol, tables map[string]int) error {
	one := func(db *sqlitec.DB, sql string) (string, error) {
		rows, names, err := db.QueryJS(sql)
		if err != nil {
			return "", err
		}
		arr := make([]any, len(rows))
		for i, r := range rows {
			o := jsjson.Object{}
			for j, n := range names {
				o = append(o, jsjson.Member{Key: n, Value: r[j]})
			}
			arr[i] = o
		}
		return jsjson.MustStringify(arr), nil
	}
	names := make([]string, 0, len(tables))
	for t := range tables {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		rows, err := dst.Query("SELECT count(*) n FROM " + q(t))
		if err != nil {
			return err
		}
		if int(rows[0][0].Int) != tables[t] {
			return &PseudonymizeError{fmt.Sprintf("check failed: %s row count %d ≠ %d", t, rows[0][0].Int, tables[t])}
		}
	}
	for _, c := range ids {
		sql := fmt.Sprintf("SELECT c, count(*) k FROM (SELECT count(*) c FROM %s WHERE %s IS NOT NULL GROUP BY %s) GROUP BY c ORDER BY c", q(c.table), q(c.column), q(c.column))
		a, err := one(src, sql)
		if err != nil {
			return err
		}
		b, err := one(dst, sql)
		if err != nil {
			return err
		}
		if a != b {
			return &PseudonymizeError{"check failed: " + c.table + "." + c.column + " frequency distribution"}
		}
	}
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			a, b := ids[i], ids[j]
			if a.domain != b.domain {
				continue
			}
			sql := fmt.Sprintf("SELECT count(*) n FROM (SELECT DISTINCT %s v FROM %s) x JOIN (SELECT DISTINCT %s v FROM %s) y ON x.v = y.v", q(a.column), q(a.table), q(b.column), q(b.table))
			x, err := one(src, sql)
			if err != nil {
				return err
			}
			y, err := one(dst, sql)
			if err != nil {
				return err
			}
			if x != y {
				return &PseudonymizeError{"check failed: " + a.table + "." + a.column + " ↔ " + b.table + "." + b.column + " join"}
			}
		}
	}
	return nil
}
