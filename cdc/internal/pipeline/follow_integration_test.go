//go:build integration

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"growth-lab-cdc/internal/target"
	"growth-lab-cdc/internal/testenv"
)

// follow runs Follow in the background with its log kept; stop cancels it and returns its error.
func (h *harness) follow(o RunOptions) (logs func() string, stop func() error) {
	var lines []string
	done := make(chan struct{})
	o.Logf = func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	ctx, cancel := context.WithCancel(context.Background())
	var err error
	go func() {
		_, err = Follow(ctx, o, 0)
		close(done)
	}()
	return func() string { return strings.Join(lines, "\n") }, func() error {
		cancel()
		<-done
		return err
	}
}

func (h *harness) epoch() int64 {
	h.T.Helper()
	var n int64
	if err := h.DstDB.QueryRow("SELECT epoch FROM " + target.EpochTable + " WHERE id = 1").Scan(&n); err != nil {
		h.T.Fatal(err)
	}
	return n
}

// caughtUp waits until Follow has finished init and the copy has applied the source's last transaction.
func (h *harness) caughtUp() {
	h.T.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for h.phase() != target.PhaseReady && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if p := h.phase(); p != target.PhaseReady {
		h.T.Fatalf("phase %q, not ready", p)
	}
	h.Load(5)
	want := h.Boundary()
	for h.SavedOr().Position().Compare(want) < 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := h.Saved().Position(); got.Compare(want) < 0 {
		h.T.Fatalf("stored %s, source %s", got, want)
	}
}

func TestFollow(t *testing.T) {
	h := newHarness(t)
	sub := func(name string, firstInstall bool, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			parent := h.T
			h.T = t
			defer func() { h.T = parent; testHooks.initStep = nil }()
			h.freshForInit(firstInstall)
			f(t)
		})
	}
	followOpts := func() RunOptions {
		o := h.opts()
		o.From = nil
		return o
	}

	for _, first := range []bool{true, false} {
		sub(fmt.Sprintf("builds the copy and follows (first install %v)", first), first, func(t *testing.T) {
			logs, stop := h.follow(followOpts())
			h.caughtUp()
			if err := stop(); err != nil {
				t.Fatalf("follow: %v", err)
			}
			if !strings.Contains(logs(), "init done") {
				t.Fatalf("no init in the log:\n%s", logs())
			}
			h.Compare()
			old := len(testenv.Followed)
			if first {
				old = 0
			}
			if got := h.tablesIn(target.OldDB); got != old {
				t.Fatalf("%d tables in %s, want %d", got, target.OldDB, old)
			}
		})
	}

	for _, c := range []struct{ step, phase string }{
		{"copied-first-table", target.PhaseCopying}, {"added-first-index", target.PhaseCatchingUp}, {"before-rename", target.PhaseSwapping},
	} {
		sub("finishes an init stopped at "+c.step, false, func(t *testing.T) {
			h.crashInit(c.step, c.phase)
			_, stop := h.follow(followOpts())
			h.caughtUp()
			if err := stop(); err != nil {
				t.Fatalf("follow: %v", err)
			}
			h.Compare()
		})
	}

	sub("goes straight to run on a ready copy", false, func(t *testing.T) {
		if err := Init(context.Background(), h.initOpts()); err != nil {
			t.Fatal(err)
		}
		before := h.epoch()
		logs, stop := h.follow(followOpts())
		h.caughtUp()
		if err := stop(); err != nil {
			t.Fatalf("follow: %v", err)
		}
		if strings.Contains(logs(), "copying") || strings.Contains(logs(), "the copy is ready") {
			t.Fatalf("init ran on a ready copy:\n%s", logs())
		}
		// one session took the lock; init would have taken it once more
		if got := h.epoch() - before; got != 1 {
			t.Fatalf("the lock was taken %d times", got)
		}
		h.Compare()
	})

	sub("--from on an empty copy takes it as given", false, func(t *testing.T) {
		h.Reset() // the copy has the rows and h.From their position
		o := h.opts()
		logs, stop := h.follow(o)
		h.caughtUp()
		if err := stop(); err != nil {
			t.Fatalf("follow: %v", err)
		}
		if strings.Contains(logs(), "copying") {
			t.Fatalf("init ran with --from:\n%s", logs())
		}
		h.Compare()
	})

	sub("--from on an unfinished init is refused", false, func(t *testing.T) {
		h.crashInit("copied-first-table", target.PhaseCopying)
		o := followOpts()
		o.From = h.From
		if o.From == nil {
			p := h.Boundary()
			o.From = &p
		}
		_, err := Follow(context.Background(), o, 0)
		if !strings.Contains(fmt.Sprint(err), "run again without --from") {
			t.Fatalf("follow should refuse --from: %v", err)
		}
	})

	sub("a ready row init did not leave stops it", false, func(t *testing.T) {
		if err := Init(context.Background(), h.initOpts()); err != nil {
			t.Fatal(err)
		}
		h.Exec(h.DstDB, "UPDATE "+target.PositionTable+" SET init_id = 'left'")
		_, err := Follow(context.Background(), followOpts(), 0)
		if !strings.Contains(fmt.Sprint(err), "not what init leaves") {
			t.Fatalf("follow should stop: %v", err)
		}
	})

	sub("init stops on a cascading foreign key", true, func(t *testing.T) {
		h.sourceFK("ON DELETE CASCADE")
		_, err := Follow(context.Background(), followOpts(), 0)
		if !strings.Contains(fmt.Sprint(err), "cascading actions") {
			t.Fatalf("follow should stop: %v", err)
		}
	})

	sub("passes the copy limit to init", true, func(t *testing.T) {
		_, err := Follow(context.Background(), followOpts(), time.Nanosecond)
		if !strings.Contains(fmt.Sprint(err), "the copy took longer than") {
			t.Fatalf("follow should stop at the copy limit: %v", err)
		}
	})

	sub("a cancel right after the copy is found missing starts no init", true, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		testHooks.initStep = func(s string) error {
			if s == "follow-before-init" {
				cancel()
			}
			return nil
		}
		_, err := Follow(ctx, followOpts(), 0)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("follow should end cancelled: %v", err)
		}
		if p := h.phase(); p != "" {
			t.Fatalf("init started: phase %q", p)
		}
	})

	sub("a cancel before the copy's state is read is not a success", true, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Follow(ctx, followOpts(), 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("follow should end cancelled: %v", err)
		}
	})

	sub("a cancel during init leaves it to resume", true, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		testHooks.initStep = func(s string) error {
			if s == "copied-first-table" {
				cancel()
			}
			return nil
		}
		if _, err := Follow(ctx, followOpts(), 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("follow should end cancelled: %v", err)
		}
		testHooks.initStep = nil
		if p := h.phase(); p != target.PhaseCopying {
			t.Fatalf("phase %q after the cancel", p)
		}
		_, stop := h.follow(followOpts())
		h.caughtUp()
		if err := stop(); err != nil {
			t.Fatalf("follow: %v", err)
		}
		h.Compare()
	})

	sub("a removed binlog stops it without a new copy", false, func(t *testing.T) {
		if err := Init(context.Background(), h.initOpts()); err != nil {
			t.Fatal(err)
		}
		h.Load(3)
		h.Exec(h.SrcDB, "FLUSH BINARY LOGS")
		var file string
		var x1, x2, x3, x4 any
		if err := h.SrcDB.QueryRow("SHOW BINARY LOG STATUS").Scan(&file, &x1, &x2, &x3, &x4); err != nil {
			t.Fatal(err)
		}
		h.Exec(h.SrcDB, "PURGE BINARY LOGS TO '"+file+"'")
		_, err := Follow(context.Background(), followOpts(), 0)
		if !strings.Contains(fmt.Sprint(err), "a fresh copy is needed") {
			t.Fatalf("follow should stop on the removed binlog: %v", err)
		}
		if p := h.phase(); p != target.PhaseReady || h.tablesIn(target.StagingDB) != 0 {
			t.Fatalf("follow started a new copy: phase %q, %d staging tables", p, h.tablesIn(target.StagingDB))
		}
	})

	// between the steps another process takes the lock, or finishes the init and lets go
	for _, step := range []string{"follow-before-init", "follow-before-run"} {
		sub("another process holds the lock at "+step, true, func(t *testing.T) {
			conn, err := h.DstDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			testHooks.initStep = func(s string) error {
				if s == step {
					_, err := conn.ExecContext(context.Background(), "SELECT GET_LOCK('"+target.LockName+"', 0)")
					return err
				}
				return nil
			}
			_, err = Follow(context.Background(), followOpts(), 0)
			if !strings.Contains(fmt.Sprint(err), "another growth-lab-cdc process") {
				t.Fatalf("follow should stop on the held lock: %v", err)
			}
		})
	}

	sub("another process finished the init in between", true, func(t *testing.T) {
		testHooks.initStep = func(s string) error {
			if s == "follow-before-init" {
				testHooks.initStep = nil
				return Init(context.Background(), h.initOpts())
			}
			return nil
		}
		logs, stop := h.follow(followOpts())
		h.caughtUp()
		if err := stop(); err != nil {
			t.Fatalf("follow: %v", err)
		}
		if !strings.Contains(logs(), "the copy is ready") {
			t.Fatalf("follow's init should find the copy ready:\n%s", logs())
		}
		h.Compare()
	})

	sub("the run command builds the copy and stops cleanly", true, func(t *testing.T) {
		cmd := exec.Command(os.Getenv("CDC_BIN"), "run", "--defaults-file", os.Getenv("CDC_SRC_CNF"), "--target-defaults-file", os.Getenv("CDC_DST_CNF"),
			"--database", "app", "--tables", strings.Join(testenv.Followed, ","))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		h.caughtUp()
		cmd.Process.Signal(syscall.SIGINT)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("run should exit 0: %v", err)
		}
		h.Compare()
	})
}
