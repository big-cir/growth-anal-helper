//go:build integration

package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"growth-lab-cdc/internal/check"
	"growth-lab-cdc/internal/target"
)

// typed has date and json columns, and a table keyed by a date.
var typed = []string{"doc", "daily"}

var typedSchema = []string{
	`CREATE TABLE app.doc (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, day date DEFAULT NULL,
		body json DEFAULT NULL, meta json NOT NULL) ENGINE=InnoDB`,
	`CREATE TABLE app.daily (day date NOT NULL PRIMARY KEY, n int NOT NULL) ENGINE=InnoDB`,
}

func (h *harness) typedOpts() RunOptions {
	return RunOptions{Source: h.Src, Target: h.Dst, Database: "app", Tables: typed, SaveEvery: 200 * time.Millisecond,
		StopWait: 5 * time.Second, Logf: h.T.Logf}
}

// typedCheck compares the typed tables with writes stopped, in small chunks.
func (h *harness) typedCheck() *check.CheckReport {
	h.T.Helper()
	rep, err := check.Check(context.Background(), check.CheckOptions{Source: h.Src, Target: h.Dst, Database: "app", Tables: typed,
		WritesStopped: true, ChunkRows: 50, Wait: 3 * time.Second, Rounds: 3, RoundGap: 300 * time.Millisecond, Logf: h.T.Logf})
	if err != nil {
		h.T.Fatalf("check: %v", err)
	}
	return rep
}

// typedSame wants no difference in the typed tables, by check and by the JSON text and type on both sides.
func (h *harness) typedSame() {
	h.T.Helper()
	for name, t := range h.typedCheck().Tables {
		if t.Missing+t.Extra+t.Differ+t.Undecided > 0 || t.Unchecked {
			h.T.Fatalf("%s: missing %d extra %d differ %d undecided %d unchecked %v", name, t.Missing, t.Extra, t.Differ, t.Undecided, t.Unchecked)
		}
	}
	q := "SELECT id, IFNULL(CAST(day AS CHAR), '-'), IFNULL(CAST(body AS CHAR), '-'), IFNULL(JSON_TYPE(body), '-'), " +
		"CAST(meta AS CHAR), JSON_TYPE(meta) FROM app.doc ORDER BY id"
	a, b := h.typedRows(h.SrcDB, q), h.typedRows(h.DstDB, q)
	if len(a) != len(b) {
		h.T.Fatalf("doc: %d rows on the source, %d in the copy", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			h.T.Fatalf("doc row %d differs:\n%.200s\n%.200s", i, a[i], b[i])
		}
	}
}

func (h *harness) typedRows(db *sql.DB, q string) []string {
	h.T.Helper()
	rows, err := db.Query(q)
	if err != nil {
		h.T.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, day, body, bodyType, meta, metaType string
		if err := rows.Scan(&id, &day, &body, &bodyType, &meta, &metaType); err != nil {
			h.T.Fatal(err)
		}
		out = append(out, strings.Join([]string{id, day, body, bodyType, meta, metaType}, "|"))
	}
	return out
}

// typedWrites changes the typed tables on the source with the values the reader and the row match must carry.
func (h *harness) typedWrites(round int) {
	h.T.Helper()
	big := `{"items": [` + strings.TrimSuffix(strings.Repeat(`"항목 데이터 0123456789",`, 12000), ",") + `]}`
	for _, q := range []string{
		fmt.Sprintf(`INSERT INTO app.doc (day, body, meta) VALUES ('2024-02-29', '{"b": 2, "a": [1, 2.5, "x"], "r": %d}', '{"k": 1.0}')`, round),
		`INSERT INTO app.doc (day, body, meta) VALUES (NULL, NULL, 'null')`,
		`INSERT INTO app.doc (day, body, meta) VALUES ('1999-12-31', '"문자열 스칼라"', '[]')`,
		`INSERT INTO app.doc (day, body, meta) VALUES ('2000-01-01', '` + big + `', '{}')`,
		`INSERT INTO app.doc (day, body, meta) VALUES ('2001-01-01', '{"zz": 1, "a": {"y": [1, {"k": 2.0}], "b": 1.0}, "mm": "값"}', '[1, "a", null]')`,
		`UPDATE app.doc SET body = '{"a": 1.0, "한글": "값"}', meta = '{"z": null}' WHERE id % 3 = 1 AND JSON_TYPE(body) = 'OBJECT' AND CHAR_LENGTH(body) < 1000 AND body->'$.zz' IS NULL ORDER BY id LIMIT 5`,
		`UPDATE app.doc SET body = 'null' WHERE body IS NULL ORDER BY id LIMIT 2`,
		`UPDATE app.doc SET body = NULL, day = NULL WHERE JSON_TYPE(body) = 'NULL' ORDER BY id LIMIT 1`,
		`UPDATE app.doc SET day = '2030-12-31' WHERE CHAR_LENGTH(body) > 100000 ORDER BY id LIMIT 1`,
	} {
		h.Exec(h.SrcDB, q)
	}
	if round < 2 {
		h.Exec(h.SrcDB, `DELETE FROM app.doc WHERE CHAR_LENGTH(body) > 100000 ORDER BY id LIMIT 1`)
		h.Exec(h.SrcDB, `DELETE FROM app.doc WHERE JSON_TYPE(meta) = 'ARRAY' ORDER BY id LIMIT 1`)
	}
	for _, q := range []string{
		fmt.Sprintf(`INSERT INTO app.daily VALUES (DATE_ADD('2010-01-01', INTERVAL %d DAY), 1)`, 5000+round),
		`UPDATE app.daily SET n = n + 1 ORDER BY day LIMIT 10`,
		`DELETE FROM app.daily ORDER BY day DESC LIMIT 1`,
	} {
		h.Exec(h.SrcDB, q)
	}
	// an empty document: a NULL forced into a NOT NULL json column outside strict mode
	conn, err := h.SrcDB.Conn(context.Background())
	if err != nil {
		h.T.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{"SET SESSION sql_mode = ''", "INSERT IGNORE INTO app.doc (day, body, meta) VALUES (NULL, '{}', NULL)"} {
		if _, err := conn.ExecContext(context.Background(), q); err != nil {
			h.T.Fatalf("%s: %v", q, err)
		}
	}
}

func TestDateAndJSON(t *testing.T) {
	h := newHarness(t)
	h.freshForInit(true)
	for _, q := range typedSchema {
		h.Exec(h.SrcDB, q)
	}
	h.Exec(h.SrcDB, `INSERT INTO app.doc (day, body, meta) VALUES ('2020-05-01', '{"seed": 1.5}', '{"n": 10.0}'), (NULL, NULL, 'true')`)
	h.Exec(h.SrcDB, `INSERT INTO app.daily (day, n) WITH RECURSIVE s (i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM s WHERE i < 299)
		SELECT DATE_ADD('2010-01-01', INTERVAL i DAY), i FROM s`)

	// changes after the snapshot reach the copy through init's replay
	testHooks.initStep = func(s string) error {
		if s == "copied-first-table" {
			testHooks.initStep = nil
			h.typedWrites(0)
		}
		return nil
	}
	defer func() { testHooks.initStep = nil }()
	io := h.initOpts()
	io.Tables = typed
	if err := Init(context.Background(), io); err != nil {
		t.Fatalf("init: %v", err)
	}
	if p := h.phase(); p != target.PhaseReady {
		t.Fatalf("phase %q after init", p)
	}
	h.typedSame()

	// run applies inserts, updates and deletes, finding old rows by their json and date values
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() {
		_, err := Run(ctx, h.typedOpts())
		ch <- err
	}()
	for round := 1; round <= 3; round++ {
		h.typedWrites(round)
	}
	want := h.Boundary()
	deadline := time.Now().Add(30 * time.Second)
	for h.SavedOr().Position().Compare(want) < 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	if err := <-ch; err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.Saved().Position(); got.Compare(want) != 0 {
		t.Fatalf("stored %s, source %s", got, want)
	}
	// every kind of value is still there to compare
	for _, where := range []string{"CHAR_LENGTH(body) > 100000", "JSON_TYPE(meta) = 'ARRAY'", "JSON_TYPE(body) = 'STRING'",
		"JSON_TYPE(body) = 'NULL'", "body IS NULL", "JSON_TYPE(meta) = 'NULL'", "body->'$.a.b' IS NOT NULL", "day IS NULL"} {
		var n int
		if err := h.SrcDB.QueryRow("SELECT COUNT(*) FROM app.doc WHERE " + where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatalf("no row with %s left to compare", where)
		}
	}
	h.typedSame()

	// a missing row in a date-keyed table is found across chunks
	h.Exec(h.DstDB, "DELETE FROM app.daily WHERE day = '2010-03-15'")
	var missing int64
	for _, tc := range h.typedCheck().Tables {
		missing += tc.Missing
	}
	if missing != 1 {
		t.Fatalf("missing %d, want 1", missing)
	}
}
