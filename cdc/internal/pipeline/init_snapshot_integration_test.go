//go:build integration

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"growth-lab-cdc/internal/cdc"
	"growth-lab-cdc/internal/testenv"
)

// holdCommits makes the source keep a transaction written to the binlog but not committed in
// the engine for wait: semi-sync waits for a replica that never acknowledges.
func (h *harness) holdCommits(wait time.Duration) (undo func()) {
	h.T.Helper()
	var n int
	h.SrcDB.QueryRow("SELECT COUNT(*) FROM information_schema.PLUGINS WHERE PLUGIN_NAME = 'rpl_semi_sync_source'").Scan(&n)
	if n == 0 {
		h.Exec(h.SrcDB, "INSTALL PLUGIN rpl_semi_sync_source SONAME 'semisync_source.so'")
	}
	for _, q := range []string{
		"SET GLOBAL rpl_semi_sync_source_wait_point = AFTER_SYNC",
		"SET GLOBAL rpl_semi_sync_source_wait_no_replica = ON",
		"SET GLOBAL rpl_semi_sync_source_timeout = " + strconv.FormatInt(wait.Milliseconds(), 10),
		"SET GLOBAL rpl_semi_sync_source_enabled = ON",
	} {
		h.Exec(h.SrcDB, q)
	}
	return func() { h.Exec(h.SrcDB, "SET GLOBAL rpl_semi_sync_source_enabled = OFF") }
}

// srcPos is the source's current binlog position as text.
func (h *harness) srcPos() string {
	h.T.Helper()
	var file, pos string
	var x1, x2, x3 any
	if err := h.SrcDB.QueryRow("SHOW BINARY LOG STATUS").Scan(&file, &pos, &x1, &x2, &x3); err != nil {
		h.T.Fatal(err)
	}
	return file + ":" + pos
}

// A transaction in the binlog but not yet committed when init takes its snapshot must still reach the copy:
// the source lock waits for its commit before the snapshot starts.
func TestInitKeepsATransactionCommittingAtTheSnapshot(t *testing.T) {
	h := newHarness(t)
	h.freshForInit(false)
	undo := h.holdCommits(3 * time.Second) // within one lock attempt
	defer undo()

	before := h.srcPos()
	done := make(chan error, 1)
	go func() {
		_, err := h.SrcDB.Exec("INSERT INTO app.member (created, name, handle, locale) VALUES (NOW(6), 'held', 'held-at-snapshot', 'KO')")
		done <- err
	}()
	// wait until the insert is in the binlog; its commit is still held
	deadline := time.Now().Add(2 * time.Second)
	for h.srcPos() == before {
		if time.Now().After(deadline) {
			t.Fatal("the insert never reached the binlog")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.heldVisible() {
		t.Fatal("the insert is already visible; the hold did not work")
	}
	var visibleAtLock, fired bool
	testHooks.initStep = func(s string) error {
		if s == "source-locked" && !fired {
			fired, visibleAtLock = true, h.heldVisible()
		}
		return nil
	}
	defer func() { testHooks.initStep = nil }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o := h.initOpts()
	var waited time.Duration
	o.Logf = func(f string, a ...any) {
		line := fmt.Sprintf(f, a...)
		if w, ok := strings.CutPrefix(line, "locked the source tables after waiting "); ok && waited == 0 {
			waited, _ = time.ParseDuration(strings.Fields(w)[0][:len(strings.Fields(w)[0])-1])
		}
		t.Log(line)
	}
	if err := Init(ctx, o); err != nil {
		t.Fatalf("init: %v", err)
	}
	if waited < time.Second {
		t.Fatalf("the lock should have waited for the held commit, waited %s", waited)
	}
	if !fired || !visibleAtLock {
		t.Fatalf("the lock should be granted only after the held commit (fired %v, visible %v)", fired, visibleAtLock)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	undo()
	h.done(false)
}

func (h *harness) heldVisible() bool {
	var n int
	h.SrcDB.QueryRow("SELECT COUNT(*) FROM app.member WHERE handle = 'held-at-snapshot'").Scan(&n)
	return n == 1
}

func TestInitSourceLock(t *testing.T) {
	h := newHarness(t)
	sub := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			parent := h.T
			h.T = t
			defer func() { h.T = parent; testHooks.initStep = nil }()
			h.freshForInit(false)
			f(t)
		})
	}

	sub("stops after the tables stay busy", func(t *testing.T) {
		tx, err := h.SrcDB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec("UPDATE app.label SET color = color WHERE id = 1"); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		err = Init(context.Background(), h.initOpts())
		if !strings.Contains(fmt.Sprint(err), "could not lock the followed tables") {
			t.Fatalf("init should stop on a busy table: %v", err)
		}
		if took := time.Since(started); took < time.Duration(snapshotLockTries*snapshotLockWait)*time.Second {
			t.Fatalf("gave up after %s", took)
		}
		tx.Rollback()
		h.resumeInit()
		h.done(false)
	})

	sub("each try stops at its limit with two busy tables", func(t *testing.T) {
		for _, q := range []string{"UPDATE app.label SET color = color WHERE id = 1", "UPDATE app.post SET body = body WHERE id = 1"} {
			tx, err := h.SrcDB.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		started := time.Now()
		err := Init(context.Background(), h.initOpts())
		took := time.Since(started)
		if !strings.Contains(fmt.Sprint(err), "could not lock the followed tables") {
			t.Fatalf("init should stop on busy tables: %v", err)
		}
		// three tries of five seconds and two pauses, not five seconds per busy table
		if limit := time.Duration(snapshotLockTries*snapshotLockWait+(snapshotLockTries-1)*snapshotLockWait+8) * time.Second; took > limit {
			t.Fatalf("gave up after %s, past %s", took, limit)
		}
	})

	sub("an unlock that fails leaves the tables writable", func(t *testing.T) {
		killed := false
		testHooks.initStep = func(s string) error {
			if s == "source-position-read" && !killed {
				killed = true
				if err := h.killLockHolder(); err != nil {
					return err
				}
				// the unlock will fail on the dead connection; the tables are free already
				return h.writeWithin(2)
			}
			return nil
		}
		if err := Init(context.Background(), h.initOpts()); err != nil {
			t.Fatalf("init should retry after the unlock fails: %v", err)
		}
		if !killed {
			t.Fatal("the hook never fired")
		}
		h.done(false)
	})

	sub("a failure while locked leaves the tables writable", func(t *testing.T) {
		testHooks.initStep = func(s string) error {
			if s == "source-locked" {
				return errCrash
			}
			return nil
		}
		if err := Init(context.Background(), h.initOpts()); !errors.Is(err, errCrash) {
			t.Fatalf("init should end with the crash: %v", err)
		}
		testHooks.initStep = nil
		conn, err := h.SrcDB.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), "SET SESSION lock_wait_timeout = 2"); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(context.Background(), "UPDATE app.label SET color = color WHERE id = 1"); err != nil {
			t.Fatalf("the source table should be writable after the failure: %v", err)
		}
		h.resumeInit()
		h.done(false)
	})

	sub("the lock connection dies before the position is read", func(t *testing.T) {
		killed := false
		testHooks.initStep = func(s string) error {
			if s == "source-locked" && !killed {
				killed = true
				return h.killLockHolder()
			}
			return nil
		}
		if err := Init(context.Background(), h.initOpts()); err != nil {
			t.Fatalf("init should retry after losing the lock connection: %v", err)
		}
		if !killed {
			t.Fatal("the hook never fired")
		}
		h.done(false)
	})

	sub("needs the LOCK TABLES privilege", func(t *testing.T) {
		for _, q := range []string{
			"DROP USER IF EXISTS 'cdc_nolock'@'%'",
			"CREATE USER 'cdc_nolock'@'%' IDENTIFIED BY 'nolock-pw'",
			"GRANT SELECT, REPLICATION CLIENT, REPLICATION SLAVE ON *.* TO 'cdc_nolock'@'%'",
		} {
			h.Exec(h.SrcDB, q)
		}
		defer h.Exec(h.SrcDB, "DROP USER IF EXISTS 'cdc_nolock'@'%'")
		o := h.initOpts()
		o.Source.User, o.Source.Password = "cdc_nolock", "nolock-pw"
		err := Init(context.Background(), o)
		if !strings.Contains(fmt.Sprint(err), "LOCK TABLES and SELECT privileges") {
			t.Fatalf("init should name the missing privilege: %v", err)
		}
		if strings.Contains(fmt.Sprint(cdc.SafeError(err)), "cdc_nolock") {
			t.Fatalf("the error shows the server message: %v", cdc.SafeError(err))
		}
	})

	sub("stops on a source trigger", func(t *testing.T) {
		h.Exec(h.SrcDB, "CREATE TRIGGER app.label_touch BEFORE UPDATE ON app.label FOR EACH ROW SET NEW.changed = NEW.changed")
		defer h.Exec(h.SrcDB, "DROP TRIGGER IF EXISTS app.label_touch")
		err := Init(context.Background(), h.initOpts())
		if !strings.Contains(fmt.Sprint(err), "the source has triggers") {
			t.Fatalf("init should stop on a source trigger: %v", err)
		}
	})

	// writes that never pause leave no moment without an open write, so the lock is never
	// granted; init gives up without holding the writes back while it waits
	sub("gives up under writers that never pause, without slowing them", func(t *testing.T) {
		var commits atomic.Int64
		var stops []func() error
		for i := 0; i < 4; i++ {
			stops = append(stops, h.writer(int64(i), &commits))
		}
		time.Sleep(time.Second)
		alone := commits.Load()
		started := time.Now()
		err := Init(context.Background(), h.initOpts())
		during := commits.Load() - alone
		took := time.Since(started)
		for _, stop := range stops {
			if werr := stop(); werr != nil {
				t.Fatal(werr)
			}
		}
		if !strings.Contains(fmt.Sprint(err), "could not lock the followed tables") {
			t.Fatalf("init should give up: %v", err)
		}
		// commits per second while init waited, against the second before it
		rate := float64(during) / took.Seconds()
		t.Logf("%d commits in the second before, %.0f per second while init waited", alone, rate)
		if rate < float64(alone)/2 {
			t.Fatalf("writes slowed down while init waited for the lock")
		}
	})
}

// writer keeps changing the followed tables from its own connection, transactions back to back,
// until stop is called.
func (h *harness) writer(seed int64, commits *atomic.Int64) (stop func() error) {
	done := make(chan struct{})
	result := make(chan error, 1)
	rng := rand.New(rand.NewSource(seed))
	go func() {
		for n := 0; ; n++ {
			select {
			case <-done:
				result <- nil
				return
			default:
			}
			tx, err := h.SrcDB.Begin()
			if err == nil {
				_, err = tx.Exec("UPDATE app.post SET body = ? WHERE id = ?", fmt.Sprintf("w%d-%d", seed, n), 1+rng.Intn(1000))
				if err == nil {
					_, err = tx.Exec("UPDATE app.team_member SET rank_no = rank_no + 1 WHERE member_id = ?", 1+rng.Intn(1000))
				}
				if err == nil {
					err = tx.Commit()
				} else {
					tx.Rollback()
				}
			}
			if testenv.IsDeadlock(err) {
				continue
			}
			if err != nil {
				result <- err
				return
			}
			commits.Add(1)
		}
	}()
	return func() error {
		close(done)
		return <-result
	}
}

// killLockHolder ends the session that holds init's READ lock on the source tables.
func (h *harness) killLockHolder() error {
	var id int64
	if err := h.SrcDB.QueryRow(`SELECT t.PROCESSLIST_ID FROM performance_schema.metadata_locks m
		JOIN performance_schema.threads t ON t.THREAD_ID = m.OWNER_THREAD_ID
		WHERE m.OBJECT_SCHEMA = 'app' AND m.LOCK_TYPE = 'SHARED_READ_ONLY' LIMIT 1`).Scan(&id); err != nil {
		return err
	}
	_, err := h.SrcDB.Exec(fmt.Sprintf("KILL %d", id))
	return err
}

// writeWithin changes a followed source table from its own connection, waiting at most secs for locks.
func (h *harness) writeWithin(secs int) error {
	conn, err := h.SrcDB.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("SET SESSION lock_wait_timeout = %d", secs)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), "UPDATE app.label SET color = color WHERE id = 1"); err != nil {
		return fmt.Errorf("the source table should be writable: %w", err)
	}
	return nil
}
