//go:build integration

// Integration tests for run against two local MySQL servers. They create and drop the databases
// app and app_other on both. They need:
//
//	CDC_SRC_CNF, CDC_DST_CNF  option files (root) for the source and the copy
//	CDC_BIN                   growth-lab-cdc binary, for the kill -9 cases
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

	"growth-lab-cdc/internal/cdc"
	"growth-lab-cdc/internal/target"
	"growth-lab-cdc/internal/testenv"
)

type harness struct{ *testenv.Env }

func newHarness(t *testing.T) *harness { return &harness{testenv.New(t)} }

func (h *harness) opts() RunOptions {
	o := RunOptions{Source: h.Src, Target: h.Dst, Database: "app", Tables: testenv.Followed, SaveEvery: 200 * time.Millisecond,
		StopWait: 5 * time.Second, Logf: h.T.Logf}
	if h.From != nil {
		o.From = h.From
		h.From = nil
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
		"--database", "app", "--tables", strings.Join(testenv.Followed, ",")}
	if o.From != nil {
		args = append(args, "--from", fmt.Sprintf("%s:%d", o.From.Name, o.From.Pos))
	}
	cmd := exec.Command(os.Getenv("CDC_BIN"), args...)
	if err := cmd.Start(); err != nil {
		h.T.Fatal(err)
	}
	return cmd
}

// finish runs until the copy reaches the source's last boundary, stops gracefully and compares.
func (h *harness) finish() {
	h.T.Helper()
	h.Load(5)
	want := h.Boundary()
	ctx, cancel := context.WithCancel(context.Background())
	ch := h.start(ctx)
	deadline := time.Now().Add(20 * time.Second)
	for h.SavedOr().Position().Compare(want) < 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	if err := <-ch; err != nil {
		h.T.Fatalf("run: %v", err)
	}
	if got := h.Saved().Position(); got.Compare(want) != 0 {
		h.T.Fatalf("stored position %s, source boundary %s", got, want)
	}
	h.Compare()
}

var errCrash = errors.New("crash for the test")

func clearHooks() {
	target.Hooks.AfterApply, target.Hooks.BeforeCommit, target.Hooks.AfterCommit = nil, nil, nil
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
	h.T.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer clearHooks()
	fired := set(cancel)
	ch := h.start(ctx)
	h.Load(60)
	var err error
	select {
	case err = <-ch:
	case <-time.After(5 * time.Second): // the uncertain cases retry and keep running
		cancel()
		err = <-ch
	}
	if !*fired {
		h.T.Fatal("the hook never fired")
	}
	return err
}

func TestRunRecovers(t *testing.T) {
	h := newHarness(t)

	t.Run("first position stored but the reply lost", func(t *testing.T) {
		parent := h.T
		defer func() { h.T = parent }()
		from := *h.From
		ctx, cancel := context.WithCancel(context.Background())
		ch := h.start(ctx)
		for i := 0; i < 50 && h.SavedOr().Pos == 0; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		cancel()
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
		if got := h.Saved(); got.Position().Compare(from) != 0 || got.EventCRC != 0 {
			t.Fatalf("expected only the --from position to be stored: %+v", got)
		}
		h.From = &from // run again with --from, as a retry after a lost reply would
		h.T = t
		h.finish()
	})

	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			parent := h.T
			h.T = t
			defer func() { h.T = parent }()
			f(t)
		})
	}

	run("crash before commit", func(t *testing.T) {
		err := h.crashAt(func(cancel context.CancelFunc) *bool {
			hit, fired := onCall(3)
			target.Hooks.BeforeCommit = func(tx *sql.Tx) error {
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
			target.Hooks.AfterCommit = func() error {
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
			target.Hooks.AfterCommit = func() error {
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
			target.Hooks.BeforeCommit = func(tx *sql.Tx) error {
				if hit() {
					tx.Rollback()
					return &cdc.UncertainError{Err: errors.New("reply lost")}
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
		before := h.Saved().Position()
		cmd := h.startBinary()
		time.Sleep(time.Second)
		for i := 0; i < 20; i++ {
			h.Exec(h.SrcDB, "INSERT INTO app_other.note (v) VALUES (?)", i)
		}
		time.Sleep(12 * time.Second) // past the default save interval of the binary
		h.Exec(h.SrcDB, "INSERT INTO app_other.note (v) VALUES (0)")
		time.Sleep(500 * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()
		if got := h.Saved(); got.Position().Compare(before) <= 0 || got.EventCRC == 0 {
			t.Fatalf("no position was stored for the unfollowed transactions: %+v", got)
		}
		h.finish()
	})

	run("crash while applying, then rotate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer clearHooks()
		hit, fired := onCall(3)
		target.Hooks.AfterApply = func() error {
			if hit() {
				h.Exec(h.SrcDB, "FLUSH BINARY LOGS") // the source moves on to a new file while the copy transaction is open
				cancel()
				return errCrash
			}
			return nil
		}
		ch := h.start(ctx)
		tx, err := h.SrcDB.Begin()
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
		target.Hooks.AfterApply = func() error {
			calls++
			switch calls {
			case 3:
				killErr = errors.Join(killErr, killAll(h.SrcDB, "Command = 'Binlog Dump'"))
				srcKilled = true
			case 40:
				killErr = errors.Join(killErr, killAll(h.DstDB, "USER = 'root' AND ID <> CONNECTION_ID()"))
				dstKilled = true
			}
			return nil
		}
		ch := h.start(ctx)
		for i := 0; i < 6 && !(srcKilled && dstKilled); i++ {
			h.Load(40)
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
		target.Hooks.AfterApply = func() error {
			if hit() {
				cancel()
			}
			return nil
		}
		ch := h.start(ctx)
		h.Exec(h.SrcDB, `INSERT INTO app.post (member_id, body, created, changed)
			SELECT member_id, 'bulk', NOW(6), NOW(6) FROM app.post LIMIT 3000`)
		want := h.Boundary()
		if err := <-ch; err != nil {
			t.Fatalf("graceful stop: %v", err)
		}
		if !*fired {
			t.Fatal("the stop was not requested while applying")
		}
		if got := h.Saved().Position(); got.Compare(want) != 0 {
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
	h.Reset()
	h.Load(20)
	if prepare != nil {
		prepare()
	}
	h.finish()
	before := h.Saved()
	inject()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := <-h.start(ctx)
	var s *cdc.StopError
	if !errors.As(err, &s) {
		t.Fatalf("expected a stop, got %v", err)
	}
	t.Logf("stopped: %s", err)
	if got := h.Saved(); got != before {
		t.Fatalf("stored position changed: %+v, before the problem %+v", got, before)
	}
}

func TestRunStopsOnLiveSchemaChange(t *testing.T) {
	h := newHarness(t)
	h.Load(20)
	h.finish()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ch := h.start(ctx)
	time.Sleep(time.Second)
	h.Load(10)
	h.Exec(h.SrcDB, "ALTER TABLE app.label ADD COLUMN extra int NULL")
	err := <-ch
	var s *cdc.StopError
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
			h.Exec(h.DstDB, "UPDATE app.member SET name = 'changed' ORDER BY id DESC LIMIT 1")
			h.Exec(h.SrcDB, "UPDATE app.member SET name = 'src' ORDER BY id DESC LIMIT 1")
		}},
		{"copy differs only in case", func() {
			h.Exec(h.SrcDB, "UPDATE app.member SET name = 'abc' ORDER BY id DESC LIMIT 1")
		}, func() {
			h.Exec(h.DstDB, "UPDATE app.member SET name = 'ABC' ORDER BY id DESC LIMIT 1")
			h.Exec(h.SrcDB, "UPDATE app.member SET name = 'next' ORDER BY id DESC LIMIT 1")
		}},
		{"copy has the row being inserted", nil, func() {
			var id int64
			h.SrcDB.QueryRow("SELECT MAX(id) + 1 FROM app.like_mark").Scan(&id)
			h.Exec(h.DstDB, "INSERT INTO app.like_mark (id, member_id, post_id, created) VALUES (?, 1, 1, NOW())", id)
			h.Exec(h.SrcDB, "INSERT INTO app.like_mark (id, member_id, post_id, created) VALUES (?, 2, 2, NOW())", id)
		}},
		{"copy lacks the row being deleted", nil, func() {
			h.Exec(h.DstDB, "DELETE FROM app.like_mark ORDER BY id DESC LIMIT 1")
			h.Exec(h.SrcDB, "DELETE FROM app.like_mark ORDER BY id DESC LIMIT 1")
		}},
		{"schema change", nil, func() {
			h.Exec(h.SrcDB, "ALTER TABLE app.label ADD COLUMN extra int NULL")
		}},
		{"statement-format insert", nil, func() {
			conn, err := h.SrcDB.Conn(context.Background())
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
			conn, err := h.SrcDB.Conn(context.Background())
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
			h.T = t
			stopCase(t, h, c.prepare, c.inject)
		})
	}
}
