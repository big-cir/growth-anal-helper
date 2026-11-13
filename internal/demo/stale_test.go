package demo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"growth-lab/internal/snapshot"
	"growth-lab/internal/workspace"
)

const demoEvents = `[{"table": "r_post", "time": "created_at"}, {"table": "r_reaction", "time": "created_at"}]`

// collectLog runs collect with the source clock at now and the given event tables and returns the log lines.
func collectLog(t *testing.T, now string, events string) []string {
	t.Helper()
	dir := demoDir(t)
	if err := os.WriteFile(filepath.Join(dir, "coverage.json"), []byte(`{"events": `+events+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROWTH_LAB_TEST_SOURCE_NOW", now)
	var lines []string
	if _, err := snapshot.RunCollect(ws, func(m string) { lines = append(lines, m) }, nil); err != nil {
		t.Fatal(err)
	}
	return lines
}

func warnings(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "warning:") {
			out = append(out, l)
		}
	}
	return out
}

func TestCollectWarnsWhenEventsEndEarly(t *testing.T) {
	w := warnings(collectLog(t, "2024-06-08 12:00:00.000000", demoEvents))
	if len(w) != 1 || !strings.Contains(w[0], "complete through 2024-06-02, not through the day before the cutoff (2024-06-07)") || !strings.Contains(w[0], "event records end at 2024-06-03 ") || !strings.Contains(w[0], "synced") {
		t.Fatalf("warnings %q", w)
	}
	if strings.Contains(w[0], "r_post") || strings.Contains(w[0], "r_reaction") {
		t.Fatalf("warning names a table: %s", w[0])
	}
}

func TestCollectQuietWhenEventsReachTheCutoffDay(t *testing.T) {
	if w := warnings(collectLog(t, "2024-06-03 12:00:00.000000", demoEvents)); len(w) != 0 {
		t.Fatalf("warnings %q", w)
	}
}

func TestCollectSucceedsWhenTheCheckFails(t *testing.T) {
	w := warnings(collectLog(t, "2024-06-03 12:00:00.000000", `[{"table": "r_post", "time": "missing"}]`))
	if len(w) != 1 || !strings.HasPrefix(w[0], "warning: could not check how far the event tables are filled") {
		t.Fatalf("warnings %q", w)
	}
}
