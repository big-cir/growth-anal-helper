package workspace_test

import (
	"strings"
	"testing"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/workspace"
)

func parseRun(t *testing.T, run string) (workspace.Config, error) {
	t.Helper()
	text := `{"name":"app","datasource":{"host":"sqlite://source.sqlite"},"policy":{"readablePrefixes":["r_","d_"]}`
	if run != "" {
		text += `,"run":` + run
	}
	raw, err := jsjson.Parse(text + "}")
	if err != nil {
		t.Fatal(err)
	}
	return workspace.ParseConfig(raw, "/ws/demo", "/hh/u")
}

func TestQueryTimeoutSettings(t *testing.T) {
	cfg, err := parseRun(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueryTimeoutMs != 60000 || cfg.BackgroundQueryTimeoutMs != 300000 {
		t.Fatalf("defaults: %d %d", cfg.QueryTimeoutMs, cfg.BackgroundQueryTimeoutMs)
	}
	if run, _ := cfg.JS().Get("run"); jsjson.MustStringify(run) != `{"heapLimitMb":2048}` {
		t.Errorf("defaults are written: %s", jsjson.MustStringify(run))
	}
	cfg, err = parseRun(t, `{"queryTimeoutMs":1500,"backgroundQueryTimeoutMs":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueryTimeoutMs != 1500 || cfg.BackgroundQueryTimeoutMs != 1 {
		t.Fatalf("values: %d %d", cfg.QueryTimeoutMs, cfg.BackgroundQueryTimeoutMs)
	}
	run, _ := cfg.JS().Get("run")
	if got := jsjson.MustStringify(run); got != `{"heapLimitMb":2048,"queryTimeoutMs":1500,"backgroundQueryTimeoutMs":1}` {
		t.Errorf("stored run: %s", got)
	}
	for run, want := range map[string]string{
		`{"queryTimeoutMs":0}`:            "workspace.json .run.queryTimeoutMs: must be >= 1",
		`{"backgroundQueryTimeoutMs":-5}`: "workspace.json .run.backgroundQueryTimeoutMs: must be >= 1",
		`{"queryTimeoutMs":1.5}`:          "workspace.json .run.queryTimeoutMs: must be an integer",
		`{"queryTimeout":1000}`:           "workspace.json .run",
	} {
		if _, err := parseRun(t, run); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v", run, err)
		}
	}
}
