package collect

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"growth-lab/internal/civiltime"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/sqlitec"
)

// RawValue is a source cell as text; nil is NULL.
type RawValue = *string

// SelectResult summarizes one streamed query.
type SelectResult struct {
	Columns []string
	Rows    int
	Ms      float64
}

// Source streams query results row by row.
type Source interface {
	Dialect() Dialect
	Now() (string, error)
	// SelectStream calls onColumns before the first row and onRow per row; it stops when a callback fails.
	SelectStream(sql string, onRow func([]RawValue) error, onColumns func([]string) error) (SelectResult, error)
	Abort()
}

// SingleValue returns a single-row, single-column result.
func SingleValue(src Source, sql string) (string, error) {
	var value RawValue
	rows := 0
	_, err := src.SelectStream(sql, func(v []RawValue) error {
		rows++
		if rows > 1 {
			return fmt.Errorf("more than one row: %s", sql)
		}
		value = v[0]
		return nil
	}, nil)
	if err != nil {
		return "", err
	}
	if value == nil {
		return "", fmt.Errorf("no value: %s", sql)
	}
	return *value, nil
}

// SqliteSource reads a SQLite file.
type SqliteSource struct {
	Path string
}

func (s *SqliteSource) Dialect() Dialect { return "sqlite" }

func (s *SqliteSource) Now() (string, error) {
	db, err := sqlitec.Open(s.Path, true)
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, _, err := db.QueryJS("SELECT strftime('%Y-%m-%d %H:%M:%f', 'now', 'localtime') AS now")
	if err != nil {
		return "", err
	}
	return rows[0][0].(string), nil
}

func (s *SqliteSource) SelectStream(sql string, onRow func([]RawValue) error, onColumns func([]string) error) (SelectResult, error) {
	t0 := time.Now()
	db, err := sqlitec.Open(s.Path, true)
	if err != nil {
		return SelectResult{}, err
	}
	defer db.Close()
	st, err := db.Prepare(sql)
	if err != nil {
		return SelectResult{}, err
	}
	defer st.Finalize()
	cols := st.Columns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	if onColumns != nil {
		if err := onColumns(names); err != nil {
			return SelectResult{}, err
		}
	}
	rows := 0
	for {
		ok, err := st.Step()
		if err != nil {
			return SelectResult{}, err
		}
		if !ok {
			break
		}
		raw := make([]RawValue, len(cols))
		for i := range raw {
			if st.Value(i).Type == sqlitec.Blob {
				return SelectResult{}, fmt.Errorf("unsupported source value type: object")
			}
			v, err := st.Value(i).JS()
			if err != nil {
				return SelectResult{}, err
			}
			switch x := v.(type) {
			case nil:
			case string:
				raw[i] = &x
			case float64:
				s := jsjson.Number(x)
				raw[i] = &s
			}
		}
		if err := onRow(raw); err != nil {
			return SelectResult{}, err
		}
		rows++
	}
	return SelectResult{Columns: names, Rows: rows, Ms: msSince(t0)}, nil
}

func (s *SqliteSource) Abort() {}

func msSince(t0 time.Time) float64 {
	return float64(int64(float64(time.Since(t0).Microseconds())/1000 + 0.5))
}

// CollectError is a collection error.
type CollectError struct{ Msg string }

func (e *CollectError) Error() string { return e.Msg }

const commitEvery = 10000

var intRE = regexp.MustCompile(`^-?\d+$`)

// convert turns a source text value into the cell stored in the snapshot (numbers bind like JavaScript numbers).
func convert(c ColumnSpec, v RawValue, where string) (any, error) {
	if v == nil {
		return nil, nil
	}
	s := *v
	switch c.Kind {
	case "int":
		if !intRE.MatchString(s) {
			return nil, &CollectError{where + ": not an integer"}
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil && !strings.Contains(err.Error(), "range") {
			return nil, &CollectError{where + ": not an integer"}
		}
		if n > sqlitec.MaxSafe || n < -sqlitec.MaxSafe {
			return nil, &CollectError{where + ": integer outside the safe range"}
		}
		return n, nil
	case "ts":
		t, err := civiltime.Normalize(s)
		if err != nil {
			return nil, &CollectError{where + ": not a timestamp"}
		}
		return t, nil
	case "bool":
		if s == "0" || s == "1" {
			n, _ := strconv.ParseFloat(s, 64)
			return n, nil
		}
		return nil, &CollectError{where + ": not a bool (0/1)"}
	}
	if utf8.RuneCountInString(s) > c.MaxLength {
		return nil, &CollectError{fmt.Sprintf("%s: exceeds maxLength (%d)", where, c.MaxLength)}
	}
	return s, nil
}

func keyGreater(a, b []float64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// LocalNow is the local time in the engine timestamp form (millisecond precision).
func LocalNow() string {
	d := time.Now()
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%03d000", d.Year(), d.Month(), d.Day(), d.Hour(), d.Minute(), d.Second(), d.Nanosecond()/1e6)
}

// CollectResult is the temporary snapshot file and its times.
type CollectResult struct {
	TmpPath, Cutoff, StartedAt, FinishedAt string
}

// Collect builds the temporary snapshot file, one source query per table. Deletes it on failure.
func Collect(specs []TableSpec, source Source, snapshotsDir string, log func(string)) (res CollectResult, err error) {
	if log == nil {
		log = func(string) {}
	}
	startedAt := LocalNow()
	now, err := source.Now()
	if err != nil {
		return res, err
	}
	cutoff, err := civiltime.Normalize(now)
	if err != nil {
		return res, err
	}
	tmpPath := filepath.Join(snapshotsDir, fmt.Sprintf("tmp-%d-%d.sqlite", os.Getpid(), time.Now().UnixMilli()))
	db, err := sqlitec.Open(tmpPath, false)
	if err != nil {
		return res, err
	}
	defer func() {
		if err != nil {
			if db.InTransaction() {
				_ = db.Exec("ROLLBACK")
			}
			db.Close()
			os.Remove(tmpPath)
			os.Remove(tmpPath + "-journal")
		}
	}()
	if err = db.Exec("PRAGMA journal_mode = DELETE"); err != nil {
		return res, err
	}
	if err = db.Exec(EngineTablesSQL); err != nil {
		return res, err
	}
	for _, t := range specs {
		if err = db.Exec(RawDDL(t)); err != nil {
			return res, err
		}
	}
	for _, t := range specs {
		if err = collectTable(db, t, source, log); err != nil {
			return res, err
		}
	}
	db.Close()
	return CollectResult{TmpPath: tmpPath, Cutoff: cutoff, StartedAt: startedAt, FinishedAt: LocalNow()}, nil
}

func collectTable(db *sqlitec.DB, t TableSpec, source Source, log func(string)) error {
	keyIdx := make([]int, len(t.Key))
	for i, k := range t.Key {
		for j, c := range t.Columns {
			if c.As == k {
				keyIdx[i] = j
			}
		}
	}
	marks := make([]string, len(t.Columns))
	expected := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		marks[i] = "?"
		expected[i] = c.As
	}
	ins, err := db.Prepare("INSERT INTO " + t.Target + " VALUES (" + strings.Join(marks, ", ") + ")")
	if err != nil {
		return err
	}
	defer ins.Finalize()
	var prevKey []float64
	n := 0
	sawHeader := false
	if err := db.Exec("BEGIN"); err != nil {
		return err
	}
	res, err := source.SelectStream(SelectSQL(t, source.Dialect()), func(raw []RawValue) error {
		if len(raw) != len(t.Columns) {
			return &CollectError{t.Target + ": column count mismatch"}
		}
		row := make([]any, len(t.Columns))
		for i, c := range t.Columns {
			v, err := convert(c, raw[i], fmt.Sprintf("%s.%s (row %d)", t.Target, c.As, n+1))
			if err != nil {
				return err
			}
			row[i] = v
		}
		key := make([]float64, len(keyIdx))
		for i, idx := range keyIdx {
			if row[idx] == nil {
				return &CollectError{fmt.Sprintf("%s: key is NULL (row %d)", t.Target, n+1)}
			}
			key[i] = row[idx].(float64)
		}
		if prevKey != nil && !keyGreater(key, prevKey) {
			return &CollectError{fmt.Sprintf("%s: key order violation (row %d): ORDER BY not respected", t.Target, n+1)}
		}
		prevKey = key
		if _, err := ins.Run(row...); err != nil {
			return &CollectError{fmt.Sprintf("%s: insert failed (row %d): %s", t.Target, n+1, err.Error())}
		}
		n++
		if n%commitEvery == 0 {
			if err := db.Exec("COMMIT"); err != nil {
				return err
			}
			if err := db.Exec("BEGIN"); err != nil {
				return err
			}
		}
		return nil
	}, func(cols []string) error {
		sawHeader = true
		if strings.Join(cols, "\t") != strings.Join(expected, "\t") {
			return &CollectError{fmt.Sprintf("%s: header mismatch (%s ≠ %s)", t.Target, strings.Join(cols, ","), strings.Join(expected, ","))}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := db.Exec("COMMIT"); err != nil {
		return err
	}
	if !sawHeader && res.Rows > 0 {
		return &CollectError{t.Target + ": rows arrived without a header"}
	}
	if _, err := db.RunOnce("INSERT INTO r_collect_log (table_name, rows, ms, collected_at) VALUES (?, ?, ?, ?)", t.Target, float64(n), res.Ms, LocalNow()); err != nil {
		return err
	}
	log(fmt.Sprintf("%s: %d rows %sms", t.Target, n, jsjson.Number(res.Ms)))
	return nil
}
