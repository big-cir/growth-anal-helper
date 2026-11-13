package query

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// With FAKE_WORKER set the test binary acts as the query worker: FAKE_DELAY_MS waits before answering
// and FAKE_CHILD_PID makes it start a long-lived child first and write the child's pid there.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_WORKER") == "1" {
		io.ReadAll(os.Stdin)
		if p := os.Getenv("FAKE_CHILD_PID"); p != "" {
			c := exec.Command("sleep", "30")
			if err := c.Start(); err == nil {
				os.WriteFile(p, []byte(strconv.Itoa(c.Process.Pid)), 0o644)
			}
		}
		ms, _ := strconv.Atoi(os.Getenv("FAKE_DELAY_MS"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		os.Stdout.WriteString(`{"ok":true,"columns":[{"name":"n","table":null,"column":null}],"rows":[[["i",1]]],"more":false,"truncatedCells":0,"ms":1,"tables":[]}`)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeWorker(t *testing.T, delay time.Duration) {
	t.Setenv("FAKE_WORKER", "1")
	t.Setenv("FAKE_DELAY_MS", strconv.Itoa(int(delay.Milliseconds())))
	old := WorkerCommand
	WorkerCommand = func() (string, []string, error) { return os.Args[0], nil, nil }
	t.Cleanup(func() { WorkerCommand = old })
}

func lease(t *testing.T) *SlotLease {
	l, err := NewExecutionSlots(1, 1).Acquire(context.Background(), Interactive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	return l
}

func TestTimeoutStopsWorkerGroup(t *testing.T) {
	fakeWorker(t, 5*time.Second)
	pidFile := filepath.Join(t.TempDir(), "child")
	t.Setenv("FAKE_CHILD_PID", pidFile)
	t0 := time.Now()
	r := RunQuery(Request{Lease: lease(t), SQL: "SELECT 1", Mode: Probe, Timeout: 300 * time.Millisecond})
	if took := time.Since(t0); took > time.Second {
		t.Errorf("took %v", took)
	}
	if r.Kind != "timeout" || !strings.Contains(r.Message, "0.3s") {
		t.Fatalf("got %+v", r)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	for i := 0; syscall.Kill(pid, 0) == nil; i++ {
		if i == 100 {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the worker's child survived the timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTimeoutKeepsFastQuery(t *testing.T) {
	fakeWorker(t, 100*time.Millisecond)
	r := RunQuery(Request{Lease: lease(t), SQL: "SELECT 1", Mode: Probe, Timeout: 2 * time.Second})
	if !r.OK || len(r.Rows) != 1 {
		t.Fatalf("got %+v", r)
	}
}

func TestStopKind(t *testing.T) {
	for _, c := range []struct {
		cancelled, timedOut, exitedOK bool
		want                          string
	}{
		{true, true, false, "cancelled"},
		{true, false, false, "cancelled"},
		{false, true, false, "timeout"},
		{false, true, true, ""},
		{false, false, false, ""},
	} {
		if got := stopKind(c.cancelled, c.timedOut, c.exitedOK); got != c.want {
			t.Errorf("%+v: got %q", c, got)
		}
	}
}

// The deadline and the cancel have both passed when the worker starts: the request reads as cancelled.
func TestCancelWinsOverDeadline(t *testing.T) {
	fakeWorker(t, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	WorkerCommand = func() (string, []string, error) {
		time.Sleep(10 * time.Millisecond)
		cancel()
		return os.Args[0], nil, nil
	}
	r := RunQuery(Request{Lease: lease(t), SQL: "SELECT 1", Mode: Probe, Timeout: time.Millisecond, Ctx: ctx})
	if r.Kind != "cancelled" {
		t.Fatalf("got %+v", r)
	}
}

// Time spent waiting for a slot does not count against the limit.
func TestTimeoutExcludesSlotWait(t *testing.T) {
	fakeWorker(t, 200*time.Millisecond)
	slots := NewExecutionSlots(1, 1)
	held, err := slots.Acquire(context.Background(), Interactive)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, held.Release)
	var r Result
	err = slots.Run(context.Background(), Interactive, func(l *SlotLease) error {
		r = RunQuery(Request{Lease: l, SQL: "SELECT 1", Mode: Probe, Timeout: 300 * time.Millisecond})
		return nil
	})
	if err != nil || !r.OK {
		t.Fatalf("got %+v %v", r, err)
	}
}
