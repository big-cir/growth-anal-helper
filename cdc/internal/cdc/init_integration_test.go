//go:build integration

package cdc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
)

func (h *harness) initOpts() InitOptions {
	return InitOptions{Source: h.src, Target: h.dst, Database: "app", Tables: followed,
		SaveEvery: 200 * time.Millisecond, StopWait: 5 * time.Second, Logf: h.t.Logf}
}

// freshForInit rebuilds both sides; the copy keeps last generation's tables, made stale, or none.
func (h *harness) freshForInit(firstInstall bool) {
	h.t.Helper()
	h.reset()
	h.from = nil
	for _, q := range []string{"DROP DATABASE IF EXISTS " + stagingDB, "DROP DATABASE IF EXISTS " + oldDB} {
		h.exec(h.dstDB, q)
	}
	if firstInstall {
		h.exec(h.dstDB, "DROP DATABASE app")
		return
	}
	// the copy misses updates and deletes, as the current sync does
	h.exec(h.dstDB, "UPDATE app.member SET name = 'stale' ORDER BY id LIMIT 50")
	h.exec(h.dstDB, "DELETE FROM app.post ORDER BY id DESC LIMIT 30")
	h.exec(h.dstDB, "INSERT INTO app.like_mark (member_id, post_id, created) VALUES (777777777, 1, NOW())")
}

// initUnderLoad runs init while the source keeps changing, and returns init's error.
func (h *harness) initUnderLoad(ctx context.Context) error {
	h.t.Helper()
	stop := h.background()
	err := Init(ctx, h.initOpts())
	if werr := stop(); werr != nil {
		h.t.Fatalf("writer: %v", werr)
	}
	return err
}

func (h *harness) phase() string {
	var p string
	if err := h.dstDB.QueryRow("SELECT phase FROM " + positionTable).Scan(&p); err != nil {
		return ""
	}
	return p
}

func (h *harness) tablesIn(db string) int {
	h.t.Helper()
	var n int
	if err := h.dstDB.QueryRow("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", db).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// done checks that init finished, then that run takes over and the copy matches the source.
func (h *harness) done(firstInstall bool) {
	h.t.Helper()
	if p := h.phase(); p != phaseReady {
		h.t.Fatalf("phase %q after init", p)
	}
	old := len(followed)
	if firstInstall {
		old = 0
	}
	if got := h.tablesIn(stagingDB); got != 0 {
		h.t.Fatalf("%d tables left in staging", got)
	}
	if got := h.tablesIn(oldDB); got != old {
		h.t.Fatalf("%d tables in %s, want %d", got, oldDB, old)
	}
	var owners int
	h.dstDB.QueryRow("SELECT COUNT(*) FROM " + ownerTable).Scan(&owners)
	if owners != 0 {
		h.t.Fatalf("%d staging owner rows left", owners)
	}
	h.finish()
	// the live tables carry the source's secondary indexes again
	o := h.opts()
	defs, err := readDefs(context.Background(), h.dstDB, "", o.names())
	if err != nil {
		h.t.Fatal(err)
	}
	src, err := readDefs(context.Background(), h.srcDB, "", o.names())
	if err != nil {
		h.t.Fatal(err)
	}
	if err := sameDefs(src, defs, true); err != nil {
		h.t.Fatal(err)
	}
}

// crashInit runs init under load with a step hook; the hook returns errCrash to stop it there.
func (h *harness) crashInit(step string, wantPhase string) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := false
	testHooks.initStep = func(s string) error {
		if s == step && !fired {
			fired = true
			cancel()
			return errCrash
		}
		return nil
	}
	err := h.initUnderLoad(ctx)
	testHooks.initStep = nil
	if !fired {
		h.t.Fatalf("step %s never reached (init: %v)", step, err)
	}
	if !errors.Is(err, errCrash) {
		h.t.Fatalf("init should end with the crash at %s: %v", step, err)
	}
	if p := h.phase(); p != wantPhase {
		h.t.Fatalf("phase %q after a crash at %s, want %s", p, step, wantPhase)
	}
}

func (h *harness) resumeInit() {
	h.t.Helper()
	if err := h.initUnderLoad(context.Background()); err != nil {
		h.t.Fatalf("resumed init: %v", err)
	}
}

func TestInit(t *testing.T) {
	h := newHarness(t)
	sub := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			parent := h.t
			h.t = t
			defer func() { h.t = parent; clearHooks(); testHooks.initStep = nil }()
			f(t)
		})
	}

	sub("replaces stale tables under load", func(t *testing.T) {
		h.freshForInit(false)
		testHooks.initStep = func(s string) error {
			if s == "before-snapshot" {
				h.exec(h.srcDB, "INSERT INTO app.member (created, name, handle, locale) VALUES (NOW(6), 'between', 'between-x', 'KO')")
				h.exec(h.srcDB, "UPDATE app.team_member SET team_id = team_id + 5000 ORDER BY member_id LIMIT 2") // key change
				h.exec(h.srcDB, "DELETE FROM app.like_mark ORDER BY id LIMIT 2")
			}
			return nil
		}
		if err := h.initUnderLoad(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.done(false)
	})

	sub("first install", func(t *testing.T) {
		h.freshForInit(true)
		if err := h.initUnderLoad(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.done(true)
	})

	sub("crash while copying, run refused, then copy again", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("copied-first-table", phaseCopying)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := Run(ctx, h.opts())
		var s *StopError
		if !errors.As(err, &s) || !strings.Contains(err.Error(), "middle of an init") {
			t.Fatalf("run during an unfinished init should be refused: %v", err)
		}
		h.resumeInit()
		h.done(false)
	})

	sub("crash while converging", func(t *testing.T) {
		h.freshForInit(false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// changes between P and the snapshot: in the snapshot already, and replayed by overwriting
		testHooks.initStep = func(s string) error {
			if s == "before-snapshot" {
				for i := 0; i < 5; i++ {
					h.exec(h.srcDB, "INSERT INTO app.member (created, name, handle, locale) VALUES (NOW(6), 'between', ?, 'KO')", fmt.Sprintf("between-%d", i))
					h.exec(h.srcDB, "UPDATE app.post SET body = 'between' ORDER BY id LIMIT 3")
				}
			}
			return nil
		}
		fired := false
		testHooks.afterApply = func() error {
			if !fired && h.phase() == phaseConverging {
				fired = true
				cancel()
				return errCrash
			}
			return nil
		}
		err := h.initUnderLoad(ctx)
		testHooks.afterApply = nil
		if !fired || !errors.Is(err, errCrash) {
			t.Fatalf("no crash while converging (fired %v): %v", fired, err)
		}
		h.resumeInit()
		h.done(false)
	})

	sub("crash catching up before the indexes", func(t *testing.T) {
		h.freshForInit(false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fired := false
		testHooks.afterApply = func() error {
			if !fired && h.phase() == phaseCatchingUp && h.indexCount(stagingDB) == 0 {
				fired = true
				cancel()
				return errCrash
			}
			return nil
		}
		err := h.initUnderLoad(ctx)
		testHooks.afterApply = nil
		if !fired || !errors.Is(err, errCrash) {
			t.Fatalf("no crash while catching up (fired %v): %v", fired, err)
		}
		h.resumeInit()
		h.done(false)
	})

	sub("crash after the first index", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("added-first-index", phaseCatchingUp)
		if n := h.indexCount(stagingDB); n == 0 {
			t.Fatal("expected a partly restored staging copy")
		}
		h.resumeInit()
		h.done(false)
	})

	sub("crash just before the swap", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("before-rename", phaseSwapping)
		h.resumeInit()
		h.done(false)
	})

	sub("crash just after the swap", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("after-rename", phaseSwapping)
		h.resumeInit()
		h.done(false)
	})

	sub("crash just after the swap, first install", func(t *testing.T) {
		h.freshForInit(true)
		h.crashInit("after-rename", phaseSwapping)
		h.resumeInit()
		h.done(true)
	})

	sub("run refused while init holds the lock", func(t *testing.T) {
		h.freshForInit(false)
		var runErr error
		testHooks.initStep = func(s string) error {
			if s == "copied-first-table" && runErr == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, runErr = Run(ctx, h.opts())
				if runErr == nil {
					runErr = errors.New("run started")
				}
			}
			return nil
		}
		if err := h.initUnderLoad(context.Background()); err != nil {
			t.Fatal(err)
		}
		var s *StopError
		if !errors.As(runErr, &s) || !strings.Contains(runErr.Error(), "another growth-lab-cdc process") {
			t.Fatalf("run should be refused by the lock: %v", runErr)
		}
		h.done(false)
	})

	sub("lock connection killed", func(t *testing.T) {
		h.freshForInit(false)
		var killErr error
		killed := false
		testHooks.initStep = func(s string) error {
			if s == "before-restore" && !killed {
				killed = true
				var id sql.NullInt64
				if killErr = h.dstDB.QueryRow("SELECT IS_USED_LOCK(?)", lockName).Scan(&id); killErr == nil {
					_, killErr = h.dstDB.Exec(fmt.Sprintf("KILL %d", id.Int64))
				}
				time.Sleep(2 * time.Second) // the lock watcher notices
			}
			return nil
		}
		if err := h.initUnderLoad(context.Background()); err != nil {
			t.Fatalf("init should take the lock again and finish: %v", err)
		}
		if !killed || killErr != nil {
			t.Fatalf("lock connection killed: %v, %v", killed, killErr)
		}
		h.done(false)
	})

	sub("copy write fails, then copy again", func(t *testing.T) {
		h.freshForInit(false)
		failed := false
		testHooks.initStep = func(s string) error {
			if s == "copied-first-table" && !failed {
				failed = true
				// the next staging table disappears under the copy
				h.exec(h.dstDB, "DROP TABLE "+stagingDB+".member")
			}
			return nil
		}
		err := h.initUnderLoad(context.Background())
		testHooks.initStep = nil
		if err == nil || h.phase() != phaseCopying {
			t.Fatalf("init should stop in copying: %v (phase %s)", err, h.phase())
		}
		h.resumeInit()
		h.done(false)
	})

	sub("duplicate when restoring a unique index", func(t *testing.T) {
		h.freshForInit(false)
		testHooks.initStep = func(s string) error {
			if s == "before-restore" {
				var handle string
				h.dstDB.QueryRow("SELECT handle FROM " + stagingDB + ".member WHERE handle IS NOT NULL LIMIT 1").Scan(&handle)
				h.exec(h.dstDB, "INSERT INTO "+stagingDB+".member (name, handle, locale) VALUES ('dup', ?, 'EN')", handle)
			}
			return nil
		}
		err := h.initUnderLoad(context.Background())
		var s *StopError
		if !errors.As(err, &s) || !strings.Contains(err.Error(), "duplicates") {
			t.Fatalf("expected a stop on the duplicate: %v", err)
		}
	})

	sub("schema change while copying", func(t *testing.T) {
		h.freshForInit(false)
		testHooks.initStep = func(s string) error {
			if s == "copied-first-table" {
				// a table the snapshot has not read yet
				h.exec(h.srcDB, "ALTER TABLE app.team_member ADD COLUMN extra int NULL")
			}
			return nil
		}
		err := h.initUnderLoad(context.Background())
		if err == nil {
			t.Fatal("init should not finish after a schema change during the copy")
		}
		t.Logf("stopped: %s", SafeError(err))
		if p := h.phase(); p != phaseCopying {
			t.Fatalf("phase %q, want copying", p)
		}
	})

	sub("swap refused when a staging table was replaced", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("before-rename", phaseSwapping)
		h.rebuild(stagingDB, "member")
		err := h.initUnderLoad(context.Background())
		var s *StopError
		if !errors.As(err, &s) || !strings.Contains(err.Error(), "cannot tell whether the swap happened") {
			t.Fatalf("expected a stop: %v", err)
		}
	})

	sub("swap refused when a live table was replaced after it", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("after-rename", phaseSwapping)
		h.rebuild("app", "member")
		err := h.initUnderLoad(context.Background())
		var s *StopError
		if !errors.As(err, &s) || !strings.Contains(err.Error(), "cannot tell whether the swap happened") {
			t.Fatalf("expected a stop: %v", err)
		}
	})

	sub("no writes on the source", func(t *testing.T) {
		h.freshForInit(false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := Init(ctx, h.initOpts()); err != nil {
			t.Fatalf("init without source writes: %v", err)
		}
		h.done(false)
	})

	sub("a row stored before init", func(t *testing.T) {
		h.freshForInit(false)
		if err := Init(context.Background(), h.initOpts()); err != nil {
			t.Fatal(err)
		}
		// as stored before init: no phase column, a fingerprint in the older form
		h.exec(h.dstDB, "ALTER TABLE "+positionTable+" DROP COLUMN phase")
		h.exec(h.dstDB, "UPDATE "+positionTable+" SET schema_fp = REPEAT('0', 64)")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := Run(ctx, h.opts())
		if !strings.Contains(fmt.Sprint(err), "before init existed") {
			t.Fatalf("run on an old row: %v", err)
		}
		if err := Init(ctx, h.initOpts()); !strings.Contains(fmt.Sprint(err), "before init existed") {
			t.Fatalf("init on an old row: %v", err)
		}
	})

	sub("first run with --from that fails its checks stores nothing", func(t *testing.T) {
		h.freshForInit(false)
		h.exec(h.dstDB, "ALTER TABLE app.member ADD COLUMN junk int NULL")
		st, err := h.src.ReadStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		o := h.opts()
		o.From = &mysql.Position{Name: st.Current.Name, Pos: st.Current.Pos}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err = Run(ctx, o)
		var stop *StopError
		if !errors.As(err, &stop) {
			t.Fatalf("expected a stop: %v", err)
		}
		var n int
		h.dstDB.QueryRow("SELECT COUNT(*) FROM " + positionTable).Scan(&n)
		if n != 0 {
			t.Fatalf("%d position rows left after a failed first run", n)
		}
	})

	sub("copy over its time limit", func(t *testing.T) {
		h.freshForInit(false)
		o := h.initOpts()
		o.MaxCopy = time.Nanosecond
		err := Init(context.Background(), o)
		if !strings.Contains(fmt.Sprint(err), "the copy took longer") || h.phase() != phaseCopying {
			t.Fatalf("expected a stop in copying: %v (phase %s)", err, h.phase())
		}
	})

	sub("a process that lost the lock cannot write", func(t *testing.T) {
		h.freshForInit(false)
		ctx := context.Background()
		a, err := takeLock(ctx, h.dstDB)
		if err != nil {
			t.Fatal(err)
		}
		defer a.release()
		var id int64
		if err := a.conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
			t.Fatal(err)
		}
		h.exec(h.dstDB, fmt.Sprintf("KILL %d", id))
		var b *copyLock
		for i := 0; i < 50; i++ {
			if b, err = takeLock(ctx, h.dstDB); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer b.release()
		// a has not noticed yet: its writes are refused by the epoch
		stale := newTarget(h.dstDB, "x", "app", nil, a.epoch)
		if err := stale.Begin(ctx, Strict); err != nil {
			t.Fatal(err)
		}
		if err := stale.Commit(ctx, "-", ""); !errors.Is(err, errLockLost) {
			t.Fatalf("commit of a stale process: %v", err)
		}
		if err := stale.Save(ctx, "-", ""); !errors.Is(err, errLockLost) {
			t.Fatalf("save of a stale process: %v", err)
		}
		if err := b.check(ctx, h.dstDB); err != nil {
			t.Fatalf("the new holder: %v", err)
		}
	})

	sub("no other process takes over during a DDL", func(t *testing.T) {
		h.freshForInit(false)
		var takeErr error
		var renameWaited bool
		var epochBefore, epochAfter int64
		blocked := make(chan struct{})
		testHooks.initStep = func(s string) error {
			if s != "before-rename" || renameWaited {
				return nil
			}
			renameWaited = true
			if err := h.dstDB.QueryRow("SELECT epoch FROM " + epochTable).Scan(&epochBefore); err != nil {
				return err
			}
			// a table lock elsewhere keeps the RENAME waiting for 5 seconds once it runs
			holder, err := h.dstDB.Conn(context.Background())
			if err != nil {
				return err
			}
			if _, err := holder.ExecContext(context.Background(), "LOCK TABLES "+stagingDB+".member READ"); err != nil {
				holder.Close()
				return err
			}
			go func() {
				defer close(blocked)
				defer holder.Close()
				renaming := func() bool {
					var n int
					if err := h.dstDB.QueryRow("SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE INFO LIKE 'RENAME TABLE%'").Scan(&n); err != nil {
						return false
					}
					return n > 0
				}
				for i := 0; i < 50 && !renaming(); i++ {
					time.Sleep(100 * time.Millisecond)
				}
				if !renaming() {
					takeErr = errors.New("the RENAME never started waiting")
					holder.ExecContext(context.Background(), "UNLOCK TABLES")
					return
				}
				// another process starting while the RENAME runs on the lock's connection
				var l *copyLock
				if l, takeErr = takeLock(context.Background(), h.dstDB); takeErr == nil {
					l.release()
					takeErr = errors.New("took the lock during the swap")
				}
				if !renaming() {
					takeErr = errors.New("the RENAME stopped waiting before the lock was tried")
				}
				time.Sleep(5 * time.Second)
				holder.ExecContext(context.Background(), "UNLOCK TABLES")
			}()
			return nil
		}
		started := time.Now()
		err := h.initUnderLoad(context.Background())
		<-blocked
		if err != nil {
			t.Fatalf("init across a long RENAME (no lost lock expected): %v", err)
		}
		if time.Since(started) < 5*time.Second {
			t.Fatalf("the RENAME did not wait (%v)", takeErr)
		}
		// a lock wrongly taken as lost would have been taken again, raising the epoch
		if err := h.dstDB.QueryRow("SELECT epoch FROM " + epochTable).Scan(&epochAfter); err != nil {
			t.Fatal(err)
		}
		if epochAfter != epochBefore {
			t.Fatalf("epoch %d before the RENAME, %d after: the long RENAME was taken as a lost lock", epochBefore, epochAfter)
		}
		if !strings.Contains(fmt.Sprint(takeErr), "another growth-lab-cdc process") {
			t.Fatalf("taking the lock during the swap should be refused: %v", takeErr)
		}
		h.done(false)
	})

	sub("kill -9 of the init process", func(t *testing.T) {
		h.freshForInit(false)
		stop := h.background()
		kills := 0
		for _, at := range []string{phaseCopying, phaseCatchingUp} {
			cmd := h.initBinary()
			deadline := time.Now().Add(60 * time.Second)
			for h.phase() != at && h.phase() != phaseReady && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if p := h.phase(); p == at {
				cmd.Process.Signal(syscall.SIGKILL)
				kills++
				t.Logf("killed in %s", at)
			}
			cmd.Wait()
		}
		if werr := stop(); werr != nil {
			t.Fatalf("writer: %v", werr)
		}
		if kills == 0 {
			t.Fatal("init finished before any kill")
		}
		h.resumeInit()
		h.done(false)
	})
}

// rebuild recreates a table of the copy with the same rows: another InnoDB table under the same name.
func (h *harness) rebuild(db, table string) {
	h.t.Helper()
	h.exec(h.dstDB, "ALTER TABLE "+quoteName(db)+"."+quoteName(table)+" ENGINE=InnoDB, ALGORITHM=COPY")
}

// initBinary runs init as the command-line binary, for cases that kill the process.
func (h *harness) initBinary() *exec.Cmd {
	h.t.Helper()
	cmd := exec.Command(os.Getenv("CDC_BIN"), "init", "--defaults-file", os.Getenv("CDC_SRC_CNF"), "--target-defaults-file", os.Getenv("CDC_DST_CNF"),
		"--database", "app", "--tables", strings.Join(followed, ","))
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	return cmd
}

// indexCount counts secondary indexes in a database of the copy.
func (h *harness) indexCount(db string) int {
	var n int
	h.dstDB.QueryRow("SELECT COUNT(DISTINCT TABLE_NAME, INDEX_NAME) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND INDEX_NAME <> 'PRIMARY'", db).Scan(&n)
	return n
}
