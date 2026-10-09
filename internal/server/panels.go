package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"growth-lab/internal/collect"
	"growth-lab/internal/i18n"
	"growth-lab/internal/jsjson"
	"growth-lab/internal/panels"
	"growth-lab/internal/quality"
	"growth-lab/internal/query"
	"growth-lab/internal/snapshot"
)

// Conflict is a 409 error.
type Conflict struct{ Msg string }

func (e *Conflict) Error() string { return e.Msg }

// NotFound is a 404 error.
type NotFound struct{ Msg string }

func (e *NotFound) Error() string { return e.Msg }

// PanelEvent is a dashboard event.
type PanelEvent struct {
	Type string
	Data jsjson.Object
}

type job struct {
	kind   string // panel or quality
	id     string
	mode   string // auto or manual_rule
	ctx    context.Context
	cancel context.CancelFunc
}

// PanelService saves and deletes panels, recomputes them in the background, runs quality checks and sends dashboard events.
type PanelService struct {
	Store *panels.Store
	app   *App

	mu            sync.Mutex
	jobs          map[string]string
	busy          map[string]bool
	queue         []*job
	running       *job
	subs          map[*panelSub]bool
	lastSnapshot  string
	quality       jsjson.Object
	qualityTarget string
	stopped       bool
	stop          chan struct{}
	wake          chan struct{}
	idle          chan struct{}
}

type panelSub struct {
	mu     sync.Mutex
	queue  []PanelEvent
	notify chan struct{}
}

func (s *panelSub) push(e PanelEvent) {
	s.mu.Lock()
	s.queue = append(s.queue, e)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *panelSub) take() []PanelEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queue
	s.queue = nil
	return q
}

func qualityState(status string, snapshotID, computedAt any, items []any, errMsg any) jsjson.Object {
	if items == nil {
		items = []any{}
	}
	return jsjson.Object{{Key: "status", Value: status}, {Key: "snapshot_id", Value: snapshotID}, {Key: "computed_at", Value: computedAt}, {Key: "items", Value: items}, {Key: "error", Value: errMsg}}
}

// NewPanelService opens the panel store.
func NewPanelService(app *App) (*PanelService, error) {
	st, err := panels.NewStore(filepath.Join(app.WS.Config.OutDir, "panels"))
	if err != nil {
		return nil, err
	}
	return &PanelService{Store: st, app: app, jobs: map[string]string{}, busy: map[string]bool{}, subs: map[*panelSub]bool{},
		quality: qualityState("idle", nil, nil, nil, nil), stop: make(chan struct{}), wake: make(chan struct{}, 1), idle: make(chan struct{})}, nil
}

// Start decides recomputes, queues quality checks and watches for snapshot changes.
func (s *PanelService) Start(poll time.Duration) {
	if snap := s.app.Snapshot(); snap != nil {
		s.lastSnapshot = snap.ID
	}
	go s.worker()
	s.Scan()
	go func() {
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.poll()
			}
		}
	}()
}

// Stop cancels queued and running jobs and waits for the worker.
func (s *PanelService) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	for _, j := range s.queue {
		j.cancel()
	}
	s.queue = nil
	if s.running != nil {
		s.running.cancel()
	}
	close(s.stop)
	s.mu.Unlock()
	<-s.idle
}

// Subscribe registers a dashboard listener.
func (s *PanelService) Subscribe() (*panelSub, func()) {
	sub := &panelSub{notify: make(chan struct{}, 1)}
	s.mu.Lock()
	s.subs[sub] = true
	s.mu.Unlock()
	return sub, func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
	}
}

// emit must be called with s.mu held.
func (s *PanelService) emit(typ string, data jsjson.Object) {
	for sub := range s.subs {
		sub.push(PanelEvent{typ, data})
	}
}

// JobStatus is idle, queued, running, cancelling or cancelled.
func (s *PanelService) JobStatus(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobStatus(id)
}

func (s *PanelService) jobStatus(id string) string {
	if st, ok := s.jobs[id]; ok {
		return st
	}
	return "idle"
}

// setJob must be called with s.mu held.
func (s *PanelService) setJob(id, st string) {
	s.jobs[id] = st
	if p, _ := s.Store.Get(id); p != nil {
		s.emit("panel_status", s.statusOf(*p))
	}
}

func (s *PanelService) statusOf(p panels.SavedPanel) jsjson.Object {
	return jsjson.Object{{Key: "panel_id", Value: p.Str("id")}, {Key: "status", Value: p.Str("status")}, {Key: "job_status", Value: s.jobStatus(p.Str("id"))}}
}

// AllStatus is every panel's status.
func (s *PanelService) AllStatus() []any {
	list, _ := s.Store.List()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []any{}
	for _, p := range list {
		out = append(out, s.statusOf(*p))
	}
	return out
}

func (s *PanelService) poll() {
	id := ""
	if cur, _ := snapshot.ReadCurrent(snapshot.Dir(s.app.WS.Config.OutDir)); cur != nil {
		id = cur.SnapshotID
	}
	s.mu.Lock()
	if id == s.lastSnapshot {
		s.mu.Unlock()
		return
	}
	s.lastSnapshot = id
	s.emit("snapshot_changed", jsjson.Object{})
	s.mu.Unlock()
	s.Scan()
}

// Scan decides recompute for every saved panel and queues it; quality checks go last.
func (s *PanelService) Scan() {
	snap := s.app.Snapshot()
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap == nil || s.stopped {
		return
	}
	current := s.app.PanelVersions(snap)
	list, _ := s.Store.List()
	for _, p := range list {
		var d string
		if p.Legacy {
			d = "review"
			if p.Str("status") == "review" {
				d = "none"
			}
		} else {
			d = panels.Decide(*p, current)
		}
		switch {
		case d == "review":
			p.Set("status", "review")
			_ = s.Store.Write(*p)
			s.emit("panel_status", s.statusOf(*p))
		case d == "auto":
			s.enqueuePanel(p.Str("id"), "auto", false)
		default:
			v := p.Obj("versions")
			rv, _ := v.Get("renderer_version")
			cur, _ := current.Get("renderer_version")
			if !sameNumber(rv, cur) && len(panels.ChangedRules(v, current)) == 0 {
				next := append(jsjson.Object{}, v...)
				replaced := false
				for i := range next {
					if next[i].Key == "renderer_version" {
						next[i].Value = cur
						replaced = true
					}
				}
				if !replaced {
					next = append(next, jsjson.Member{Key: "renderer_version", Value: cur})
				}
				p.Set("versions", next)
				_ = s.Store.Write(*p)
			}
		}
	}
	if s.qualityTarget != snap.ID {
		s.enqueueQuality(snap.ID)
	}
}

func sameNumber(a, b any) bool {
	x, ok1 := toF(a)
	y, ok2 := toF(b)
	return ok1 && ok2 && x == y
}

func toF(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

func (s *PanelService) newJob(kind, id, mode string) *job {
	ctx, cancel := context.WithCancel(context.Background())
	return &job{kind: kind, id: id, mode: mode, ctx: ctx, cancel: cancel}
}

// enqueuePanel must be called with s.mu held.
func (s *PanelService) enqueuePanel(id, mode string, front bool) {
	st := s.jobStatus(id)
	if st == "queued" || st == "running" || st == "cancelling" {
		return
	}
	j := s.newJob("panel", id, mode)
	if front {
		s.queue = append([]*job{j}, s.queue...)
	} else {
		s.queue = append(s.queue, j)
	}
	s.setJob(id, "queued")
	s.kick()
}

// enqueueQuality must be called with s.mu held.
func (s *PanelService) enqueueQuality(snapshotID string) {
	s.qualityTarget = snapshotID
	for _, j := range s.queue {
		if j.kind == "quality" {
			return
		}
	}
	s.queue = append(s.queue, s.newJob("quality", "", ""))
	if st, _ := s.quality.Get("status"); st != "running" {
		s.quality = setField(s.quality, "status", "queued")
		sid, _ := s.quality.Get("snapshot_id")
		s.emit("quality_status", jsjson.Object{{Key: "status", Value: "queued"}, {Key: "snapshot_id", Value: sid}})
	}
	s.kick()
}

func (s *PanelService) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func setField(o jsjson.Object, k string, v any) jsjson.Object {
	out := append(jsjson.Object{}, o...)
	for i := range out {
		if out[i].Key == k {
			out[i].Value = v
			return out
		}
	}
	return append(out, jsjson.Member{Key: k, Value: v})
}

// worker runs one job at a time; panel recomputes before quality checks.
func (s *PanelService) worker() {
	defer close(s.idle)
	for {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		idx := -1
		for i, j := range s.queue {
			if j.kind == "panel" {
				idx = i
				break
			}
		}
		if idx < 0 && len(s.queue) > 0 {
			idx = 0
		}
		if idx < 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
			case <-s.stop:
				return
			}
			continue
		}
		j := s.queue[idx]
		s.queue = append(s.queue[:idx:idx], s.queue[idx+1:]...)
		s.running = j
		s.mu.Unlock()
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panel job failed", "panic", r, "kind", j.kind, "panel", j.id)
				}
			}()
			if j.kind == "panel" {
				s.runPanelJob(j)
			} else {
				s.runQuality(j.ctx)
			}
		}()
		s.mu.Lock()
		s.running = nil
		s.mu.Unlock()
	}
}

// Cancel removes a queued recompute, or stops a running one and waits.
func (s *PanelService) Cancel(id string) (string, error) {
	s.mu.Lock()
	st := s.jobStatus(id)
	if st == "queued" {
		for i, j := range s.queue {
			if j.kind == "panel" && j.id == id {
				s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
				break
			}
		}
		s.setJob(id, "cancelled")
		s.mu.Unlock()
		return "cancelled", nil
	}
	if st == "running" && s.running != nil && s.running.kind == "panel" && s.running.id == id {
		j := s.running
		s.setJob(id, "cancelling")
		j.cancel()
		s.mu.Unlock()
		for {
			s.mu.Lock()
			still := s.running == j
			out := s.jobStatus(id)
			s.mu.Unlock()
			if !still {
				return out, nil
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	s.mu.Unlock()
	return "", &Conflict{i18n.Tr("No recompute is running or queued", "진행 중이거나 대기 중인 재계산이 없어요")}
}

func (s *PanelService) runOn(spec panels.Spec, real, agentPath, asOf string, kind query.SlotKind, ctx context.Context) panels.RunResult {
	cfg := s.app.WS.Config
	dict, _, err := s.app.Metrics()
	if err != nil {
		return panels.RunResult{Stage: "exec", Message: err.Error()}
	}
	var r panels.RunResult
	err = s.app.Slots.Run(ctx, kind, func(l *query.SlotLease) error {
		r = panels.Run(panels.RunInput{Spec: spec, RealPath: real, AgentPath: agentPath, AsOf: asOf, Params: cfg.Params, PanelPrefix: cfg.PanelPrefixes,
			Metrics: dict, Blocked: s.app.Blocked(), HeapLimitMb: float64(cfg.HeapLimitMb), Roles: s.app.Roles, Lease: l, Ctx: ctx})
		return nil
	})
	if err != nil {
		return panels.RunResult{Stage: "cancelled", Message: i18n.Tr("Cancelled", "취소됨")}
	}
	return r
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// MakeResult is the stored result of a panel run.
func MakeResult(spec panels.Spec, snapshotID, asOf, mode string, columns []panels.Column, rows []panels.Row, tables []string, lang string) jsjson.Object {
	caveats, _ := panels.EffectiveCaveats(spec, columns, rows, lang)
	if caveats == nil {
		caveats = []string{}
	}
	if tables == nil {
		tables = []string{}
	}
	return jsjson.Object{{Key: "snapshot_id", Value: snapshotID}, {Key: "as_of", Value: asOf}, {Key: "computed_at", Value: nowISO()}, {Key: "mode", Value: mode},
		{Key: "columns", Value: columnsJS(columns)}, {Key: "rows", Value: rowsJS(rows)}, {Key: "tables", Value: tables}, {Key: "caveats", Value: caveats},
		{Key: "headline", Value: headlineJS(spec, columns, rows)}}
}

func (s *PanelService) runPanelJob(j *job) {
	snap := s.app.Snapshot()
	before, _ := s.Store.Get(j.id)
	if snap == nil || before == nil {
		s.mu.Lock()
		delete(s.jobs, j.id)
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.setJob(j.id, "running")
	s.mu.Unlock()
	r := s.runOn(before.Spec, snap.Real, snap.Agent, snap.AsOf, query.Background, j.ctx)
	p, _ := s.Store.Get(j.id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == nil {
		delete(s.jobs, j.id)
		return
	}
	if !r.OK && r.Stage == "cancelled" {
		s.setJob(j.id, "cancelled")
		return
	}
	if r.OK {
		current := s.app.PanelVersions(snap)
		p.Set("status", "ok")
		p.Set("last_error", nil)
		p.Set("last_result", MakeResult(p.Spec, snap.ID, snap.AsOf, j.mode, r.Columns, r.Real, r.Tables, s.app.Lang))
		if j.mode == "manual_rule" {
			p.Set("versions", current)
		} else {
			v := setField(p.Obj("versions"), "snapshot_id", snap.ID)
			rv, _ := current.Get("renderer_version")
			p.Set("versions", setField(v, "renderer_version", rv))
		}
	} else {
		st := "recompute_failed"
		if r.Stage == "id_dependent" {
			st = "id_dependent"
		}
		p.Set("status", st)
		p.Set("last_error", jsjson.Object{{Key: "at", Value: nowISO()}, {Key: "message", Value: r.Message}})
	}
	_ = s.Store.Write(*p)
	s.setJob(j.id, "idle")
	if now := s.app.Snapshot(); now == nil || now.ID != snap.ID {
		go s.Scan()
	}
}

// Recompute queues a recompute with the current rules (only panels in review or recompute_failed).
func (s *PanelService) Recompute(id string) (string, error) {
	p, err := s.MustGet(id)
	if err != nil {
		return "", err
	}
	if p.Legacy {
		return "", &Conflict{i18n.Tr("This panel was saved before the metric dictionary and cannot be recomputed. Regenerate it with the agent", "지표 사전 이전에 저장한 패널이라 다시 계산할 수 없어요. 에이전트로 재생성해 주세요")}
	}
	if st := p.Str("status"); st != "review" && st != "recompute_failed" {
		return "", &Conflict{i18n.Tr("Only panels that need review or failed to recompute can be recomputed", "재검토 필요·재계산 실패 패널만 다시 계산할 수 있어요")}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		return "", &Conflict{i18n.Tr("Another task is running on this panel", "이 패널에서 다른 작업이 진행 중이에요")}
	}
	if st := s.jobStatus(id); st == "queued" || st == "running" || st == "cancelling" {
		return "", &Conflict{i18n.Tr("Already recomputing", "이미 재계산 중이에요")}
	}
	s.enqueuePanel(id, "manual_rule", true)
	return "queued", nil
}

// MustGet returns a panel or NotFound.
func (s *PanelService) MustGet(id string) (*panels.SavedPanel, error) {
	p, err := s.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, &NotFound{i18n.Tr("No such panel", "없는 패널")}
	}
	return p, nil
}

// Create saves a panel unless the same user already saved the same preview.
func (s *PanelService) Create(fields jsjson.Object, spec panels.Spec) (*panels.SavedPanel, bool, error) {
	hash, _ := fields.Get("preview_hash")
	by, _ := fields.Get("created_by")
	list, err := s.Store.List()
	if err != nil {
		return nil, false, err
	}
	for _, x := range list {
		h, _ := x.Fields.Get("preview_hash")
		c, _ := x.Fields.Get("created_by")
		if h == hash && c == by {
			return x, false, nil
		}
	}
	p := &panels.SavedPanel{Fields: append(append(jsjson.Object{}, fields...),
		jsjson.Member{Key: "id", Value: NewID()}, jsjson.Member{Key: "created_at", Value: nowISO()}, jsjson.Member{Key: "status", Value: "ok"}, jsjson.Member{Key: "last_error", Value: nil}), Spec: spec}
	if err := s.Store.Write(*p); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	s.emit("panels_changed", jsjson.Object{})
	s.mu.Unlock()
	if snap := s.app.Snapshot(); snap != nil && panels.Decide(*p, s.app.PanelVersions(snap)) != "none" {
		s.Scan()
	}
	return p, true, nil
}

// Delete removes a panel unless a job runs on it.
func (s *PanelService) Delete(id string) error {
	if _, err := s.MustGet(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.jobStatus(id); st == "queued" || st == "running" || st == "cancelling" || s.busy[id] {
		return &Conflict{i18n.Tr("A recompute or another task is running", "재계산이나 다른 작업이 진행 중이에요")}
	}
	if _, err := s.Store.Delete(id); err != nil {
		return err
	}
	delete(s.jobs, id)
	s.emit("panels_changed", jsjson.Object{})
	return nil
}

// Exclusive runs fn with the panel locked against other rewrites and recomputes.
func (s *PanelService) Exclusive(id string, fn func(p *panels.SavedPanel) (jsjson.Object, error)) (jsjson.Object, error) {
	p, err := s.MustGet(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if st := s.jobStatus(id); s.busy[id] || st == "queued" || st == "running" || st == "cancelling" {
		s.mu.Unlock()
		return nil, &Conflict{i18n.Tr("Another task is running on this panel", "이 패널에서 다른 작업이 진행 중이에요")}
	}
	s.busy[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.busy, id)
		s.mu.Unlock()
	}()
	return fn(p)
}

// VerifyLastResult reruns the panel on the snapshot of its last result and returns the pseudonymized rows if it matches.
func (s *PanelService) VerifyLastResult(p *panels.SavedPanel) ([]panels.Column, []panels.Row, error) {
	last := p.Obj("last_result")
	sid, _ := last.Get("snapshot_id")
	asOf, _ := last.Get("as_of")
	f := snapshot.SnapshotFiles(snapshot.Dir(s.app.WS.Config.OutDir), str(sid))
	_, e1 := os.Stat(f.Real)
	_, e2 := os.Stat(f.Agent)
	if e1 != nil || e2 != nil {
		return nil, nil, &Conflict{i18n.Tr("The snapshot of the last result is gone. Recompute first, then try again", "마지막 결과를 계산한 스냅샷이 없어요. 다시 계산한 뒤에 시도해 주세요")}
	}
	r := s.runOn(p.Spec, f.Real, f.Agent, str(asOf), query.Interactive, context.Background())
	if !r.OK {
		return nil, nil, &Conflict{i18n.Tr("Could not recheck the last result: "+r.Message, "마지막 결과를 다시 확인하지 못했어요: "+r.Message)}
	}
	saved, _ := last.Get("rows")
	if !reflect.DeepEqual(jsjson.MustStringify(rowsJS(r.Real)), jsjson.MustStringify(saved)) {
		return nil, nil, &Conflict{i18n.Tr("The rerun result differs from the last result", "마지막 결과와 다시 실행한 결과가 달라요")}
	}
	return r.Columns, r.Agent, nil
}

// UpdateSummary stores a new description.
func (s *PanelService) UpdateSummary(id, summary, snapshotID string) (*panels.SavedPanel, error) {
	p, err := s.MustGet(id)
	if err != nil {
		return nil, err
	}
	p.Set("summary", summary)
	p.Set("summary_snapshot_id", snapshotID)
	return p, s.Store.Write(*p)
}

// Quality is the latest quality check state.
func (s *PanelService) Quality() jsjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quality
}

func (s *PanelService) qualityChecks() ([]quality.Check, error) {
	specs, err := collect.LoadSpec(filepath.Join(s.app.WS.Dir, "tables.json"))
	if err != nil {
		return nil, err
	}
	params := append(append(jsjson.Object{}, s.app.WS.Config.Params...), jsjson.Member{Key: "__ga4", Value: s.app.WS.Config.GA4 != nil})
	builtin, err := quality.Builtin(specs, params, s.app.Lang)
	if err != nil {
		return nil, err
	}
	ws, err := quality.Workspace(s.app.WS.Dir)
	if err != nil {
		return nil, err
	}
	return append(builtin, ws...), nil
}

func (s *PanelService) runQuality(ctx context.Context) {
	snap := s.app.Snapshot()
	if snap == nil {
		return
	}
	s.mu.Lock()
	s.quality = setField(s.quality, "status", "running")
	sid, _ := s.quality.Get("snapshot_id")
	s.emit("quality_status", jsjson.Object{{Key: "status", Value: "running"}, {Key: "snapshot_id", Value: sid}})
	s.mu.Unlock()
	cfg := s.app.WS.Config
	var items []any
	var errMsg any
	checks, err := s.qualityChecks()
	if err != nil {
		errMsg = err.Error()
	} else {
		readable := append([]string{}, cfg.ReadablePrefixes...)
		for _, p := range []string{"r_", "snapshot_"} {
			if !contains(readable, p) {
				readable = append(readable, p)
			}
		}
		blocked := query.BlockedList{Columns: s.app.Blocked().Columns, Tables: []string{}}
		for _, c := range checks {
			if ctx.Err() != nil {
				break
			}
			var r query.Result
			if e := s.app.Slots.Run(ctx, query.Background, func(l *query.SlotLease) error {
				r = query.RunQuery(query.Request{Lease: l, SQL: c.SQL, Path: snap.Real, Mode: query.Panel, AsOf: snap.AsOf, Params: cfg.Params, ReadablePrefixes: readable,
					Blocked: &blocked, HeapLimitMb: float64(cfg.HeapLimitMb), Ctx: ctx})
				return nil
			}); e != nil {
				break
			}
			if !r.OK && r.Kind == "cancelled" {
				break
			}
			item := jsjson.Object{{Key: "id", Value: c.ID}, {Key: "title", Value: c.Title}, {Key: "display", Value: c.Display}, {Key: "builtin", Value: c.Builtin}}
			if r.OK {
				names := make([]string, len(r.Columns))
				for i, col := range r.Columns {
					names[i] = col.Name
				}
				item = append(item, jsjson.Member{Key: "columns", Value: names}, jsjson.Member{Key: "rows", Value: rowsJS(panels.UntagRows(r.Rows))}, jsjson.Member{Key: "error", Value: nil}, jsjson.Member{Key: "ms", Value: r.Ms})
			} else {
				item = append(item, jsjson.Member{Key: "columns", Value: []string{}}, jsjson.Member{Key: "rows", Value: []any{}}, jsjson.Member{Key: "error", Value: r.Message}, jsjson.Member{Key: "ms", Value: nil})
			}
			items = append(items, item)
		}
		if ctx.Err() == nil {
			items = append(items, s.seedCheck(snap, ctx))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		s.quality = setField(s.quality, "status", "idle")
	} else {
		s.quality = qualityState("idle", snap.ID, nowISO(), items, errMsg)
	}
	sid, _ = s.quality.Get("snapshot_id")
	s.emit("quality_status", jsjson.Object{{Key: "status", Value: "idle"}, {Key: "snapshot_id", Value: sid}})
}

// seedCheck runs seed panels with panel rules; failed seeds are left out of the context.
func (s *PanelService) seedCheck(snap *Snapshot, ctx context.Context) jsjson.Object {
	base := jsjson.Object{{Key: "id", Value: "q_seed_panels"}, {Key: "title", Value: i18n.Tr("Seed panel check", "시드 패널 검사")}, {Key: "display", Value: "table"}, {Key: "builtin", Value: true},
		{Key: "columns", Value: []string{"seed", "metric", "result", "tables", "message"}}}
	t0 := time.Now()
	seeds, _, err := LoadSeedPanels(s.app.WS.Dir)
	if err != nil {
		return append(base, jsjson.Member{Key: "rows", Value: []any{}}, jsjson.Member{Key: "error", Value: err.Error()}, jsjson.Member{Key: "ms", Value: nil})
	}
	rows := []any{}
	failed := map[string]bool{}
	for _, sd := range seeds {
		if ctx.Err() != nil {
			break
		}
		r := s.runOn(sd.Spec, snap.Real, snap.Agent, snap.AsOf, query.Background, ctx)
		if !r.OK && r.Stage == "cancelled" {
			break
		}
		if !r.OK {
			failed[sd.ID] = true
		}
		var metric any = i18n.Tr("(not in dictionary)", "(사전 밖)")
		if sd.Spec.Metric != nil {
			metric = *sd.Spec.Metric
		}
		result := i18n.Tr("pass", "통과")
		var msg any
		if !r.OK {
			result = i18n.Tr("failed: "+r.Stage, "실패: "+r.Stage)
			m := []rune(r.Message)
			if len(m) > 300 {
				m = m[:300]
			}
			msg = string(m)
		}
		var tables any
		if r.Tables != nil {
			tables = strings.Join(r.Tables, ", ")
		}
		rows = append(rows, []any{sd.ID, metric, result, tables, msg})
	}
	if ctx.Err() == nil {
		s.app.SetExcludedSeeds(failed)
	}
	return append(base, jsjson.Member{Key: "rows", Value: rows}, jsjson.Member{Key: "error", Value: nil}, jsjson.Member{Key: "ms", Value: float64(time.Since(t0).Milliseconds())})
}

// View picks the fields the screen may see; full adds the owner's details.
func (s *PanelService) View(p *panels.SavedPanel, snap *Snapshot, full bool) jsjson.Object {
	r := p.Obj("last_result")
	get := func(o jsjson.Object, k string) any { v, _ := o.Get(k); return v }
	cols, _ := get(r, "columns").([]any)
	names := make([]any, len(cols))
	for i, c := range cols {
		o, _ := c.(jsjson.Object)
		names[i] = jsjson.Object{{Key: "name", Value: get(o, "name")}}
	}
	var metric any
	if p.Spec.Metric != nil {
		metric = *p.Spec.Metric
	}
	def := make([]any, len(p.Spec.Definition))
	for i, d := range p.Spec.Definition {
		def[i] = []string{d[0], d[1]}
	}
	createdBy := get(p.Fields, "created_by")
	base := jsjson.Object{
		{Key: "id", Value: p.Str("id")}, {Key: "title", Value: get(p.Fields, "title")}, {Key: "description", Value: get(p.Fields, "description")},
		{Key: "summary", Value: get(p.Fields, "summary")}, {Key: "summary_snapshot_id", Value: get(p.Fields, "summary_snapshot_id")},
		{Key: "metric", Value: metric}, {Key: "display", Value: panels.DisplayJSON(p.Spec.Display)}, {Key: "definition", Value: def},
		{Key: "status", Value: get(p.Fields, "status")}, {Key: "job_status", Value: s.JobStatus(p.Str("id"))}, {Key: "created_by", Value: createdBy},
		{Key: "created_at", Value: get(p.Fields, "created_at")}, {Key: "legacy", Value: p.Legacy},
		{Key: "last_result", Value: jsjson.Object{{Key: "columns", Value: names}, {Key: "rows", Value: get(r, "rows")}, {Key: "headline", Value: get(r, "headline")},
			{Key: "caveats", Value: get(r, "caveats")}, {Key: "as_of", Value: get(r, "as_of")}, {Key: "computed_at", Value: get(r, "computed_at")},
			{Key: "mode", Value: get(r, "mode")}, {Key: "snapshot_id", Value: get(r, "snapshot_id")}}},
		{Key: "full", Value: full},
	}
	if !full {
		return base
	}
	answers := make([]any, len(p.Spec.Answers))
	specJS := p.Spec.JS()
	if a, ok := specJS.Get("answers"); ok {
		if arr, ok := a.([]any); ok {
			answers = arr
		}
	}
	changed := []string{}
	if snap != nil {
		changed = panels.ChangedRules(p.Obj("versions"), s.app.PanelVersions(snap))
		if changed == nil {
			changed = []string{}
		}
	}
	return append(base,
		jsjson.Member{Key: "answers", Value: answers}, jsjson.Member{Key: "sql", Value: p.Spec.SQL}, jsjson.Member{Key: "prompt", Value: get(p.Fields, "prompt")},
		jsjson.Member{Key: "tables", Value: get(r, "tables")}, jsjson.Member{Key: "last_error", Value: get(p.Fields, "last_error")}, jsjson.Member{Key: "versions", Value: get(p.Fields, "versions")},
		jsjson.Member{Key: "changed_rules", Value: changed}, jsjson.Member{Key: "generated_model", Value: get(p.Fields, "generated_model")})
}
