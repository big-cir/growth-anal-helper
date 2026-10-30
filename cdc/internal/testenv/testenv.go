//go:build integration

// Package testenv sets up the source and copy servers for the integration tests. It needs
// CDC_SRC_CNF and CDC_DST_CNF, option files (root) for the source and the copy, and CDC_BIN.
package testenv

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	gosql "github.com/go-sql-driver/mysql"

	"growth-lab-cdc/internal/cdc"
	"growth-lab-cdc/internal/source"
	"growth-lab-cdc/internal/target"
)

var Followed = []string{"member", "post", "like_mark", "team_member", "label"}

// appSchema covers the column types run converts, a composite key and secondary unique keys.
var appSchema = []string{
	`CREATE TABLE app.member (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, created datetime(6) DEFAULT NULL,
		name varchar(50) COLLATE utf8mb4_general_ci DEFAULT NULL, handle varchar(100) COLLATE utf8mb4_general_ci DEFAULT NULL,
		locale enum('EN','KO','JA') COLLATE utf8mb4_general_ci NOT NULL, UNIQUE KEY handle (handle)) ENGINE=InnoDB`,
	`CREATE TABLE app.post (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, member_id bigint DEFAULT NULL,
		body varchar(2000) COLLATE utf8mb4_general_ci DEFAULT NULL, created datetime(6) DEFAULT NULL, changed datetime(6) DEFAULT NULL,
		KEY member (member_id), KEY body_prefix (body(20) DESC)) ENGINE=InnoDB`,
	`CREATE TABLE app.like_mark (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, member_id bigint NOT NULL, post_id bigint NOT NULL,
		created datetime NOT NULL, UNIQUE KEY member_post (member_id, post_id)) ENGINE=InnoDB`,
	`CREATE TABLE app.team_member (team_id bigint NOT NULL, member_id bigint NOT NULL, rank_no int NOT NULL DEFAULT 0,
		changed datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), PRIMARY KEY (team_id, member_id)) ENGINE=InnoDB`,
	`CREATE TABLE app.label (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, title varchar(256) COLLATE utf8mb4_general_ci NOT NULL,
		color tinyint NOT NULL, changed datetime(6) DEFAULT NULL, KEY color (color) INVISIBLE) ENGINE=InnoDB`,
}

// seed fills the followed tables.
var seed = []string{
	`INSERT INTO app.member (created, name, handle, locale) SELECT NOW(6), CONCAT('m', n), CONCAT('h', n), ELT(1 + n % 3, 'EN', 'KO', 'JA') FROM seq`,
	`INSERT INTO app.post (member_id, body, created, changed) SELECT n, CONCAT('Body ', n), NOW(6), IF(n % 2, NULL, NOW(6)) FROM seq`,
	`INSERT INTO app.like_mark (member_id, post_id, created) SELECT n, n + 1, NOW() FROM seq`,
	`INSERT INTO app.team_member (team_id, member_id, rank_no) SELECT n % 50, n, n % 7 FROM seq`,
	`INSERT INTO app.label (title, color) SELECT CONCAT('label ', n), n % 100 FROM seq WHERE n <= 300`,
}

// Env is a source and a copy server with the test tables.
type Env struct {
	T        *testing.T
	Src, Dst source.Source
	SrcDB    *sql.DB
	DstDB    *sql.DB
	From     *mysql.Position // the copy position after Reset, until a run takes it
	rng      *rand.Rand
	seq      int64
}

// New connects to both servers and builds the test tables, or skips when the environment is not set.
func New(t *testing.T) *Env {
	t.Helper()
	for _, k := range []string{"CDC_SRC_CNF", "CDC_DST_CNF", "CDC_BIN"} {
		if os.Getenv(k) == "" {
			t.Skip(k + " is not set")
		}
	}
	h := &Env{T: t, rng: rand.New(rand.NewSource(1))}
	var err error
	if h.Src, err = source.LoadSource(os.Getenv("CDC_SRC_CNF")); err != nil {
		t.Fatal(err)
	}
	if h.Dst, err = source.LoadSource(os.Getenv("CDC_DST_CNF")); err != nil {
		t.Fatal(err)
	}
	h.Reset()
	return h
}

// Reset rebuilds both sides: the source gets the tables and rows, the copy the same rows,
// and the copy position is the source position after the rows were written.
func (h *Env) Reset() {
	h.T.Helper()
	if h.SrcDB != nil {
		h.SrcDB.Close()
		h.DstDB.Close()
	}
	var err error
	if h.SrcDB, err = h.Src.Open(); err != nil {
		h.T.Fatal(err)
	}
	if h.DstDB, err = target.Open(h.Dst); err != nil {
		h.T.Fatal(err)
	}
	h.SrcDB.SetMaxOpenConns(4)
	for _, db := range []*sql.DB{h.SrcDB, h.DstDB} {
		for _, q := range []string{"DROP DATABASE IF EXISTS app", "DROP DATABASE IF EXISTS app_other", "DROP DATABASE IF EXISTS growth_lab_cdc",
			"CREATE DATABASE app", "CREATE DATABASE app_other",
			"CREATE TABLE app_other.note (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, v int) ENGINE=InnoDB"} {
			h.Exec(db, q)
		}
		for _, q := range appSchema {
			h.Exec(db, q)
		}
	}
	conn, err := h.SrcDB.Conn(context.Background())
	if err != nil {
		h.T.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "USE app"); err != nil {
		h.T.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TEMPORARY TABLE seq (n int PRIMARY KEY) SELECT 1 + a.i + 10 * b.i + 100 * c.i AS n FROM "+
		"(SELECT 0 i UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) a, "+
		"(SELECT 0 i UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) b, "+
		"(SELECT 0 i UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) c"); err != nil {
		h.T.Fatal(err)
	}
	for _, q := range seed {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			h.T.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "DROP TEMPORARY TABLE seq"); err != nil {
		h.T.Fatal(err)
	}
	// nothing else writes to the source, so its current position is the end of the seed
	var file string
	var pos uint32
	var x1, x2, x3 any
	if err := h.SrcDB.QueryRow("SHOW BINARY LOG STATUS").Scan(&file, &pos, &x1, &x2, &x3); err != nil {
		h.T.Fatal(err)
	}
	h.From = &mysql.Position{Name: file, Pos: pos}
	for _, tb := range Followed {
		h.CopyTable(tb)
	}
	// later reads of the newest file see only what the tests write
	h.Exec(h.SrcDB, "FLUSH BINARY LOGS")
}

func (h *Env) CopyTable(table string) {
	h.T.Helper()
	rows, err := h.SrcDB.Query("SELECT * FROM app." + table)
	if err != nil {
		h.T.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	marks := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
	for rows.Next() {
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			h.T.Fatal(err)
		}
		args := make([]any, len(cols))
		for i, v := range vals {
			if v != nil {
				args[i] = string(v)
			}
		}
		h.Exec(h.DstDB, "INSERT INTO app."+table+" VALUES ("+marks+")", args...)
	}
}

func (h *Env) Exec(db *sql.DB, q string, args ...any) {
	h.T.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		h.T.Fatalf("%s: %v", q, err)
	}
}

// Load writes n random changes to the source, some grouped in transactions, some outside the Followed tables.
func (h *Env) Load(n int) {
	h.T.Helper()
	ctx := context.Background()
	for i := 0; i < n; {
		tx, err := h.SrcDB.BeginTx(ctx, nil)
		if err != nil {
			h.T.Fatal(err)
		}
		for k := 1 + h.rng.Intn(4); k > 0 && i < n; k-- {
			h.Change(tx)
			i++
		}
		if err := tx.Commit(); err != nil {
			h.T.Fatal(err)
		}
	}
}

func (h *Env) Change(tx *sql.Tx) {
	h.T.Helper()
	if err := h.ChangeErr(tx); err != nil {
		h.T.Fatal(err)
	}
}

// ChangeErr writes one random change; it does not touch the test, so it can run off the test goroutine.
func (h *Env) ChangeErr(tx *sql.Tx) error {
	h.seq++
	big := 900_000_000 + h.seq
	locales := []string{"KO", "EN", "JA"}
	stmts := []struct {
		q    string
		args []any
	}{
		{"INSERT INTO app.member (created, name, handle, locale) VALUES (NOW(6), ?, ?, ?)",
			[]any{fmt.Sprintf("n%d", h.seq), fmt.Sprintf("s%d", h.seq), locales[h.rng.Intn(3)]}},
		{"UPDATE app.member SET name = ?, locale = ? ORDER BY RAND() LIMIT 1", []any{fmt.Sprintf("u%d", h.seq), locales[h.rng.Intn(3)]}},
		{"INSERT INTO app.post (member_id, body, created, changed) VALUES (?, ?, NOW(6), NOW(6))", []any{h.seq, "c"}},
		{"UPDATE app.post SET body = ?, changed = NOW(6) ORDER BY RAND() LIMIT 1", []any{fmt.Sprintf("Edit %d ", h.seq)}},
		{"DELETE FROM app.post ORDER BY RAND() LIMIT 1", nil},
		{"INSERT INTO app.like_mark (member_id, post_id, created) VALUES (?, ?, NOW())", []any{big, h.seq}},
		{"DELETE FROM app.like_mark ORDER BY RAND() LIMIT 1", nil},
		{"INSERT INTO app.team_member (team_id, member_id, rank_no) SELECT team_id, ?, 1 FROM app.team_member ORDER BY RAND() LIMIT 1", []any{big}},
		{"UPDATE app.team_member SET rank_no = rank_no + 1 ORDER BY RAND() LIMIT 1", nil},
		{"UPDATE app.team_member SET team_id = team_id + 1000 ORDER BY RAND() LIMIT 1", nil}, // key change
		{"UPDATE app.label SET title = ?, changed = NOW(6) ORDER BY RAND() LIMIT 1", []any{fmt.Sprintf("t%d", h.seq)}},
		{"INSERT INTO app_other.note (v) VALUES (?)", []any{h.seq}},
	}
	s := stmts[h.rng.Intn(len(stmts))]
	if _, err := tx.Exec(s.q, s.args...); err != nil {
		return fmt.Errorf("%s: %w", s.q, err)
	}
	return nil
}

// Background keeps writing changes until stop is called, which returns the first error.
func (h *Env) Background() (stop func() error) {
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		for {
			select {
			case <-done:
				result <- nil
				return
			default:
			}
			tx, err := h.SrcDB.Begin()
			if err == nil {
				for k := 1 + h.rng.Intn(4); k > 0 && err == nil; k-- {
					err = h.ChangeErr(tx)
				}
				if err == nil {
					err = tx.Commit()
				} else {
					tx.Rollback()
				}
			}
			if IsDeadlock(err) {
				continue // init's source lock can make a writer the deadlock victim; applications retry
			}
			if err != nil {
				result <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return func() error {
		close(done)
		return <-result
	}
}

func IsDeadlock(err error) bool {
	var me *gosql.MySQLError
	return errors.As(err, &me) && me.Number == 1213
}

// Boundary is the end of the last finished transaction on the source, read from SHOW BINLOG EVENTS.
// It fails when a source transaction is still open, since the end would not be final.
func (h *Env) Boundary() mysql.Position {
	h.T.Helper()
	trx := -1
	for i := 0; i < 50 && trx != 0; i++ {
		if err := h.SrcDB.QueryRow("SELECT COUNT(*) FROM information_schema.innodb_trx").Scan(&trx); err != nil {
			h.T.Fatal(err)
		}
		if trx != 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if trx != 0 {
		h.T.Fatalf("%d source transactions still open", trx)
	}
	st, err := h.Src.ReadStatus(context.Background())
	if err != nil {
		h.T.Fatal(err)
	}
	file := st.Current.Name
	rows, err := h.SrcDB.Query("SHOW BINLOG EVENTS IN '" + file + "'")
	if err != nil {
		h.T.Fatal(err)
	}
	defer rows.Close()
	// as inspect counts: a commit, a rollback, or a statement outside a transaction, up to the current position
	var end uint32
	inTx := false
	for rows.Next() {
		var logName, typ, info string
		var pos, next uint32
		var serverID uint64
		if err := rows.Scan(&logName, &pos, &typ, &serverID, &next, &info); err != nil {
			h.T.Fatal(err)
		}
		if next > st.Current.Pos {
			break
		}
		q := strings.ToUpper(strings.TrimSpace(info))
		switch {
		case typ == "Xid", typ == "Query" && (q == "COMMIT" || q == "ROLLBACK"):
			end, inTx = next, false
		case typ == "Query" && q == "BEGIN":
			inTx = true
		case typ == "Query" && !inTx:
			end = next
		}
	}
	if err := rows.Err(); err != nil {
		h.T.Fatal(err)
	}
	if end == 0 {
		h.T.Fatal("no transaction in the last file; write something after a rotate")
	}
	return mysql.Position{Name: file, Pos: end}
}

// SavedOr is the stored position, or none while run has not created it yet.
func (h *Env) SavedOr() cdc.Pos {
	var s string
	if err := h.DstDB.QueryRow("SELECT position FROM " + target.PositionTable).Scan(&s); err != nil {
		return cdc.Pos{}
	}
	p, _ := cdc.ParsePos(s)
	return p
}

func (h *Env) Saved() cdc.Pos {
	h.T.Helper()
	var s string
	if err := h.DstDB.QueryRow("SELECT position FROM " + target.PositionTable).Scan(&s); err != nil {
		h.T.Fatal(err)
	}
	p, err := cdc.ParsePos(s)
	if err != nil {
		h.T.Fatal(err)
	}
	return p
}

// Compare checks that every Followed table has the same rows on both sides, in key order.
func (h *Env) Compare() {
	h.T.Helper()
	for _, tb := range Followed {
		a, b := h.Dump(h.SrcDB, tb), h.Dump(h.DstDB, tb)
		if len(a) != len(b) {
			h.T.Fatalf("%s: %d rows on the source, %d in the copy", tb, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				h.T.Fatalf("%s: row %d differs", tb, i)
			}
		}
	}
}

func (h *Env) Dump(db *sql.DB, table string) []string {
	h.T.Helper()
	order := "id"
	if table == "team_member" {
		order = "team_id, member_id"
	}
	rows, err := db.Query("SELECT * FROM app." + table + " ORDER BY " + order)
	if err != nil {
		h.T.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			h.T.Fatal(err)
		}
		var b strings.Builder
		for _, v := range vals {
			fmt.Fprintf(&b, "%v|%q;", v.Valid, v.String)
		}
		out = append(out, b.String())
	}
	return out
}
