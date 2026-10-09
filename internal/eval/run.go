package eval

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"growth-lab/internal/agent"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/server"
	"growth-lab/internal/trace"
	"growth-lab/internal/workspace"
)

// Options are the eval run options; zero values mean defaults.
type Options struct {
	Blind  bool
	Runs   *float64
	CaseID string
	Tag    string
	// overrides agent.model for this run only
	Model      string
	MaxCostUSD *float64
	Seed       *float64
	Ctx        context.Context
	Log        func(string)
}

const maxAutoAnswers = 2

func toUint32(f float64) uint32 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	m := math.Mod(math.Trunc(f), 4294967296)
	if m < 0 {
		m += 4294967296
	}
	return uint32(m)
}

// Shuffle is the deterministic mulberry32 shuffle, so an order can be reproduced with --seed.
func Shuffle[T any](items []T, seed float64) []T {
	a := toUint32(seed)
	next := func() float64 {
		a += 0x6d2b79f5
		t := a
		t = (t ^ t>>15) * (t | 1)
		t ^= t + (t^t>>7)*(t|61)
		return float64(t^t>>14) / 4294967296
	}
	out := append([]T(nil), items...)
	for i := len(out) - 1; i > 0; i-- {
		j := int(math.Floor(next() * float64(i+1)))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// MetricIDs are the ids of the current metric dictionary.
func MetricIDs(a *server.App) (map[string]bool, error) {
	dict, _, err := a.Metrics()
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, m := range dict.Metrics {
		ids[m.ID] = true
	}
	return ids, nil
}

// NewApp is the app for an eval: same workspace settings, optionally another model.
func NewApp(ws *workspace.Workspace, model string) (*server.App, error) {
	if model == "" {
		return server.NewApp(ws)
	}
	cp := *ws
	cp.Config.Agent.Model = &model
	return server.NewApp(&cp)
}

func runOne(ctx context.Context, a *server.App, snap *server.Snapshot, systemPrompt string, c *Case, run int) (RunRecord, error) {
	t0 := time.Now()
	rec := RunRecord{CaseID: c.ID, Run: run, Status: "failed", Steps: []jsjson.Object{}}
	if server.IsSensitiveInput(c.Question, nil) {
		rec.Status = "blocked_preflight"
		return rec, nil
	}
	var mu sync.Mutex
	acted := false
	onEvent := func(ev agent.Event) {
		mu.Lock()
		defer mu.Unlock()
		switch ev.Type {
		case "plan":
			acted = true
		case "question":
			if !acted && rec.Asks == 0 {
				rec.AskedFirst = true
			}
			if rec.AskQuestions == nil {
				rec.AskQuestions = []string{}
			}
			for _, q := range ev.Questions {
				rec.AskQuestions = append(rec.AskQuestions, q.Text)
			}
		case "preview":
			spec := ev.Spec
			rec.Status, rec.Spec, rec.Columns, rec.Rows = "preview", &spec, ev.Columns, ev.RealRows
		case "refused":
			rec.Status = "refused"
			rec.RefuseReason = ptr(ev.Reason)
		case "failed":
			rec.Status = "failed"
			rec.FailReason, rec.FailMessage = ptr(ev.Reason), ptr(ev.Message)
		case "cancelled":
			if rec.Status != "question" {
				rec.Status = "cancelled"
			}
		}
	}
	cfg := a.WS.Config
	id := fmt.Sprintf("eval%02d%s", run, c.ID)
	if len(id) > 40 {
		id = id[:40]
	}
	tracer := trace.NewTracer(id, nil, cfg.Agent.Provider, cfg.Agent.Model, func(s trace.Span) {
		mu.Lock()
		defer mu.Unlock()
		if s.Span == "panel_run" {
			rec.PanelsRun++
		}
		rec.Steps = append(rec.Steps, SpanJS(s))
	}, nil)
	dict, _, err := a.Metrics()
	if err != nil {
		return rec, err
	}
	deps := server.MakeLoopDeps(a, server.LoopInput{Snap: snap, SystemPrompt: systemPrompt, Metrics: dict, Outbound: &agent.Outbound{Mode: cfg.Agent.DataMode, Roles: a.Roles},
		Emit: func(ev agent.Event, _ int) { onEvent(ev) }, Tracer: tracer})
	req := agent.NewRequest(id, deps)
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			req.Cancel()
		case <-stopped:
		}
	}()
	req.Run(agent.StartInput{Text: c.Question})
	for ctx.Err() == nil && req.State() == "waiting_user" {
		if _, turn, ok := req.PendingAsk(); ok {
			mu.Lock()
			rec.Asks++
			asks := rec.Asks
			if c.Expect.Action == "ask" {
				rec.Status = "question"
			}
			mu.Unlock()
			if c.Expect.Action == "ask" {
				req.Cancel()
				break
			}
			if asks > maxAutoAnswers {
				rec.AskLoop = true
				req.Cancel()
				break
			}
			// empty answers: the engine uses each question's default option
			req.Answer(turn, map[string]string{})
			continue
		}
		if _, _, _, _, turn, ok := req.PendingOffdict(); ok {
			approve := c.Expect.Action == "panel" && c.Expect.Metric == nil
			if !approve {
				rec.OffdictAttempts++
			}
			req.ApproveOffdict(turn, approve)
			continue
		}
		break
	}
	if ctx.Err() != nil && !req.Finished() {
		req.Cancel()
	}
	mu.Lock()
	defer mu.Unlock()
	rec.Turns, rec.Probes, rec.Fixes, rec.Cost = req.Turns, req.Probes, req.Fixes, req.CostTotal
	rec.Ms = float64(time.Since(t0).Milliseconds())
	return rec, nil
}

// gitCommit is the engine's commit, when it runs from a git checkout.
func gitCommit() *string {
	dirs := []string{}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, d := range dirs {
		cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
		cmd.Dir = d
		if out, err := cmd.Output(); err == nil {
			s := strings.TrimSpace(string(out))
			return &s
		}
	}
	return nil
}

// ReportsDir is <outDir>/eval/reports.
func ReportsDir(a *server.App) string { return filepath.Join(OutDir(a), "reports") }

// Run runs the selected cases and saves the report.
func Run(ws *workspace.Workspace, o Options) (Report, string, error) {
	log := o.Log
	if log == nil {
		log = func(string) {}
	}
	ctx := o.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	runs := 2
	if o.Runs != nil {
		if *o.Runs != math.Trunc(*o.Runs) || *o.Runs < 1 {
			return Report{}, "", &Error{"--runs must be a positive integer"}
		}
		runs = int(*o.Runs)
	}
	a, err := NewApp(ws, o.Model)
	if err != nil {
		return Report{}, "", err
	}
	snap, env, err := CheckEnv(a)
	if err != nil {
		return Report{}, "", err
	}
	ids, err := MetricIDs(a)
	if err != nil {
		return Report{}, "", err
	}
	all, problems, err := LoadCases(ws.Dir, ids)
	if err != nil {
		return Report{}, "", err
	}
	if len(problems) > 0 {
		return Report{}, "", &Error{"case problems (run eval check):\n" + strings.Join(problems, "\n")}
	}
	var cases []*Case
	for _, c := range all {
		if (o.CaseID == "" || c.ID == o.CaseID) && (o.Tag == "" || contains(c.Tags, o.Tag)) {
			cases = append(cases, c)
		}
	}
	if len(cases) == 0 {
		return Report{}, "", &Error{"no cases match"}
	}
	fixtures := map[string]*Fixture{}
	var stale []StaleCase
	var ready []*Case
	for _, c := range cases {
		if c.Reference != nil {
			f, err := ReadFixture(a, snap.ID, c.ID)
			if err != nil {
				return Report{}, "", err
			}
			if why := StaleReason(c, f, env); why != "" {
				stale = append(stale, StaleCase{c.ID, why})
				continue
			}
			fixtures[c.ID] = f
		}
		ready = append(ready, c)
	}

	a.StartIsolationCheck()
	a.WaitAgent()
	if st := a.AgentStatus(); st.State != "ok" {
		return Report{}, "", &Error{"agent unavailable: " + st.Message}
	}
	mode := "full"
	if o.Blind {
		mode = "blind"
	}
	cv := a.ContextVersion(snap)
	systemPrompt, err := a.SystemPrompt(snap, cv, o.Blind)
	if err != nil {
		return Report{}, "", err
	}

	seed := math.Floor(rand.Float64() * 2147483648)
	if o.Seed != nil {
		seed = *o.Seed
	}
	var items []OrderItem
	for _, c := range ready {
		for i := 1; i <= runs; i++ {
			items = append(items, OrderItem{c.ID, i})
		}
	}
	order := Shuffle(items, seed)
	byID := map[string]*Case{}
	graded := map[string][]GradedRun{}
	for _, c := range ready {
		byID[c.ID] = c
	}
	cost := 0.0
	aborted := ""
	done := []OrderItem{}
	for _, item := range order {
		if ctx.Err() != nil {
			aborted = "interrupted"
			break
		}
		if o.MaxCostUSD != nil && cost >= *o.MaxCostUSD {
			aborted = "budget"
			break
		}
		c := byID[item.CaseID]
		rec, err := runOne(ctx, a, snap, systemPrompt, c, item.Run)
		if err != nil {
			return Report{}, "", err
		}
		if ctx.Err() != nil {
			aborted = "interrupted"
			break
		}
		cost += rec.Cost
		g, err := GradeRun(c, fixtures[c.ID], rec)
		if err != nil {
			return Report{}, "", err
		}
		graded[c.ID] = append(graded[c.ID], g)
		done = append(done, item)
		codes := ""
		if len(g.Codes) > 0 {
			codes = " (" + strings.Join(g.Codes, ", ") + ")"
		}
		log(fmt.Sprintf("%s#%d: %s%s %ds", c.ID, item.Run, g.Verdict, codes, round(rec.Ms/1000)))
	}

	caseReports := []CaseReport{}
	for _, c := range ready {
		gs := graded[c.ID]
		if len(gs) == 0 {
			continue
		}
		sortRuns(gs)
		caseReports = append(caseReports, BuildCaseReport(c, gs))
	}
	cfg := a.WS.Config.Agent
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	rep := Report{
		Meta: Meta{CreatedAt: created, Workspace: ws.Config.Name, Provider: cfg.Provider, Model: cfg.Model, ContextMode: mode, ContextVersion: server.ContextJS(cv), SnapshotID: snap.ID, Env: env,
			Runs: runs, Seed: seed, Order: done, GitCommit: gitCommit(), MaxCostUSD: o.MaxCostUSD, CostUSD: cost, Aborted: aborted, Stale: stale},
		Cases:   caseReports,
		Summary: Summarize(caseReports, cost),
	}
	dir := ReportsDir(a)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return Report{}, "", err
	}
	stamp := strings.NewReplacer("-", "", ":", "").Replace(created)
	stamp = strings.Replace(stamp, "T", "-", 1)[:15]
	path := filepath.Join(dir, stamp+".json")
	for i := 2; exists(path); i++ {
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.json", stamp, i))
	}
	if err := writeAtomic(path, jsjson.Indent(rep.JS(), 2)+"\n"); err != nil {
		return Report{}, "", err
	}
	return rep, path, nil
}

func sortRuns(gs []GradedRun) {
	for i := 1; i < len(gs); i++ {
		for j := i; j > 0 && gs[j].Run < gs[j-1].Run; j-- {
			gs[j], gs[j-1] = gs[j-1], gs[j]
		}
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
