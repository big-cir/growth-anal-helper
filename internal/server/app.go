// Package server is the web server: workspace state, conversations, saved panels and the HTTP API.
package server

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"growth-lab/internal/agent"
	"growth-lab/internal/auth"
	"growth-lab/internal/collect"
	"growth-lab/internal/collect/ga4"
	"growth-lab/internal/i18n"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/query"
	"growth-lab/internal/sensitive"
	"growth-lab/internal/snapshot"
	"growth-lab/internal/sqlitec"
	"growth-lab/internal/trace"
	"growth-lab/internal/workspace"
)

// Snapshot is the current snapshot set.
type Snapshot struct {
	ID, Real, Agent, AsOf                                    string
	RawHash, DerivedHash, RolesHash, ParamsHash, GA4SpecHash string
}

// AgentStatus: checking, ok, failed or disabled.
type AgentStatus struct{ State, Message string }

// JS is the status as the screen reads it.
func (s AgentStatus) JS() jsjson.Object {
	return jsjson.Object{{Key: "state", Value: s.State}, {Key: "message", Value: s.Message}}
}

// engine bookkeeping tables, in the order the agent sees them refused
var excludedTables = []string{"snapshot_meta", "snapshot_params", "r_collect_log", "r_ga4_collect_log"}

// App is the server state shared by conversations and panels.
type App struct {
	WS     *workspace.Workspace
	Slots  *query.ExecutionSlots
	Runner agent.Runner
	Roles  *collect.Roles
	Lang   string
	Audit  *auth.AuditLog
	Trace  *trace.Log

	mu            sync.Mutex
	agentStatus   AgentStatus
	agentReady    chan struct{}
	excludedSeeds map[string]bool
	ctxCache      *struct{ key, text string }
}

// NewApp loads the workspace definitions and checks what goes into the agent context.
func NewApp(ws *workspace.Workspace) (*App, error) {
	i18n.SetLanguage(ws.Config.Language)
	runner, err := agent.MakeRunner(ws.Config.Agent, ws.Config.OutDir)
	if err != nil {
		return nil, err
	}
	specs, err := collect.LoadSpec(filepath.Join(ws.Dir, "tables.json"))
	if err != nil {
		return nil, err
	}
	derived, err := collect.LoadDerivedRoles(filepath.Join(ws.Dir, "derived-columns.json"))
	if err != nil {
		return nil, err
	}
	roles, err := collect.AllRoles(specs, derived)
	if err != nil {
		return nil, err
	}
	if ws.Config.GA4 != nil {
		plan, err := ga4.LoadPlan(ws.Dir, *ws.Config.GA4)
		if err != nil {
			return nil, err
		}
		for _, k := range ga4.RoleColumns(plan) {
			roles.Set(k, collect.Ordinary)
		}
	}
	ready := make(chan struct{})
	close(ready)
	a := &App{
		WS: ws, Slots: query.NewExecutionSlots(ws.Config.Agent.Concurrency, 1), Runner: runner, Roles: roles, Lang: ws.Config.Language,
		agentStatus: AgentStatus{"checking", i18n.Tr("Checking agent isolation", "에이전트 격리 점검 중")}, agentReady: ready, excludedSeeds: map[string]bool{},
	}
	if err := a.CheckContextAssets(); err != nil {
		return nil, err
	}
	dict, _, err := a.Metrics()
	if err != nil {
		return nil, err
	}
	if snap := a.Snapshot(); snap != nil {
		pol := ws.Config
		_, have, err := agent.SnapshotSchema(snap.Agent, pol.ReadablePrefixes, pol.PanelPrefixes, "")
		if err != nil {
			return nil, err
		}
		var missing []string
		for _, t := range append(append([]string{}, dict.DimensionTables...), metricTables(dict)...) {
			if !contains(have, t) && !contains(missing, t) {
				missing = append(missing, t)
			}
		}
		if len(missing) > 0 {
			return nil, &agent.ContextError{Msg: "metric dictionary tables missing from the snapshot: " + strings.Join(missing, ", ")}
		}
	}
	return a, nil
}

func metricTables(d panels.MetricDict) []string {
	var out []string
	for _, m := range d.Metrics {
		out = append(out, m.Tables...)
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// SeedFile is a seed panel file as read.
type SeedFile struct{ Name, Text string }

// LoadSeedPanels reads seed-panels/*.json in name order.
func LoadSeedPanels(wsDir string) ([]agent.SeedPanel, []SeedFile, error) {
	dir := filepath.Join(wsDir, "seed-panels")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var seeds []agent.SeedPanel
	var files []SeedFile
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, nil, err
		}
		files = append(files, SeedFile{n, string(b)})
		raw, err := jsjson.Parse(string(b))
		if err != nil {
			return nil, nil, fmt.Errorf("seed-panels/%s: %w", n, err)
		}
		spec, err := panels.ParseSpec(raw)
		if err != nil {
			return nil, nil, err
		}
		id := strings.TrimSuffix(n, ".json")
		if o, ok := raw.(jsjson.Object); ok {
			if v, ok := o.Get("id"); ok {
				if s, ok := v.(string); ok {
					id = s
				}
			}
		}
		seeds = append(seeds, agent.SeedPanel{ID: id, Spec: spec})
	}
	return seeds, files, nil
}

// Snapshot is the set current.json points at, or nil.
func (a *App) Snapshot() *Snapshot {
	cur, err := snapshot.ReadCurrent(snapshot.Dir(a.WS.Config.OutDir))
	if err != nil || cur == nil {
		return nil
	}
	if _, err := os.Stat(cur.File); err != nil {
		return nil
	}
	if _, err := os.Stat(cur.AgentFile); err != nil {
		return nil
	}
	db, err := sqlitec.Open(cur.File, true)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, _, err := db.QueryJS("SELECT snapshot_id, source_cutoff_at, raw_hash, derived_hash, roles_hash, params_hash, ga4_spec_hash FROM snapshot_meta")
	if err != nil || len(rows) == 0 {
		return nil
	}
	s := func(v any) string { x, _ := v.(string); return x }
	r := rows[0]
	return &Snapshot{ID: s(r[0]), Real: cur.File, Agent: cur.AgentFile, AsOf: s(r[1]), RawHash: s(r[2]), DerivedHash: s(r[3]), RolesHash: s(r[4]), ParamsHash: s(r[5]), GA4SpecHash: s(r[6])}
}

// Metrics is the metric dictionary, validated on every read.
func (a *App) Metrics() (panels.MetricDict, string, error) {
	seeds, _, err := LoadSeedPanels(a.WS.Dir)
	if err != nil {
		return panels.MetricDict{}, "", err
	}
	ss := make([]panels.Seed, len(seeds))
	for i, s := range seeds {
		ss[i] = panels.Seed{ID: s.ID, Metric: s.Spec.Metric}
	}
	return panels.LoadMetrics(a.WS.Dir, a.WS.Config.PanelPrefixes, ss)
}

// Guide is guide.md ("" when missing).
func (a *App) Guide() string {
	b, _ := os.ReadFile(filepath.Join(a.WS.Dir, "guide.md"))
	return string(b)
}

// Blocked are the private columns and the engine bookkeeping tables refused in agent queries.
func (a *App) Blocked() query.BlockedList {
	var cols []string
	for _, k := range a.Roles.Keys() {
		if r, _ := a.Roles.Get(k); r == collect.Private {
			cols = append(cols, k)
		}
	}
	return query.BlockedList{Columns: cols, Tables: append([]string{}, excludedTables...)}
}

// CheckContextAssets refuses guide, metric dictionary or seed panels that mention private columns or engine tables, or hold secrets.
func (a *App) CheckContextAssets() error {
	files := []SeedFile{{"guide.md", a.Guide()}}
	if b, err := os.ReadFile(filepath.Join(a.WS.Dir, "metrics.json")); err == nil {
		files = append(files, SeedFile{"metrics.json", string(b)})
	}
	_, seeds, err := LoadSeedPanels(a.WS.Dir)
	if err != nil {
		return err
	}
	for _, f := range seeds {
		files = append(files, SeedFile{"seed-panels/" + f.Name, f.Text})
	}
	private := a.Blocked().Columns
	public := map[string]bool{}
	for _, k := range a.Roles.Keys() {
		if r, _ := a.Roles.Get(k); r != collect.Private {
			public[strings.SplitN(k, ".", 2)[1]] = true
		}
	}
	needles := append([]string{}, private...)
	for _, c := range private {
		if n := strings.SplitN(c, ".", 2)[1]; !public[n] {
			needles = append(needles, n)
		}
	}
	var problems []string
	for _, f := range files {
		for _, c := range needles {
			re := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(c) + `($|[^A-Za-z0-9_])`)
			if re.MatchString(f.Text) {
				problems = append(problems, f.Name+": private column "+c)
				break
			}
		}
		for _, t := range excludedTables {
			if regexp.MustCompile(`\b` + t + `\b`).MatchString(f.Text) {
				problems = append(problems, f.Name+": engine table "+t)
				break
			}
		}
		if shape := sensitive.SecretShape(f.Text); shape != "" {
			problems = append(problems, f.Name+": secret-looking value ("+shape+")")
		}
		if sensitive.SecretAssignment(f.Text) {
			problems = append(problems, f.Name+": secret assignment")
		}
	}
	if len(problems) > 0 {
		return &agent.ContextError{Msg: "content that cannot be given to the agent — " + strings.Join(problems, " / ")}
	}
	return nil
}

// ContextVersion identifies everything the agent context depends on.
func (a *App) ContextVersion(snap *Snapshot) panels.ContextVersion {
	derived, _ := os.ReadFile(filepath.Join(a.WS.Dir, "derived.sql"))
	_, files, _ := LoadSeedPanels(a.WS.Dir)
	docs := make([]panels.SeedDoc, len(files))
	for i, f := range files {
		docs[i] = panels.SeedDoc{Name: f.Name, Text: f.Text}
	}
	_, metricsText, _ := a.Metrics()
	return panels.ContextVersion{
		SnapshotID:    snap.ID,
		SchemaVersion: panels.SchemaVersion(string(derived), snap.RolesHash+":"+snap.GA4SpecHash),
		PolicyVersion: panels.PolicyVersion(a.WS.Config.ReadablePrefixes, a.WS.Config.PanelPrefixes),
		DocsVersion:   panels.DocsVersion(a.Guide(), docs, metricsText),
		PromptVersion: panels.PromptVersion(agent.EngineGuide(a.Lang), agent.ActionSchemaText),
	}
}

// ContextJS is a context version as stored in previews and panels.
func ContextJS(v panels.ContextVersion) jsjson.Object {
	return jsjson.Object{{Key: "snapshot_id", Value: v.SnapshotID}, {Key: "schema_version", Value: v.SchemaVersion}, {Key: "policy_version", Value: v.PolicyVersion},
		{Key: "docs_version", Value: v.DocsVersion}, {Key: "prompt_version", Value: v.PromptVersion}}
}

// PanelVersions are the versions a saved panel's result depends on.
func (a *App) PanelVersions(snap *Snapshot) jsjson.Object {
	return append(ContextJS(a.ContextVersion(snap)), jsjson.Member{Key: "pattern_contract_version", Value: float64(panels.PatternContractVersion)}, jsjson.Member{Key: "renderer_version", Value: float64(panels.RendererVersion)})
}

// SystemPrompt builds the agent context; blind leaves out seed panel SQL.
func (a *App) SystemPrompt(snap *Snapshot, ctx panels.ContextVersion, blind bool) (string, error) {
	a.mu.Lock()
	var excluded []string
	for k := range a.excludedSeeds {
		excluded = append(excluded, k)
	}
	a.mu.Unlock()
	sort.Strings(excluded)
	key := fmt.Sprintf("%s|%s|%v", ctx.Key(), strings.Join(excluded, ","), blind)
	a.mu.Lock()
	if a.ctxCache != nil && a.ctxCache.key == key {
		t := a.ctxCache.text
		a.mu.Unlock()
		return t, nil
	}
	a.mu.Unlock()
	guide := a.Guide()
	cfg := a.WS.Config
	dict, _, err := a.Metrics()
	if err != nil {
		return "", err
	}
	schemaText, have, err := agent.SnapshotSchema(snap.Agent, cfg.ReadablePrefixes, cfg.PanelPrefixes, guide)
	if err != nil {
		return "", err
	}
	var missing []string
	for _, t := range append(append([]string{}, dict.DimensionTables...), metricTables(dict)...) {
		if !contains(have, t) && !contains(missing, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return "", &agent.ContextError{Msg: "metric dictionary tables missing from the snapshot: " + strings.Join(missing, ", ")}
	}
	seeds, _, err := LoadSeedPanels(a.WS.Dir)
	if err != nil {
		return "", err
	}
	var kept []agent.SeedPanel
	for _, s := range seeds {
		if !contains(excluded, s.ID) {
			kept = append(kept, s)
		}
	}
	calendar := ""
	if v, ok := cfg.Params.Get("calendar_start"); ok {
		calendar, _ = v.(string)
	}
	now := time.Now()
	text, err := agent.BuildContext(agent.ContextInput{
		Schema: schemaText, Metrics: panels.MetricsContext(dict), Guide: guide, SeedPanels: kept, Lang: a.Lang,
		State: agent.ContextState{AsOf: snap.AsOf, Today: fmt.Sprintf("%04d-%02d-%02d", now.Year(), now.Month(), now.Day()), CalendarStart: calendar, Params: cfg.Params},
	}, !blind)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.ctxCache = &struct{ key, text string }{key, text}
	a.mu.Unlock()
	return text, nil
}

// SetExcludedSeeds leaves failed seed panels out of the context.
func (a *App) SetExcludedSeeds(ids map[string]bool) {
	a.mu.Lock()
	a.excludedSeeds = ids
	a.mu.Unlock()
}

func model(cfg workspace.Agent) string {
	if cfg.Model == nil {
		return ""
	}
	return *cfg.Model
}

// Summarize writes the long description of a panel (only pseudonymized results are sent).
func (a *App) Summarize(spec panels.Spec, columns []panels.Column, agentRows []panels.Row) agent.SummaryOutcome {
	a.WaitAgent()
	st := a.AgentStatus()
	if st.State != "ok" {
		return agent.SummaryOutcome{Message: st.Message}
	}
	cfg := a.WS.Config.Agent
	out, err := agent.Summarize(agent.SummarizeInput{
		Spec: spec, Columns: columns, Rows: agentRows, Outbound: &agent.Outbound{Mode: cfg.DataMode, Roles: a.Roles}, Lang: a.Lang,
		Call: func(input, sessionID string) agent.CallResult {
			var res agent.CallResult
			err := a.Slots.Run(context.Background(), query.Interactive, func(*query.SlotLease) error {
				res = a.Runner.Call(agent.CallOptions{Input: input, SessionID: sessionID, SystemPrompt: agent.SummaryGuide(a.Lang), JSONSchema: agent.SummarySchemaArg,
					BudgetUsd: cfg.CallBudgetUsd, Model: model(cfg), TimeoutMs: cfg.CallTimeoutMs, Ctx: context.Background()})
				return nil
			})
			if err != nil {
				return agent.CallResult{Type: "cancelled", Message: i18n.Tr("Cancelled", "취소됨")}
			}
			if !res.OK && res.Type == "isolation" {
				a.DisableAgent(i18n.Tr("Agent isolation check failed: the agent is unavailable until the server restarts", "에이전트 격리 점검 실패: 서버를 다시 시작하기 전까지 에이전트를 쓸 수 없어요"))
			}
			return res
		},
	})
	if err != nil {
		return agent.SummaryOutcome{Message: err.Error()}
	}
	return out
}

// StartIsolationCheck calls the agent once at start; requests wait for it.
func (a *App) StartIsolationCheck() {
	ready := make(chan struct{})
	a.mu.Lock()
	a.agentReady = ready
	a.mu.Unlock()
	go func() {
		defer close(ready)
		cfg := a.WS.Config.Agent
		var r agent.CallResult
		_ = a.Slots.Run(context.Background(), query.Interactive, func(*query.SlotLease) error {
			r = a.Runner.Call(agent.CallOptions{
				Input:        "This is an isolation check. Reply with the refuse action, reason \"ok\" and an empty alternatives array.",
				SystemPrompt: "Isolation check call.", JSONSchema: agent.ActionSchemaArg, BudgetUsd: math.Min(0.2, cfg.CallBudgetUsd),
				Model: model(cfg), TimeoutMs: cfg.CallTimeoutMs, Ctx: context.Background(),
			})
			return nil
		})
		var st AgentStatus
		switch {
		case r.OK && cfg.Provider == "claude-code":
			st = AgentStatus{"ok", i18n.Tr("Agent isolation check passed", "에이전트 격리 점검 통과")}
		case r.OK:
			st = AgentStatus{"ok", i18n.Tr("API connection check passed ("+cfg.Provider+")", "API 연결 점검 통과 ("+cfg.Provider+")")}
		case r.Type == "isolation":
			st = AgentStatus{"failed", r.Message}
		default:
			st = AgentStatus{"failed", i18n.Tr("Could not start the agent: "+r.Message, "에이전트를 시작하지 못함: "+r.Message)}
		}
		a.mu.Lock()
		a.agentStatus = st
		a.mu.Unlock()
	}()
}

// WaitAgent waits for the isolation check.
func (a *App) WaitAgent() {
	a.mu.Lock()
	ch := a.agentReady
	a.mu.Unlock()
	<-ch
}

// AgentStatus is the current agent status.
func (a *App) AgentStatus() AgentStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.agentStatus
}

// DisableAgent turns the agent off until restart.
func (a *App) DisableAgent(message string) {
	a.mu.Lock()
	a.agentStatus = AgentStatus{"disabled", message}
	a.mu.Unlock()
}
