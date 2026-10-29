//go:build integration

package cdc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// sourceRows counts the rows of the followed tables on the source.
func (h *harness) sourceRows() int64 {
	h.t.Helper()
	var all int64
	for _, tb := range followed {
		var n int64
		if err := h.srcDB.QueryRow("SELECT COUNT(*) FROM app." + tb).Scan(&n); err != nil {
			h.t.Fatal(err)
		}
		all += n
	}
	return all
}

// checkLogged runs a check with writes going on, waiting at most wait for the copy per chunk,
// and returns its report and log.
func (h *harness) checkLogged(wait time.Duration) (*CheckReport, string) {
	h.t.Helper()
	var logs []string
	o := h.checkOpts(false)
	o.Wait = wait
	o.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	rep, err := Check(context.Background(), o)
	if err != nil {
		h.t.Fatalf("check: %v", err)
	}
	return rep, strings.Join(logs, "\n")
}

func TestRunSavesAtAHeartbeat(t *testing.T) {
	h := newHarness(t)
	h.finish()
	o := h.opts()
	o.SaveEvery, o.HeartbeatPeriod = time.Hour, time.Second
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() {
		_, err := Run(ctx, o)
		ch <- err
	}()
	// a followed change is stored with its transaction; the skipped one after it waits for a save
	h.exec(h.srcDB, "UPDATE app.label SET title = 'hb' WHERE id = 1")
	h.exec(h.srcDB, "INSERT INTO app_other.note (v) VALUES (1)")
	want := h.boundary()
	deadline := time.Now().Add(5 * time.Second)
	for h.saved().position().Compare(want) < 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := h.saved().position(); got.Compare(want) != 0 {
		t.Fatalf("stored %s after the source went quiet, want the skipped transaction's end %s", got, want)
	}
	cancel()
	if err := <-ch; err != nil {
		t.Fatalf("run: %v", err)
	}
	// the next run starts at the position a heartbeat stored and finds the same event there
	h.finish()
}

func TestInitWithHeartbeats(t *testing.T) {
	h := newHarness(t)
	h.freshForInit(false)
	o := h.initOpts()
	o.HeartbeatPeriod = time.Second
	stop := h.background()
	err := Init(context.Background(), o)
	if werr := stop(); werr != nil {
		t.Fatalf("writer: %v", werr)
	}
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	h.done(false)
}

func TestCheckWithWritesGoingOn(t *testing.T) {
	h := newHarness(t)
	h.finish() // run is stopped and has stored the source's end

	t.Run("a rotation with no transaction after the copy", func(t *testing.T) {
		h.t = t
		h.exec(h.srcDB, "FLUSH BINARY LOGS")
		rep, logs := h.checkLogged(3 * time.Second)
		h.want(rep, 0, 0, 0)
		if got, all := sum(rep).Rows, h.sourceRows(); got != all {
			t.Fatalf("compared %d rows of %d\n%s", got, all, logs)
		}
	})

	t.Run("a transaction the copy has not applied", func(t *testing.T) {
		h.t = t
		h.exec(h.srcDB, "INSERT INTO app_other.note (v) VALUES (2)")
		rep, logs := h.checkLogged(500 * time.Millisecond)
		if sum(rep).Undecided == 0 {
			t.Fatalf("check went on past a transaction the copy has not applied\n%s", logs)
		}
		if !strings.Contains(logs, "undecided: the copy stayed at") || !strings.Contains(logs, "with transactions between them") {
			t.Fatalf("the log does not say why:\n%s", logs)
		}
		if strings.Contains(logs, "Body ") || strings.Contains(logs, "label ") {
			t.Fatalf("the log shows row values:\n%s", logs)
		}
	})

	t.Run("run applying while other tables keep changing", func(t *testing.T) {
		h.t = t
		o := h.opts()
		o.HeartbeatPeriod = time.Second
		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan error, 1)
		go func() {
			_, err := Run(ctx, o)
			ch <- err
		}()
		done := make(chan struct{})
		wrote := make(chan error, 1)
		go func() {
			for i := 0; ; i++ {
				select {
				case <-done:
					wrote <- nil
					return
				case <-time.After(50 * time.Millisecond):
				}
				if _, err := h.srcDB.Exec("INSERT INTO app_other.note (v) VALUES (?)", i); err != nil {
					wrote <- err
					return
				}
			}
		}()
		rep, logs := h.checkLogged(3 * time.Second)
		close(done)
		if err := <-wrote; err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := <-ch; err != nil {
			t.Fatalf("run: %v", err)
		}
		h.want(rep, 0, 0, 0)
		if got, all := sum(rep).Rows, h.sourceRows(); got != all {
			t.Fatalf("compared %d rows of %d\n%s", got, all, logs)
		}
	})
}
