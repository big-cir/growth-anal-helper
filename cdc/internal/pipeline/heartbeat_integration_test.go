//go:build integration

package pipeline

import (
	"context"
	"testing"
	"time"
)

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
	h.Exec(h.SrcDB, "UPDATE app.label SET title = 'hb' WHERE id = 1")
	h.Exec(h.SrcDB, "INSERT INTO app_other.note (v) VALUES (1)")
	want := h.Boundary()
	deadline := time.Now().Add(5 * time.Second)
	for h.Saved().Position().Compare(want) < 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := h.Saved().Position(); got.Compare(want) != 0 {
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
	stop := h.Background()
	err := Init(context.Background(), o)
	if werr := stop(); werr != nil {
		t.Fatalf("writer: %v", werr)
	}
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	h.done(false)
}
