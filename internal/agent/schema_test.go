package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"growth-lab/internal/sqlitec"
)

// The schema shown to the agent never lists SQLite statistics tables, even when a prefix matches them.
func TestSnapshotSchemaHidesStatistics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sqlite")
	db, err := sqlitec.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE d_doc (id INTEGER PRIMARY KEY, kind TEXT); CREATE TABLE s_member (id INTEGER PRIMARY KEY, app TEXT);
		INSERT INTO d_doc VALUES (1, 'a'), (2, 'b'); INSERT INTO s_member VALUES (1, 'x');
		CREATE INDEX d_doc_kind ON d_doc(kind); CREATE INDEX s_member_app ON s_member(app);
		PRAGMA analysis_limit = 0; ANALYZE`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, c := range []struct{ readable, panel []string }{
		{[]string{"s", "d_"}, []string{"d_"}},
		{[]string{"r_", "d_", "snapshot_"}, []string{"d_"}},
	} {
		text, tables, err := SnapshotSchema(path, c.readable, c.panel, "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, "sqlite_") {
			t.Errorf("%v: schema lists sqlite_ tables:\n%s", c.readable, text)
		}
		for _, n := range tables {
			if strings.HasPrefix(n, "sqlite_") {
				t.Errorf("%v: readable tables include %s", c.readable, n)
			}
		}
		if !strings.Contains(text, "d_doc") {
			t.Errorf("%v: d_doc missing", c.readable)
		}
	}
}
