package panels

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"growth-lab/internal/collect"
	"growth-lab/internal/coverage"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/query"
	"growth-lab/internal/sqlitec"
)

// With FAKE_WORKER set the test binary is the query worker. It appends each path to FAKE_LOG and
// stalls on paths that contain FAKE_SLOW; other queries run for real.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_WORKER") == "1" {
		buf, _ := io.ReadAll(os.Stdin)
		in, err := query.ParseInput(buf)
		if err != nil {
			os.Exit(2)
		}
		if f, err := os.OpenFile(os.Getenv("FAKE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.WriteString(filepath.Base(in.Path) + "\n")
			f.Close()
		}
		if slow := os.Getenv("FAKE_SLOW"); slow != "" && strings.Contains(in.Path, slow) {
			time.Sleep(5 * time.Second)
		}
		os.Stdout.WriteString(jsjson.MustStringify(query.Run(in)))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func timeoutRun(t *testing.T, slow string) (RunResult, string) {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"real.sqlite", "agent.sqlite", "gap.sqlite"} {
		p := filepath.Join(dir, name)
		db, err := sqlitec.Open(p, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE d_doc (id INTEGER PRIMARY KEY, at TEXT); INSERT INTO d_doc VALUES (1, '2024-03-01 00:00:00'), (2, '2024-03-02 00:00:00')"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		paths = append(paths, p)
	}
	logPath := filepath.Join(dir, "log")
	t.Setenv("FAKE_WORKER", "1")
	t.Setenv("FAKE_SLOW", slow)
	t.Setenv("FAKE_LOG", logPath)
	old := query.WorkerCommand
	query.WorkerCommand = func() (string, []string, error) { return os.Args[0], nil, nil }
	t.Cleanup(func() { query.WorkerCommand = old })
	l, err := query.NewExecutionSlots(1, 1).Acquire(context.Background(), query.Interactive)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	cov := &coverage.Resolved{Gaps: [][2]string{{"2024-01-01 00:00:00", "2024-02-01 00:00:00"}}, Events: []coverage.Column{{Table: "d_doc", Time: "at"}}, GapPaths: []string{paths[2]}}
	r := Run(RunInput{
		Spec:     Spec{Title: "docs", Question: "how many docs", SQL: "SELECT count(*) AS n FROM d_doc", Display: Display{Type: "table"}},
		RealPath: paths[0], AgentPath: paths[1], AsOf: "2024-06-01 00:00:00.000000", PanelPrefix: []string{"d_"},
		HeapLimitMb: 64, Roles: collect.NewRoles(), Lease: l, Coverage: cov, Lang: "ko", Timeout: 300 * time.Millisecond,
	})
	b, _ := os.ReadFile(logPath)
	return r, string(b)
}

func TestRunTimeoutOnRealCopy(t *testing.T) {
	r, log := timeoutRun(t, "real.sqlite")
	if r.OK || r.Stage != "timeout" {
		t.Fatalf("got %+v", r)
	}
	if strings.Contains(log, "agent.sqlite") {
		t.Errorf("ran the pseudonymized copy after a timeout: %q", log)
	}
}

func TestRunTimeoutOnAgentCopy(t *testing.T) {
	r, _ := timeoutRun(t, "agent.sqlite")
	if r.OK || r.Stage != "timeout" {
		t.Fatalf("got %+v", r)
	}
}

func TestRunTimeoutOnGapCopyPasses(t *testing.T) {
	r, log := timeoutRun(t, "gap.sqlite")
	if !r.OK {
		t.Fatalf("got %+v", r)
	}
	if !strings.Contains(log, "gap.sqlite") || len(r.Signals) != 1 || r.Signals[0].Caveat != "데이터 공백 확인을 실행하지 못했어요" {
		t.Errorf("signals %+v, log %q", r.Signals, log)
	}
}
