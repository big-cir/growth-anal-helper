//go:build integration

package cdc

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func (h *harness) checkOpts(writesStopped bool) CheckOptions {
	return CheckOptions{Source: h.src, Target: h.dst, Database: "app", Tables: followed, WritesStopped: writesStopped,
		ChunkRows: 100, Wait: 3 * time.Second, Rounds: 3, RoundGap: 300 * time.Millisecond, Logf: h.t.Logf}
}

func (h *harness) check(writesStopped bool) *CheckReport {
	h.t.Helper()
	rep, err := Check(context.Background(), h.checkOpts(writesStopped))
	if err != nil {
		h.t.Fatalf("check: %v", err)
	}
	return rep
}

// sum adds up the tables of a report.
func sum(r *CheckReport) TableCheck {
	var s TableCheck
	for _, t := range r.Tables {
		s.Chunks, s.Rows, s.Missing, s.Extra, s.Differ, s.Undecided = s.Chunks+t.Chunks, s.Rows+t.Rows,
			s.Missing+t.Missing, s.Extra+t.Extra, s.Differ+t.Differ, s.Undecided+t.Undecided
		s.Unchecked = s.Unchecked || t.Unchecked
	}
	return s
}

func (h *harness) want(r *CheckReport, missing, extra, differ int64) {
	h.t.Helper()
	s := sum(r)
	if s.Missing != missing || s.Extra != extra || s.Differ != differ || s.Undecided != 0 || s.Unchecked {
		h.t.Fatalf("got missing %d extra %d differ %d undecided %d unchecked %v; want %d %d %d",
			s.Missing, s.Extra, s.Differ, s.Undecided, s.Unchecked, missing, extra, differ)
	}
}

// copyExec runs statements on one copy connection, so a session setting applies to them.
func (h *harness) copyExec(qs ...string) {
	h.t.Helper()
	conn, err := h.dstDB.Conn(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range qs {
		if _, err := conn.ExecContext(context.Background(), q); err != nil {
			h.t.Fatalf("%s: %v", q, err)
		}
	}
}

// checkOne compares one table with writes stopped, outside the followed list.
func (h *harness) checkOne(table string, chunkRows int) *TableCheck {
	h.t.Helper()
	c, ct, done := h.checkerFor(table, chunkRows)
	defer done()
	tc := &TableCheck{}
	if err := c.table(context.Background(), ct, tc); err != nil {
		h.t.Fatal(err)
	}
	return tc
}

// checkerFor sets up a writes-stopped checker for one table; done closes its connections.
func (h *harness) checkerFor(table string, chunkRows int) (*checker, *checkTable, func()) {
	h.t.Helper()
	ctx := context.Background()
	names := []string{"app." + table}
	srcDefs, err := readDefs(ctx, h.srcDB, "", names)
	if err != nil {
		h.t.Fatal(err)
	}
	ct, err := checkSQL(srcDefs[names[0]], "`app`.`"+table+"`", "`app`.`"+table+"`")
	if err != nil {
		h.t.Fatal(err)
	}
	var uuid string
	h.srcDB.QueryRow("SELECT @@server_uuid").Scan(&uuid)
	c := &checker{o: CheckOptions{WritesStopped: true, ChunkRows: chunkRows}, uuid: uuid, pt: h.saved()}
	if c.src, err = h.srcDB.Conn(ctx); err != nil {
		h.t.Fatal(err)
	}
	if c.dst, err = h.dstDB.Conn(ctx); err != nil {
		h.t.Fatal(err)
	}
	return c, ct, func() { c.src.Close(); c.dst.Close() }
}

func (h *harness) checkBinary(extra ...string) (int, string) {
	h.t.Helper()
	args := append([]string{"check", "--defaults-file", os.Getenv("CDC_SRC_CNF"), "--target-defaults-file", os.Getenv("CDC_DST_CNF"),
		"--database", "app", "--tables", strings.Join(followed, ","), "--chunk-rows", "100"}, extra...)
	var out bytes.Buffer
	cmd := exec.Command(os.Getenv("CDC_BIN"), args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String()
	} else if err != nil {
		h.t.Fatal(err)
	}
	return 0, out.String()
}

func TestCheck(t *testing.T) {
	h := newHarness(t)
	sub := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			parent := h.t
			h.t = t
			defer func() { h.t = parent }()
			h.reset()
			f(t)
		})
	}

	sub("same rows with writes stopped", func(t *testing.T) {
		h.load(50)
		h.finish()
		r := h.check(true)
		h.want(r, 0, 0, 0)
		var rows int64
		for _, tb := range followed {
			var n int64
			h.srcDB.QueryRow("SELECT COUNT(*) FROM app." + tb).Scan(&n)
			rows += n
		}
		if s := sum(r); s.Rows != rows || s.Chunks < 10 {
			t.Fatalf("%d rows in %d chunks, source has %d", s.Rows, s.Chunks, rows)
		}
	})

	sub("counts each kind of mismatch", func(t *testing.T) {
		h.finish()
		h.exec(h.dstDB, "DELETE FROM app.member WHERE id = 5")
		h.exec(h.dstDB, "INSERT INTO app.like_mark (id, member_id, post_id, created) VALUES (999999999, 1, 999999999, NOW())") // past the last key
		h.exec(h.dstDB, "INSERT INTO app.label (id, title, color) VALUES (-5, 'below', 1)")                                    // before the first key
		h.exec(h.dstDB, "UPDATE app.post SET body = 'other' WHERE id = 7")
		h.exec(h.dstDB, "UPDATE app.team_member SET rank_no = rank_no + 1 ORDER BY team_id, member_id LIMIT 1") // composite key
		h.want(h.check(true), 1, 2, 2)
	})

	sub("tells close values apart", func(t *testing.T) {
		h.exec(h.srcDB, "UPDATE app.member SET name = NULL WHERE id = 11")
		h.exec(h.srcDB, "UPDATE app.member SET name = 'a\\0', handle = 'b' WHERE id = 12")
		h.finish()
		h.exec(h.dstDB, "UPDATE app.member SET name = '' WHERE id = 11")                                // NULL and empty
		h.exec(h.dstDB, "UPDATE app.member SET name = 'a', handle = '\\0b' WHERE id = 12")              // a zero byte moved to the next column
		h.exec(h.dstDB, "UPDATE app.post SET created = created + INTERVAL 1 MICROSECOND WHERE id = 13") // fraction of a second
		h.copyExec("SET SESSION sql_mode = ''", "UPDATE app.member SET locale = 'XX' WHERE id = 14")    // enum error value
		h.want(h.check(true), 0, 0, 4)
	})

	sub("empty table with a row only in the copy", func(t *testing.T) {
		h.exec(h.srcDB, "DELETE FROM app.label")
		h.finish()
		h.exec(h.dstDB, "INSERT INTO app.label (id, title, color) VALUES (5, 'only here', 1)")
		r := h.check(true)
		h.want(r, 0, 1, 0)
		if r.Tables["app.label"].Extra != 1 {
			t.Fatal("the extra row should be in label")
		}
	})

	sub("descending key, enum empty item and a removed boundary row", func(t *testing.T) {
		h.finish()
		for _, db := range []string{"src", "dst"} {
			conn := h.srcDB
			if db == "dst" {
				conn = h.dstDB
			}
			h.exec(conn, "CREATE TABLE app.extra (a varchar(10) NOT NULL, b int NOT NULL, e enum('', 'x') NOT NULL, PRIMARY KEY (a DESC, b)) ENGINE=InnoDB")
			h.exec(conn, "INSERT INTO app.extra SELECT CONCAT('k', id % 7), id, '' FROM app.member WHERE id <= 300")
		}
		if tc := h.checkOne("extra", 50); tc.differs() || tc.Rows != 300 || tc.Chunks != 6 {
			t.Fatalf("before changes: %+v", tc)
		}
		h.copyExec("SET SESSION sql_mode = ''", "UPDATE app.extra SET e = 'zz' WHERE a = 'k1' AND b = 1") // error value 0, not the '' item
		var a string
		var b int
		h.srcDB.QueryRow("SELECT a, b FROM app.extra ORDER BY a, b LIMIT 1 OFFSET 50").Scan(&a, &b)
		h.exec(h.dstDB, "DELETE FROM app.extra WHERE a = ? AND b = ?", a, b)
		if tc := h.checkOne("extra", 50); tc.Missing != 1 || tc.Differ != 1 || tc.Extra != 0 {
			t.Fatalf("after changes: %+v", tc)
		}
	})

	sub("reaches the source across rotations, a few events per page", func(t *testing.T) {
		eventPage = 2
		defer func() { eventPage = 1000 }()
		h.finish()
		for i := 0; i < 3; i++ {
			h.exec(h.srcDB, "FLUSH BINARY LOGS")
		}
		r := h.check(true)
		h.want(r, 0, 0, 0)
		if strings.HasPrefix(r.Position, h.saved().File+":") {
			t.Fatal("the source position should be in a later file than the stored one")
		}
	})

	sub("time limit leaves tables unchecked", func(t *testing.T) {
		h.finish()
		o := h.checkOpts(true)
		o.MaxTime = time.Millisecond
		r, err := Check(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		for n, tc := range r.Tables {
			if !tc.Unchecked {
				t.Fatalf("%s should be unchecked", n)
			}
		}
		if r.ExitCode() != 4 {
			t.Fatalf("exit %d", r.ExitCode())
		}
	})

	sub("time limit while waiting for the copy", func(t *testing.T) {
		h.finish()
		h.load(5) // run is not running, so the copy never reaches the source
		o := h.checkOpts(true)
		o.MaxTime = 500 * time.Millisecond
		r, err := Check(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if r.Position != "" {
			t.Fatalf("the copy never reached the source, yet the position is %s", r.Position)
		}
		for n, tc := range r.Tables {
			if !tc.Unchecked {
				t.Fatalf("%s should be unchecked", n)
			}
		}
		if r.ExitCode() != 4 {
			t.Fatalf("exit %d", r.ExitCode())
		}
	})

	sub("many rows only in the copy", func(t *testing.T) {
		h.finish()
		add := func(from, n int) {
			h.exec(h.dstDB, "INSERT INTO app.like_mark (id, member_id, post_id, created) SELECT ? + id, ? + id, 1, NOW() FROM app.member WHERE id <= ?", 800000000+from, 800000000+from, n)
		}
		add(0, 1000)
		add(1000, 100)
		h.want(h.check(true), 0, 1100, 0)
		r := h.check(false) // too many to keep as suspects
		if s := sum(r); s.Undecided != 1 || s.Extra != 0 {
			t.Fatalf("%+v", s)
		}
		// the row read itself stops past the limit, even when the chunk hash counted fewer
		c, ct, done := h.checkerFor("like_mark", 10)
		defer done()
		if _, ok, err := c.diff(context.Background(), ct, span{}); err != nil || ok {
			t.Fatalf("a read of %d rows with a limit of %d: ok %v err %v", 2100, c.rowLimit(), ok, err)
		}
		add(2000, 400) // past what a chunk is read row by row for
		r = h.check(true)
		if s := sum(r); s.Undecided != 1 || s.Extra != 0 || r.ExitCode() != 4 {
			t.Fatalf("%+v exit %d", s, r.ExitCode())
		}
	})

	sub("writes during a writes-stopped check", func(t *testing.T) {
		h.finish()
		ctx, cancel := context.WithCancel(context.Background())
		ch := h.start(ctx)
		stop := h.background()
		_, err := Check(context.Background(), h.checkOpts(true))
		if werr := stop(); werr != nil {
			t.Fatal(werr)
		}
		cancel()
		<-ch
		if err == nil {
			t.Fatal("check should stop when the source is written")
		}
		t.Logf("stopped: %s", err)
	})

	sub("suspects while the source writes", func(t *testing.T) {
		h.finish()
		h.exec(h.dstDB, "INSERT INTO app.like_mark (id, member_id, post_id, created) VALUES (999999999, 1, 999999999, NOW())")
		ctx, cancel := context.WithCancel(context.Background())
		ch := h.start(ctx)
		stop := h.background()
		r := h.check(false)
		if werr := stop(); werr != nil {
			t.Fatal(werr)
		}
		cancel()
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
		h.want(r, 0, 1, 0)
	})

	sub("undecided while the copy lags", func(t *testing.T) {
		h.finish()
		stop := h.background()
		o := h.checkOpts(false)
		o.Wait = 300 * time.Millisecond
		r, err := Check(context.Background(), o)
		if werr := stop(); werr != nil {
			t.Fatal(werr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if s := sum(r); s.Undecided == 0 || s.Missing+s.Extra+s.Differ != 0 {
			t.Fatalf("%+v", s)
		}
	})

	sub("stops on a copy that is not ready", func(t *testing.T) {
		h.finish()
		o := h.checkOpts(true)
		o.Tables = followed[:2]
		if _, err := Check(context.Background(), o); err == nil {
			t.Fatal("another table list should stop")
		}
		h.exec(h.dstDB, "UPDATE "+positionTable+" SET phase = 'copying'")
		if _, err := Check(context.Background(), h.checkOpts(true)); err == nil {
			t.Fatal("a copy in the middle of init should stop")
		}
	})

	sub("command shows no values", func(t *testing.T) {
		const mark = "SECRETMARK"
		h.exec(h.srcDB, "UPDATE app.member SET handle = '"+mark+"1' WHERE id = 3")
		h.finish()
		h.exec(h.dstDB, "UPDATE app.member SET name = '"+mark+"' WHERE id = 3")
		code, out := h.checkBinary("--writes-stopped")
		if code != 3 || strings.Contains(out, mark) || strings.Contains(out, "SELECT") {
			t.Fatalf("exit %d, output:\n%s", code, out)
		}
		// a MySQL error in the middle: a table lock makes the copy read time out
		lock, err := h.dstDB.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// the pool keeps the session, so the lock must be released, not only the connection returned
		defer func() {
			lock.ExecContext(context.Background(), "UNLOCK TABLES")
			lock.Close()
		}()
		if _, err := lock.ExecContext(context.Background(), "LOCK TABLES app.member WRITE"); err != nil {
			t.Fatal(err)
		}
		var waitWas string
		h.dstDB.QueryRow("SELECT @@GLOBAL.lock_wait_timeout").Scan(&waitWas)
		h.exec(h.dstDB, "SET GLOBAL lock_wait_timeout = 1")
		defer h.exec(h.dstDB, "SET GLOBAL lock_wait_timeout = "+waitWas)
		code, out = h.checkBinary("--writes-stopped")
		if code != 1 || !strings.Contains(out, "check failed") || strings.Contains(out, mark) || strings.Contains(out, "SELECT") {
			t.Fatalf("exit %d, output:\n%s", code, out)
		}
		t.Logf("error output: %s", out)
	})
}
