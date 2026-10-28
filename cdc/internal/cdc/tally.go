package cdc

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
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

// schemaChange marks statements after which the current primary keys may not describe older rows.
var schemaChange = map[string]bool{"CREATE": true, "ALTER": true, "DROP": true, "RENAME": true, "TRUNCATE": true}

// tally counts rows per table and statements per database; both are merged into the report only when the transaction commits.
type tally struct {
	filter   Filter
	selected map[string]TableInfo // "db.table"
	loc      *time.Location
	rep      *Report
	version  int // source server version, for executable comments

	inTxn          bool
	open           map[string]*Counts
	openStatements map[string]int64
}

func newTally(f Filter, selected map[string]TableInfo, loc *time.Location, version int, rep *Report) *tally {
	t := &tally{filter: f, selected: selected, loc: loc, version: version, rep: rep}
	rep.Tables, rep.Statements, rep.Hours = map[string]*Counts{}, map[string]int64{}, map[string]int64{}
	for name := range selected {
		rep.Tables[name] = &Counts{}
		rep.TablesSelected = append(rep.TablesSelected, name)
	}
	sort.Strings(rep.TablesSelected)
	t.reset()
	return t
}

func (t *tally) reset() {
	t.inTxn = false
	t.open = map[string]*Counts{}
	t.openStatements = map[string]int64{}
}

func (t *tally) begin() error {
	if t.inTxn {
		return &Error{"BEGIN inside an open transaction"}
	}
	t.reset()
	t.inTxn = true
	return nil
}

func (t *tally) rows(db, table string, kind replication.EnumRowsEventType, rows [][]any, full bool, size int64) error {
	key := db + "." + table
	info, ok := t.selected[key]
	if !ok {
		if t.filter.Table(db, table) {
			return &Error{key + ": table is not in the list made at the start (created or made visible during the read); run again"}
		}
		return nil
	}
	if !t.inTxn {
		return &Error{key + ": row event outside a transaction"}
	}
	if !full {
		return &Error{key + ": row image is not full (binlog_row_image must be FULL)"}
	}
	for _, r := range rows {
		if len(r) != info.Columns {
			return &Error{fmt.Sprintf("%s: row has %d columns but the table has %d now (schema changed in the range); run again from after the change", key, len(r), info.Columns)}
		}
	}
	c := t.open[key]
	if c == nil {
		c = &Counts{}
		t.open[key] = c
	}
	c.RowBytes += size
	switch kind {
	case replication.EnumRowsEventTypeInsert:
		c.Inserts += int64(len(rows))
	case replication.EnumRowsEventTypeDelete:
		c.Deletes += int64(len(rows))
	case replication.EnumRowsEventTypeUpdate:
		if len(rows)%2 != 0 {
			return &Error{key + ": update event has an odd number of row images"}
		}
		for i := 0; i < len(rows); i += 2 {
			changed, err := keyChanged(info.PK, rows[i], rows[i+1])
			if err != nil {
				return &Error{key + ": " + err.Error()}
			}
			if changed {
				c.KeyChanges++
			}
		}
		c.Updates += int64(len(rows) / 2)
	default:
		return &Error{key + ": unsupported row event"}
	}
	return nil
}

func keyChanged(pk []int, before, after []any) (bool, error) {
	if len(pk) == 0 {
		return false, &Error{"primary key unknown"}
	}
	for _, i := range pk {
		if i >= len(before) || i >= len(after) {
			return false, &Error{"primary key column missing from the row image"}
		}
		if !reflect.DeepEqual(before[i], after[i]) {
			return true, nil
		}
	}
	return false, nil
}

func (t *tally) commit(ts uint32, pos mysql.Position) {
	r := t.rep
	var target int64
	for name, c := range t.open {
		r.Tables[name].add(c)
		target += c.rows()
	}
	for k, n := range t.openStatements {
		r.Statements[k] += n
	}
	r.Commits++
	if target > 0 {
		r.TargetCommits++
		r.Hours[time.Unix(int64(ts), 0).In(t.loc).Format("2006-01-02 15:00")] += target
	}
	at := time.Unix(int64(ts), 0).In(t.loc)
	if r.FirstCommit == nil {
		r.FirstCommit = &at
	}
	r.LastCommit = &at
	r.Counted = pos
	t.reset()
}

func (t *tally) rollback(pos mysql.Position) {
	t.rep.Rollbacks++
	t.rep.Counted = pos
	t.reset()
}

// statement handles a Query event other than BEGIN, COMMIT and ROLLBACK. Statements on followed
// databases are counted, not interpreted; their row changes are not in the table counts.
// The database is the one selected when the statement ran.
func (t *tally) statement(db, q string, pos mysql.Position) error {
	q = skipComments(q, t.version)
	kw := keyword(q)
	if savepoint(q) {
		return nil
	}
	if !t.filter.Database(db) {
		if !t.inTxn {
			t.rep.Counted = pos
		}
		return nil
	}
	if schemaChange[kw] {
		return &Error{fmt.Sprintf("schema change (%s) in %s at %s; counts after it would use the wrong table layout, run again from after it", kw, db, pos)}
	}
	k := db + " " + kw
	if t.inTxn {
		t.openStatements[k]++
		return nil
	}
	t.rep.Statements[k]++
	t.rep.Counted = pos
	return nil
}

// savepoint reports SAVEPOINT, RELEASE SAVEPOINT and ROLLBACK [WORK] TO, which change no data.
func savepoint(q string) bool {
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

// skipComments drops leading whitespace and comments so the first word is the statement's own.
// An executable comment /*!NNNNN … */ is read when the server runs it (no version, or one up to the server's).
func skipComments(q string, version int) string {
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

// keyword is the leading word of a statement, letters only, so errors never carry the statement text.
func keyword(q string) string {
	end := 0
	for end < len(q) && end < 20 && (q[end] >= 'A' && q[end] <= 'Z' || q[end] == '_') {
		end++
	}
	if end == 0 {
		return "?"
	}
	return q[:end]
}

func (t *tally) finish() {
	if t.inTxn {
		t.rep.DroppedOpen = true
	}
	t.reset()
	var sum Counts
	for _, c := range t.rep.Tables {
		sum.add(c)
	}
	var missing []string
	for _, k := range []struct {
		name string
		n    int64
	}{{"insert", sum.Inserts}, {"update", sum.Updates}, {"delete", sum.Deletes}, {"key-change", sum.KeyChanges}} {
		if k.n == 0 {
			missing = append(missing, k.name)
		}
	}
	t.rep.NotObserved = missing
}

// handle applies one event; pos is the end of the outermost event and ts its header time.
func (t *tally) handle(ev *replication.BinlogEvent, ts uint32, pos mysql.Position) error {
	switch e := ev.Event.(type) {
	case *replication.RowsEvent:
		full := true
		for _, s := range e.SkippedColumns {
			if len(s) > 0 {
				full = false
			}
		}
		return t.rows(string(e.Table.Schema), string(e.Table.Table), e.Type(), e.Rows, full, int64(ev.Header.EventSize))
	case *replication.XIDEvent:
		if !t.inTxn {
			return &Error{"commit without BEGIN"}
		}
		t.commit(ts, pos)
	case *replication.QueryEvent:
		q := strings.ToUpper(strings.TrimSpace(string(e.Query)))
		switch q {
		case "BEGIN":
			return t.begin()
		case "COMMIT":
			if !t.inTxn {
				return &Error{"commit without BEGIN"}
			}
			t.commit(ts, pos)
		case "ROLLBACK":
			t.rollback(pos)
		default:
			return t.statement(string(e.Schema), q, pos)
		}
	case *replication.TransactionPayloadEvent:
		t.rep.Compressed++
		for _, inner := range e.Events {
			if err := t.handle(inner, ts, pos); err != nil {
				return err
			}
		}
	default:
		switch ev.Header.EventType {
		case replication.XA_PREPARE_LOG_EVENT, replication.PARTIAL_UPDATE_ROWS_EVENT:
			return &Error{fmt.Sprintf("unsupported event %s", ev.Header.EventType)}
		}
	}
	return nil
}
