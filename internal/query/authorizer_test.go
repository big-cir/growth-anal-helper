package query

import (
	"path/filepath"
	"testing"

	"growth-lab/internal/sqlitec"
)

// statsFile is a small database with ANALYZE statistics on an indexed table s_doc.
func statsFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats.sqlite")
	db, err := sqlitec.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec(`CREATE TABLE s_doc (id INTEGER PRIMARY KEY, kind TEXT);
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 200) INSERT INTO s_doc SELECT i, 'k' || (i % 7) FROM n;
		CREATE INDEX s_doc_kind ON s_doc(kind);
		PRAGMA analysis_limit = 0; ANALYZE`); err != nil {
		t.Fatal(err)
	}
	return path
}

// A short prefix that also matches sqlite_ names still cannot read the statistics tables.
func TestStatisticsNotReadableWithShortPrefix(t *testing.T) {
	path := statsFile(t)
	for _, st := range []string{"sqlite_stat1", "sqlite_stat4", "SQLITE_STAT4"} {
		out := Run(&Input{Path: path, SQL: "SELECT * FROM " + st, ReadablePrefixes: []string{"s"}, HeapLimitMb: 64, MaxRows: 50, Overflow: "more", CellLimit: 4096, OutputLimit: 1 << 20})
		if ok, _ := out.Get("ok"); ok != false {
			t.Errorf("%s was readable", st)
		}
		if msg, _ := out.Get("message"); msg != "tables not readable: sqlite_stat1" && msg != "tables not readable: sqlite_stat4" {
			t.Errorf("%s: %v", st, msg)
		}
	}
	out := Run(&Input{Path: path, SQL: "SELECT count(*) FROM s_doc WHERE kind = 'k1'", ReadablePrefixes: []string{"s"}, HeapLimitMb: 64, MaxRows: 50, Overflow: "more", CellLimit: 4096, OutputLimit: 1 << 20})
	if ok, _ := out.Get("ok"); ok != true {
		t.Errorf("table read failed: %v", out)
	}
	seen := NewSeen()
	auth := Authorizer([]string{"s", "sql", "sqlite"}, seen, Blocked{}, nil)
	name := "sqlite_stat4"
	if auth(sqlitec.Read, &name, nil, nil, nil) != sqlitec.Ignore || !seen.Denied[name] {
		t.Errorf("authorizer allowed %s", name)
	}
}
