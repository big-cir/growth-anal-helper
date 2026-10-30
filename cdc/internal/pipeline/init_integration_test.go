//go:build integration

package pipeline

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

	"growth-lab-cdc/internal/cdc"
	"growth-lab-cdc/internal/schema"
	"growth-lab-cdc/internal/target"
	"growth-lab-cdc/internal/testenv"
)

func (h *harness) initOpts() InitOptions {
	return InitOptions{Source: h.Src, Target: h.Dst, Database: "app", Tables: testenv.Followed,
		SaveEvery: 200 * time.Millisecond, StopWait: 5 * time.Second, Logf: h.T.Logf}
}

// freshForInit rebuilds both sides; the copy keeps last generation's tables, made stale, or none.
func (h *harness) freshForInit(firstInstall bool) {
	h.T.Helper()
	h.Reset()
	h.From = nil
	for _, q := range []string{"DROP DATABASE IF EXISTS " + target.StagingDB, "DROP DATABASE IF EXISTS " + target.OldDB} {
		h.Exec(h.DstDB, q)
	}
	if firstInstall {
		h.Exec(h.DstDB, "DROP DATABASE app")
		return
	}
	// the copy misses updates and deletes, as the current sync does
	h.Exec(h.DstDB, "UPDATE app.member SET name = 'stale' ORDER BY id LIMIT 50")
	h.Exec(h.DstDB, "DELETE FROM app.post ORDER BY id DESC LIMIT 30")
	h.Exec(h.DstDB, "INSERT INTO app.like_mark (member_id, post_id, created) VALUES (777777777, 1, NOW())")
}

// initUnderLoad runs init while the source keeps changing, and returns init's error.
func (h *harness) initUnderLoad(ctx context.Context) error {
	h.T.Helper()
	stop := h.Background()
	err := Init(ctx, h.initOpts())
	if werr := stop(); werr != nil {
		h.T.Fatalf("writer: %v", werr)
	}
	return err
}

func (h *harness) phase() string {
	var p string
	if err := h.DstDB.QueryRow("SELECT phase FROM " + target.PositionTable).Scan(&p); err != nil {
		return ""
	}
	return p
}

func (h *harness) tablesIn(db string) int {
	h.T.Helper()
	var n int
	if err := h.DstDB.QueryRow("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", db).Scan(&n); err != nil {
		h.T.Fatal(err)
	}
	return n
}

// done checks that init finished, then that run takes over and the copy matches the source.
func (h *harness) done(firstInstall bool) {
	h.T.Helper()
	if p := h.phase(); p != target.PhaseReady {
		h.T.Fatalf("phase %q after init", p)
	}
	old := len(testenv.Followed)
	if firstInstall {
		old = 0
	}
	if got := h.tablesIn(target.StagingDB); got != 0 {
		h.T.Fatalf("%d tables left in staging", got)
	}
	if got := h.tablesIn(target.OldDB); got != old {
		h.T.Fatalf("%d tables in %s, want %d", got, target.OldDB, old)
	}
	var owners int
	h.DstDB.QueryRow("SELECT COUNT(*) FROM " + target.OwnerTable).Scan(&owners)
	if owners != 0 {
		h.T.Fatalf("%d staging owner rows left", owners)
	}
	h.finish()
	// the live tables carry the source's secondary indexes again
	o := h.opts()
	defs, err := schema.ReadDefs(context.Background(), h.DstDB, "", o.names())
	if err != nil {
		h.T.Fatal(err)
	}
	src, err := schema.ReadDefs(context.Background(), h.SrcDB, "", o.names())
	if err != nil {
		h.T.Fatal(err)
	}
	if err := schema.SameDefs(src, defs, true); err != nil {
		h.T.Fatal(err)
	}
}

// crashInit runs init under load with a step hook; the hook returns errCrash to stop it there.
func (h *harness) crashInit(step string, wantPhase string) {
	h.T.Helper()
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
		h.T.Fatalf("step %s never reached (init: %v)", step, err)
	}
	if !errors.Is(err, errCrash) {
		h.T.Fatalf("init should end with the crash at %s: %v", step, err)
	}
	if p := h.phase(); p != wantPhase {
		h.T.Fatalf("phase %q after a crash at %s, want %s", p, step, wantPhase)
	}
}

func (h *harness) resumeInit() {
	h.T.Helper()
	if err := h.initUnderLoad(context.Background()); err != nil {
		h.T.Fatalf("resumed init: %v", err)
	}
}

func TestInit(t *testing.T) {
	h := newHarness(t)
	sub := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			parent := h.T
			h.T = t
			defer func() { h.T = parent; clearHooks(); testHooks.initStep = nil }()
			f(t)
		})
	}

	sub("replaces stale tables under load", func(t *testing.T) {
		h.freshForInit(false)
		testHooks.initStep = func(s string) error {
			if s == "before-snapshot" {
				h.Exec(h.SrcDB, "INSERT INTO app.member (created, name, handle, locale) VALUES (NOW(6), 'between', 'between-x', 'KO')")
				h.Exec(h.SrcDB, "UPDATE app.team_member SET team_id = team_id + 5000 ORDER BY member_id LIMIT 2") // key change
				h.Exec(h.SrcDB, "DELETE FROM app.like_mark ORDER BY id LIMIT 2")
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
		h.crashInit("copied-first-table", target.PhaseCopying)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := Run(ctx, h.opts())
		var s *cdc.StopError
		if !errors.As(err, &s) || !strings.Contains(err.Error(), "middle of an init") {
			t.Fatalf("run during an unfinished init should be refused: %v", err)
		}
		h.resumeInit()
		h.done(false)
	})

	sub("refuses to continue a snapshot taken without the source lock", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("added-first-index", target.PhaseCatchingUp)
		h.Exec(h.DstDB, "UPDATE "+target.PositionTable+" SET snapshot_locked = 0")
		err := Init(context.Background(), h.initOpts())
		var s *cdc.StopError
		if !errors.As(err, &s) || !strings.Contains(err.Error(), "without locking") {
			t.Fatalf("an init from an earlier version should stop: %v", err)
		}
	})

	sub("crash catching up before the indexes", func(t *testing.T) {
		h.freshForInit(false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fired := false
		target.Hooks.AfterApply = func() error {
			if !fired && h.phase() == target.PhaseCatchingUp && h.indexCount(target.StagingDB) == 0 {
				fired = true
				cancel()
				return errCrash
			}
			return nil
		}
		err := h.initUnderLoad(ctx)
		target.Hooks.AfterApply = nil
		if !fired || !errors.Is(err, errCrash) {
			t.Fatalf("no crash while catching up (fired %v): %v", fired, err)
		}
		h.resumeInit()
		h.done(false)
	})

	sub("crash after the first index", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("added-first-index", target.PhaseCatchingUp)
		if n := h.indexCount(target.StagingDB); n == 0 {
			t.Fatal("expected a partly restored staging copy")
		}
		h.resumeInit()
		h.done(false)
	})

	sub("crash just before the swap", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("before-rename", target.PhaseSwapping)
		h.resumeInit()
		h.done(false)
	})

	sub("crash just after the swap", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("after-rename", target.PhaseSwapping)
		h.resumeInit()
		h.done(false)
	})

	sub("crash just after the swap, first install", func(t *testing.T) {
		h.freshForInit(true)
		h.crashInit("after-rename", target.PhaseSwapping)
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
		var s *cdc.StopError
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
				if killErr = h.DstDB.QueryRow("SELECT IS_USED_LOCK(?)", target.LockName).Scan(&id); killErr == nil {
					_, killErr = h.DstDB.Exec(fmt.Sprintf("KILL %d", id.Int64))
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
				h.Exec(h.DstDB, "DROP TABLE "+target.StagingDB+".member")
			}
			return nil
		}
		err := h.initUnderLoad(context.Background())
		testHooks.initStep = nil
		if err == nil || h.phase() != target.PhaseCopying {
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
				h.DstDB.QueryRow("SELECT handle FROM " + target.StagingDB + ".member WHERE handle IS NOT NULL LIMIT 1").Scan(&handle)
				h.Exec(h.DstDB, "INSERT INTO "+target.StagingDB+".member (name, handle, locale) VALUES ('dup', ?, 'EN')", handle)
			}
			return nil
		}
		err := h.initUnderLoad(context.Background())
		var s *cdc.StopError
		if !errors.As(err, &s) || !strings.Contains(err.Error(), "duplicates") {
			t.Fatalf("expected a stop on the duplicate: %v", err)
		}
	})

	sub("schema change while copying", func(t *testing.T) {
		h.freshForInit(false)
		testHooks.initStep = func(s string) error {
			if s == "copied-first-table" {
				// a table the snapshot has not read yet
				h.Exec(h.SrcDB, "ALTER TABLE app.team_member ADD COLUMN extra int NULL")
			}
			return nil
		}
		err := h.initUnderLoad(context.Background())
		if err == nil {
			t.Fatal("init should not finish after a schema change during the copy")
		}
		t.Logf("stopped: %s", cdc.SafeError(err))
		if p := h.phase(); p != target.PhaseCopying {
			t.Fatalf("phase %q, want copying", p)
		}
	})

	sub("swap refused when a staging table was replaced", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("before-rename", target.PhaseSwapping)
		h.rebuild(target.StagingDB, "member")
		err := h.initUnderLoad(context.Background())
		var s *cdc.StopError
		if !errors.As(err, &s) || !strings.Contains(err.Error(), "cannot tell whether the swap happened") {
			t.Fatalf("expected a stop: %v", err)
		}
	})

	sub("swap refused when a live table was replaced after it", func(t *testing.T) {
		h.freshForInit(false)
		h.crashInit("after-rename", target.PhaseSwapping)
		h.rebuild("app", "member")
		err := h.initUnderLoad(context.Background())
		var s *cdc.StopError
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
		h.Exec(h.DstDB, "ALTER TABLE "+target.PositionTable+" DROP COLUMN phase")
		h.Exec(h.DstDB, "UPDATE "+target.PositionTable+" SET schema_fp = REPEAT('0', 64)")
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
		h.Exec(h.DstDB, "ALTER TABLE app.member ADD COLUMN junk int NULL")
		st, err := h.Src.ReadStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		o := h.opts()
		o.From = &mysql.Position{Name: st.Current.Name, Pos: st.Current.Pos}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err = Run(ctx, o)
		var stop *cdc.StopError
		if !errors.As(err, &stop) {
			t.Fatalf("expected a stop: %v", err)
		}
		var n int
		h.DstDB.QueryRow("SELECT COUNT(*) FROM " + target.PositionTable).Scan(&n)
		if n != 0 {
			t.Fatalf("%d position rows left after a failed first run", n)
		}
	})

	sub("copy over its time limit", func(t *testing.T) {
		h.freshForInit(false)
		o := h.initOpts()
		o.MaxCopy = time.Nanosecond
		err := Init(context.Background(), o)
		if !strings.Contains(fmt.Sprint(err), "the copy took longer") || h.phase() != target.PhaseCopying {
			t.Fatalf("expected a stop in copying: %v (phase %s)", err, h.phase())
		}
	})

	sub("a process that lost the lock cannot write", func(t *testing.T) {
		h.freshForInit(false)
		ctx := context.Background()
		a, err := target.TakeLock(ctx, h.DstDB)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Release()
		var id int64
		if err := a.Do(func(c *sql.Conn) error { return c.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id) }); err != nil {
			t.Fatal(err)
		}
		h.Exec(h.DstDB, fmt.Sprintf("KILL %d", id))
		var b *target.Lock
		for i := 0; i < 50; i++ {
			if b, err = target.TakeLock(ctx, h.DstDB); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer b.Release()
		// a has not noticed yet: its writes are refused by the epoch
		stale := target.New(h.DstDB, "x", "app", nil, a.Epoch())
		if err := stale.Begin(ctx, cdc.Strict); err != nil {
			t.Fatal(err)
		}
		if err := stale.Commit(ctx, "-", ""); !errors.Is(err, target.ErrLockLost) {
			t.Fatalf("commit of a stale process: %v", err)
		}
		if err := stale.Save(ctx, "-", ""); !errors.Is(err, target.ErrLockLost) {
			t.Fatalf("save of a stale process: %v", err)
		}
		if err := b.Check(ctx, h.DstDB); err != nil {
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
			if err := h.DstDB.QueryRow("SELECT epoch FROM " + target.EpochTable).Scan(&epochBefore); err != nil {
				return err
			}
			// a table lock elsewhere keeps the RENAME waiting for 5 seconds once it runs
			holder, err := h.DstDB.Conn(context.Background())
			if err != nil {
				return err
			}
			if _, err := holder.ExecContext(context.Background(), "LOCK TABLES "+target.StagingDB+".member READ"); err != nil {
				holder.Close()
				return err
			}
			go func() {
				defer close(blocked)
				defer holder.Close()
				renaming := func() bool {
					var n int
					if err := h.DstDB.QueryRow("SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE INFO LIKE 'RENAME TABLE%'").Scan(&n); err != nil {
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
				var l *target.Lock
				if l, takeErr = target.TakeLock(context.Background(), h.DstDB); takeErr == nil {
					l.Release()
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
		if err := h.DstDB.QueryRow("SELECT epoch FROM " + target.EpochTable).Scan(&epochAfter); err != nil {
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
		stop := h.Background()
		kills := 0
		for _, at := range []string{target.PhaseCopying, target.PhaseCatchingUp} {
			cmd := h.initBinary()
			deadline := time.Now().Add(60 * time.Second)
			for h.phase() != at && h.phase() != target.PhaseReady && time.Now().Before(deadline) {
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
	h.T.Helper()
	h.Exec(h.DstDB, "ALTER TABLE "+cdc.QuoteName(db)+"."+cdc.QuoteName(table)+" ENGINE=InnoDB, ALGORITHM=COPY")
}

// initBinary runs init as the command-line binary, for cases that kill the process.
func (h *harness) initBinary() *exec.Cmd {
	h.T.Helper()
	cmd := exec.Command(os.Getenv("CDC_BIN"), "init", "--defaults-file", os.Getenv("CDC_SRC_CNF"), "--target-defaults-file", os.Getenv("CDC_DST_CNF"),
		"--database", "app", "--tables", strings.Join(testenv.Followed, ","))
	if err := cmd.Start(); err != nil {
		h.T.Fatal(err)
	}
	return cmd
}

// indexCount counts secondary indexes in a database of the copy.
func (h *harness) indexCount(db string) int {
	var n int
	h.DstDB.QueryRow("SELECT COUNT(DISTINCT TABLE_NAME, INDEX_NAME) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND INDEX_NAME <> 'PRIMARY'", db).Scan(&n)
	return n
}
