//go:build integration

// Integration tests for run against two local MySQL servers. They create and drop the databases
// app and app_other on both. They need:
//
//	CDC_SRC_CNF, CDC_DST_CNF  option files (root) for the source and the copy
//	CDC_BIN                   growth-lab-cdc binary, for the kill -9 cases
package cdc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
)

var followed = []string{"member", "post", "like_mark", "team_member", "label"}

// schema covers the column types run converts, a composite key and secondary unique keys.
var schema = []string{
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

var seed = []string{
	`INSERT INTO app.member (created, name, handle, locale) SELECT NOW(6), CONCAT('m', n), CONCAT('h', n), ELT(1 + n % 3, 'EN', 'KO', 'JA') FROM seq`,
	`INSERT INTO app.post (member_id, body, created, changed) SELECT n, CONCAT('Body ', n), NOW(6), IF(n % 2, NULL, NOW(6)) FROM seq`,
	`INSERT INTO app.like_mark (member_id, post_id, created) SELECT n, n + 1, NOW() FROM seq`,
	`INSERT INTO app.team_member (team_id, member_id, rank_no) SELECT n % 50, n, n % 7 FROM seq`,
	`INSERT INTO app.label (title, color) SELECT CONCAT('label ', n), n % 100 FROM seq WHERE n <= 300`,
}

type harness struct {
	t        *testing.T
	src, dst Source
	srcDB    *sql.DB
	dstDB    *sql.DB
	from     *mysql.Position
	rng      *rand.Rand
	seq      int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	for _, k := range []string{"CDC_SRC_CNF", "CDC_DST_CNF", "CDC_BIN"} {
		if os.Getenv(k) == "" {
			t.Skip(k + " is not set")
		}
	}
	h := &harness{t: t, rng: rand.New(rand.NewSource(1))}
	var err error
	if h.src, err = LoadSource(os.Getenv("CDC_SRC_CNF")); err != nil {
		t.Fatal(err)
	}
	if h.dst, err = LoadSource(os.Getenv("CDC_DST_CNF")); err != nil {
		t.Fatal(err)
	}
	h.reset()
	return h
}

// reset rebuilds both sides: the source gets the tables and rows, the copy the same rows,
// and the copy position is the source position after the rows were written.
func (h *harness) reset() {
	h.t.Helper()
	if h.srcDB != nil {
		h.srcDB.Close()
		h.dstDB.Close()
	}
	var err error
	if h.srcDB, err = h.src.open(); err != nil {
		h.t.Fatal(err)
	}
	if h.dstDB, err = openTarget(h.dst); err != nil {
		h.t.Fatal(err)
	}
	h.srcDB.SetMaxOpenConns(4)
	for _, db := range []*sql.DB{h.srcDB, h.dstDB} {
		for _, q := range []string{"DROP DATABASE IF EXISTS app", "DROP DATABASE IF EXISTS app_other", "DROP DATABASE IF EXISTS growth_lab_cdc",
			"CREATE DATABASE app", "CREATE DATABASE app_other",
			"CREATE TABLE app_other.note (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, v int) ENGINE=InnoDB"} {
			h.exec(db, q)
		}
		for _, q := range schema {
			h.exec(db, q)
		}
	}
	conn, err := h.srcDB.Conn(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "USE app"); err != nil {
		h.t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TEMPORARY TABLE seq (n int PRIMARY KEY) SELECT 1 + a.i + 10 * b.i + 100 * c.i AS n FROM "+
		"(SELECT 0 i UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) a, "+
		"(SELECT 0 i UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) b, "+
		"(SELECT 0 i UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) c"); err != nil {
		h.t.Fatal(err)
	}
	for _, q := range seed {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			h.t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "DROP TEMPORARY TABLE seq"); err != nil {
		h.t.Fatal(err)
	}
	// nothing else writes to the source, so its current position is the end of the seed
	var file string
	var pos uint32
	var x1, x2, x3 any
	if err := h.srcDB.QueryRow("SHOW BINARY LOG STATUS").Scan(&file, &pos, &x1, &x2, &x3); err != nil {
		h.t.Fatal(err)
	}
	h.from = &mysql.Position{Name: file, Pos: pos}
	for _, tb := range followed {
		h.copyTable(tb)
	}
	// later reads of the newest file see only what the tests write
	h.exec(h.srcDB, "FLUSH BINARY LOGS")
}

func (h *harness) copyTable(table string) {
	h.t.Helper()
	rows, err := h.srcDB.Query("SELECT * FROM app." + table)
	if err != nil {
		h.t.Fatal(err)
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
			h.t.Fatal(err)
		}
		args := make([]any, len(cols))
		for i, v := range vals {
			if v != nil {
				args[i] = string(v)
			}
		}
		h.exec(h.dstDB, "INSERT INTO app."+table+" VALUES ("+marks+")", args...)
	}
}

func (h *harness) opts() RunOptions {
	o := RunOptions{Source: h.src, Target: h.dst, Database: "app", Tables: followed, SaveEvery: 200 * time.Millisecond,
		StopWait: 5 * time.Second, Logf: h.t.Logf}
	if h.from != nil {
		o.From = h.from
		h.from = nil
	}
	return o
}

// start runs in the background; the channel gets its error.
func (h *harness) start(ctx context.Context) <-chan error {
	o := h.opts()
	ch := make(chan error, 1)
	go func() {
		_, err := Run(ctx, o)
		ch <- err
	}()
	return ch
}

// startBinary runs the command-line binary, for cases that kill the process.
func (h *harness) startBinary() *exec.Cmd {
	o := h.opts()
	args := []string{"run", "--defaults-file", os.Getenv("CDC_SRC_CNF"), "--target-defaults-file", os.Getenv("CDC_DST_CNF"),
		"--database", "app", "--tables", strings.Join(followed, ",")}
	if o.From != nil {
		args = append(args, "--from", fmt.Sprintf("%s:%d", o.From.Name, o.From.Pos))
	}
	cmd := exec.Command(os.Getenv("CDC_BIN"), args...)
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	return cmd
}

func (h *harness) exec(db *sql.DB, q string, args ...any) {
	h.t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		h.t.Fatalf("%s: %v", q, err)
	}
}

// load writes n random changes to the source, some grouped in transactions, some outside the followed tables.
func (h *harness) load(n int) {
	h.t.Helper()
	ctx := context.Background()
	for i := 0; i < n; {
		tx, err := h.srcDB.BeginTx(ctx, nil)
		if err != nil {
			h.t.Fatal(err)
		}
		for k := 1 + h.rng.Intn(4); k > 0 && i < n; k-- {
			h.change(tx)
			i++
		}
		if err := tx.Commit(); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) change(tx *sql.Tx) {
	h.t.Helper()
	if err := h.changeErr(tx); err != nil {
		h.t.Fatal(err)
	}
}

// changeErr writes one random change; it does not touch the test, so it can run off the test goroutine.
func (h *harness) changeErr(tx *sql.Tx) error {
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

// background keeps writing changes until stop is called, which returns the first error.
func (h *harness) background() (stop func() error) {
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
			tx, err := h.srcDB.Begin()
			if err == nil {
				for k := 1 + h.rng.Intn(4); k > 0 && err == nil; k-- {
					err = h.changeErr(tx)
				}
				if err == nil {
					err = tx.Commit()
				} else {
					tx.Rollback()
				}
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

// boundary is the end of the last finished transaction on the source, read with inspect.
// It fails when a source transaction is still open, since the end would not be final.
func (h *harness) boundary() mysql.Position {
	h.t.Helper()
	trx := -1
	for i := 0; i < 50 && trx != 0; i++ {
		if err := h.srcDB.QueryRow("SELECT COUNT(*) FROM information_schema.innodb_trx").Scan(&trx); err != nil {
			h.t.Fatal(err)
		}
		if trx != 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if trx != 0 {
		h.t.Fatalf("%d source transactions still open", trx)
	}
	f, err := NewFilter([]string{"app", "app_other"}, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	st, err := h.src.ReadStatus(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	rep, err := Inspect(context.Background(), InspectOptions{Source: h.src, Filter: f, FromFile: st.Files[len(st.Files)-1].Name, MaxBytes: 1 << 30, MaxDuration: time.Minute})
	if err != nil || !rep.Complete {
		h.t.Fatalf("inspect: %v %+v", err, rep)
	}
	if rep.Counted.Name == "" {
		h.t.Fatal("no transaction in the last file; write something after a rotate")
	}
	return rep.Counted
}

// savedOr is the stored position, or none while run has not created it yet.
func (h *harness) savedOr() binlogPos {
	var s string
	if err := h.dstDB.QueryRow("SELECT position FROM " + positionTable).Scan(&s); err != nil {
		return binlogPos{}
	}
	p, _ := parsePos(s)
	return p
}

func (h *harness) saved() binlogPos {
	h.t.Helper()
	var s string
	if err := h.dstDB.QueryRow("SELECT position FROM " + positionTable).Scan(&s); err != nil {
		h.t.Fatal(err)
	}
	p, err := parsePos(s)
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// finish runs until the copy reaches the source's last boundary, stops gracefully and compares.
func (h *harness) finish() {
	h.t.Helper()
	h.load(5)
	want := h.boundary()
	ctx, cancel := context.WithCancel(context.Background())
	ch := h.start(ctx)
	deadline := time.Now().Add(20 * time.Second)
	for h.savedOr().position().Compare(want) < 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	if err := <-ch; err != nil {
		h.t.Fatalf("run: %v", err)
	}
	if got := h.saved().position(); got.Compare(want) != 0 {
		h.t.Fatalf("stored position %s, source boundary %s", got, want)
	}
	h.compare()
}

// compare checks that every followed table has the same rows on both sides, in key order.
func (h *harness) compare() {
	h.t.Helper()
	for _, tb := range followed {
		a, b := h.dump(h.srcDB, tb), h.dump(h.dstDB, tb)
		if len(a) != len(b) {
			h.t.Fatalf("%s: %d rows on the source, %d in the copy", tb, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				h.t.Fatalf("%s: row %d differs", tb, i)
			}
		}
	}
}

func (h *harness) dump(db *sql.DB, table string) []string {
	h.t.Helper()
	order := "id"
	if table == "team_member" {
		order = "team_id, member_id"
	}
	rows, err := db.Query("SELECT * FROM app." + table + " ORDER BY " + order)
	if err != nil {
		h.t.Fatal(err)
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
			h.t.Fatal(err)
		}
		var b strings.Builder
		for _, v := range vals {
			fmt.Fprintf(&b, "%v|%q;", v.Valid, v.String)
		}
		out = append(out, b.String())
	}
	return out
}

var errCrash = errors.New("crash for the test")

func clearHooks() {
	testHooks.afterApply, testHooks.beforeCommit, testHooks.afterCommit = nil, nil, nil
}

// onCall returns a function that is true on its nth call only, and whether that call happened.
func onCall(n int) (func() bool, *bool) {
	calls, fired := 0, false
	return func() bool {
		calls++
		if calls == n {
			fired = true
		}
		return calls == n
	}, &fired
}

// crashAt runs with hooks set by set, writes changes, and waits for the run to end (or ends it
// when the hook makes it retry and keep going). The hook must have fired.
// It returns the run's error.
func (h *harness) crashAt(set func(cancel context.CancelFunc) *bool) error {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer clearHooks()
	fired := set(cancel)
	ch := h.start(ctx)
	h.load(60)
	var err error
	select {
	case err = <-ch:
	case <-time.After(5 * time.Second): // the uncertain cases retry and keep running
		cancel()
		err = <-ch
	}
	if !*fired {
		h.t.Fatal("the hook never fired")
	}
	return err
}

func TestRunRecovers(t *testing.T) {
	h := newHarness(t)

	t.Run("first position stored but the reply lost", func(t *testing.T) {
		parent := h.t
		defer func() { h.t = parent }()
		from := *h.from
		ctx, cancel := context.WithCancel(context.Background())
		ch := h.start(ctx)
		for i := 0; i < 50 && h.savedOr().Pos == 0; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		cancel()
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
		if got := h.saved(); got.position().Compare(from) != 0 || got.EventCRC != 0 {
			t.Fatalf("expected only the --from position to be stored: %+v", got)
		}
		h.from = &from // run again with --from, as a retry after a lost reply would
		h.t = t
		h.finish()
	})

	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			parent := h.t
			h.t = t
			defer func() { h.t = parent }()
			f(t)
		})
	}

	run("crash before commit", func(t *testing.T) {
		err := h.crashAt(func(cancel context.CancelFunc) *bool {
			hit, fired := onCall(3)
			testHooks.beforeCommit = func(tx *sql.Tx) error {
				if hit() {
					cancel()
					tx.Rollback()
					return errCrash
				}
				return nil
			}
			return fired
		})
		if !errors.Is(err, errCrash) {
			t.Fatalf("the first run should end with the crash: %v", err)
		}
		h.finish()
	})

	run("crash after commit", func(t *testing.T) {
		err := h.crashAt(func(cancel context.CancelFunc) *bool {
			hit, fired := onCall(3)
			testHooks.afterCommit = func() error {
				if hit() {
					cancel()
					return errCrash
				}
				return nil
			}
			return fired
		})
		if !errors.Is(err, errCrash) {
			t.Fatalf("the first run should end with the crash: %v", err)
		}
		h.finish()
	})

	run("commit outcome unknown, committed", func(t *testing.T) {
		err := h.crashAt(func(context.CancelFunc) *bool {
			hit, fired := onCall(3)
			testHooks.afterCommit = func() error {
				if hit() {
					return errors.New("reply lost")
				}
				return nil
			}
			return fired
		})
		if err != nil {
			t.Fatalf("the run should have retried and stopped cleanly: %v", err)
		}
		h.finish()
	})

	run("commit outcome unknown, not committed", func(t *testing.T) {
		err := h.crashAt(func(context.CancelFunc) *bool {
			hit, fired := onCall(3)
			testHooks.beforeCommit = func(tx *sql.Tx) error {
				if hit() {
					tx.Rollback()
					return &UncertainError{errors.New("reply lost")}
				}
				return nil
			}
			return fired
		})
		if err != nil {
			t.Fatalf("the run should have retried and stopped cleanly: %v", err)
		}
		h.finish()
	})

	run("kill -9 after only unfollowed transactions", func(t *testing.T) {
		before := h.saved().position()
		cmd := h.startBinary()
		time.Sleep(time.Second)
		for i := 0; i < 20; i++ {
			h.exec(h.srcDB, "INSERT INTO app_other.note (v) VALUES (?)", i)
		}
		time.Sleep(12 * time.Second) // past the default save interval of the binary
		h.exec(h.srcDB, "INSERT INTO app_other.note (v) VALUES (0)")
		time.Sleep(500 * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()
		if got := h.saved(); got.position().Compare(before) <= 0 || got.EventCRC == 0 {
			t.Fatalf("no position was stored for the unfollowed transactions: %+v", got)
		}
		h.finish()
	})

	run("crash while applying, then rotate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer clearHooks()
		hit, fired := onCall(3)
		testHooks.afterApply = func() error {
			if hit() {
				h.exec(h.srcDB, "FLUSH BINARY LOGS") // the source moves on to a new file while the copy transaction is open
				cancel()
				return errCrash
			}
			return nil
		}
		ch := h.start(ctx)
		tx, err := h.srcDB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 8; i++ {
			if _, err := tx.Exec("INSERT INTO app.member (created, name, handle, locale) VALUES (NOW(6), 'r', ?, 'EN')", fmt.Sprintf("rot%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-ch; !errors.Is(err, errCrash) {
			t.Fatalf("the first run should end with the crash: %v", err)
		}
		if !*fired {
			t.Fatal("the hook never fired")
		}
		clearHooks()
		h.finish()
	})

	run("source and copy connections killed while applying", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer clearHooks()
		calls, srcKilled, dstKilled := 0, false, false
		var killErr error
		testHooks.afterApply = func() error {
			calls++
			switch calls {
			case 3:
				killErr = errors.Join(killErr, killAll(h.srcDB, "Command = 'Binlog Dump'"))
				srcKilled = true
			case 40:
				killErr = errors.Join(killErr, killAll(h.dstDB, "USER = 'root' AND ID <> CONNECTION_ID()"))
				dstKilled = true
			}
			return nil
		}
		ch := h.start(ctx)
		for i := 0; i < 6 && !(srcKilled && dstKilled); i++ {
			h.load(40)
			time.Sleep(2 * time.Second)
		}
		cancel()
		if err := <-ch; err != nil {
			t.Fatalf("run should have reconnected: %v", err)
		}
		if !srcKilled || !dstKilled || killErr != nil {
			t.Fatalf("connections killed: source %v, copy %v, errors %v", srcKilled, dstKilled, killErr)
		}
		clearHooks()
		h.finish()
	})

	run("stop request while applying a large transaction", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer clearHooks()
		hit, fired := onCall(100)
		testHooks.afterApply = func() error {
			if hit() {
				cancel()
			}
			return nil
		}
		ch := h.start(ctx)
		h.exec(h.srcDB, `INSERT INTO app.post (member_id, body, created, changed)
			SELECT member_id, 'bulk', NOW(6), NOW(6) FROM app.post LIMIT 3000`)
		want := h.boundary()
		if err := <-ch; err != nil {
			t.Fatalf("graceful stop: %v", err)
		}
		if !*fired {
			t.Fatal("the stop was not requested while applying")
		}
		if got := h.saved().position(); got.Compare(want) != 0 {
			t.Fatalf("the large transaction should be finished and stored: %s, want %s", got, want)
		}
		clearHooks()
		h.finish()
	})
}

// killAll kills the connections that match; it runs inside hooks, so it returns errors instead of failing the test.
func killAll(db *sql.DB, where string) error {
	rows, err := db.Query("SELECT ID FROM information_schema.PROCESSLIST WHERE " + where)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return fmt.Errorf("no connection matched %s", where)
	}
	for _, id := range ids {
		if _, err := db.Exec(fmt.Sprintf("KILL %d", id)); err != nil {
			return fmt.Errorf("KILL %d: %w", id, err)
		}
	}
	return nil
}

// stopCase starts from a fresh copy, catches up, injects a problem and expects a stop that keeps the stored position.
func stopCase(t *testing.T, h *harness, prepare, inject func()) {
	t.Helper()
	h.reset()
	h.load(20)
	if prepare != nil {
		prepare()
	}
	h.finish()
	before := h.saved()
	inject()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := <-h.start(ctx)
	var s *StopError
	if !errors.As(err, &s) {
		t.Fatalf("expected a stop, got %v", err)
	}
	t.Logf("stopped: %s", err)
	if got := h.saved(); got != before {
		t.Fatalf("stored position changed: %+v, before the problem %+v", got, before)
	}
}

func TestRunStopsOnLiveSchemaChange(t *testing.T) {
	h := newHarness(t)
	h.load(20)
	h.finish()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ch := h.start(ctx)
	time.Sleep(time.Second)
	h.load(10)
	h.exec(h.srcDB, "ALTER TABLE app.label ADD COLUMN extra int NULL")
	err := <-ch
	var s *StopError
	if !errors.As(err, &s) || !strings.Contains(err.Error(), "statement-format event of a kind") {
		t.Fatalf("expected the stream to stop at the ALTER, got %v", err)
	}
	t.Logf("stopped: %s", err)
}

func TestRunStops(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name            string
		prepare, inject func()
	}{
		{"copy row changed", nil, func() {
			h.exec(h.dstDB, "UPDATE app.member SET name = 'changed' ORDER BY id DESC LIMIT 1")
			h.exec(h.srcDB, "UPDATE app.member SET name = 'src' ORDER BY id DESC LIMIT 1")
		}},
		{"copy differs only in case", func() {
			h.exec(h.srcDB, "UPDATE app.member SET name = 'abc' ORDER BY id DESC LIMIT 1")
		}, func() {
			h.exec(h.dstDB, "UPDATE app.member SET name = 'ABC' ORDER BY id DESC LIMIT 1")
			h.exec(h.srcDB, "UPDATE app.member SET name = 'next' ORDER BY id DESC LIMIT 1")
		}},
		{"copy has the row being inserted", nil, func() {
			var id int64
			h.srcDB.QueryRow("SELECT MAX(id) + 1 FROM app.like_mark").Scan(&id)
			h.exec(h.dstDB, "INSERT INTO app.like_mark (id, member_id, post_id, created) VALUES (?, 1, 1, NOW())", id)
			h.exec(h.srcDB, "INSERT INTO app.like_mark (id, member_id, post_id, created) VALUES (?, 2, 2, NOW())", id)
		}},
		{"copy lacks the row being deleted", nil, func() {
			h.exec(h.dstDB, "DELETE FROM app.like_mark ORDER BY id DESC LIMIT 1")
			h.exec(h.srcDB, "DELETE FROM app.like_mark ORDER BY id DESC LIMIT 1")
		}},
		{"schema change", nil, func() {
			h.exec(h.srcDB, "ALTER TABLE app.label ADD COLUMN extra int NULL")
		}},
		{"statement-format insert", nil, func() {
			conn, err := h.srcDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for _, q := range []string{"SET SESSION binlog_format = 'STATEMENT'",
				"INSERT INTO app.like_mark (member_id, post_id, created) VALUES (7, 7, '2026-01-01')"} {
				if _, err := conn.ExecContext(context.Background(), q); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"statement naming a followed table from another database", nil, func() {
			conn, err := h.srcDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for _, q := range []string{"SET SESSION binlog_format = 'STATEMENT'", "USE app_other",
				"INSERT INTO app.like_mark (member_id, post_id, created) VALUES (8, 8, '2026-01-01')"} {
				if _, err := conn.ExecContext(context.Background(), q); err != nil {
					t.Fatal(err)
				}
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.t = t
			stopCase(t, h, c.prepare, c.inject)
		})
	}
}
