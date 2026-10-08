// 저장 패널 서비스: 저장·삭제, 백그라운드 재계산·품질 검사 대기열, 전역 이벤트.
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { isDeepStrictEqual } from 'node:util';
import { loadSeedPanels, type App, type Snapshot } from './app.ts';
import { newId } from './conversations.ts';
import { runPanel, type PanelRunResult } from '../panels/run.ts';
import { computeHeadline, effectiveCaveats, untagRows, ContractError, type Headline, type Row } from '../panels/contract.ts';
import { PanelStore, type JobStatus, type LastResult, type PanelVersions, type ResultMode, type SavedPanel } from '../panels/store.ts';
import { changedRules, decide } from '../panels/recompute.ts';
import { displayToJson, type PanelSpec } from '../panels/spec.ts';
import { readCurrent, snapshotFiles, snapshotsDir } from '../snapshot/store.ts';
import { runQuery, SlotCancelled, type SlotLease } from '../query/executor.ts';
import type { ResultColumn } from '../query/worker.ts';
import { builtinChecks, workspaceChecks, type QualityCheck } from '../quality/builtin.ts';
import { loadSpec } from '../collect/spec.ts';

export type PanelEvent = { type: string; data: Record<string, unknown> };

export type QualityItem = { id: string; title: string; display: 'table' | 'line'; builtin: boolean; columns: string[]; rows: Row[]; error: string | null; ms: number | null };
export type QualityState = { status: 'idle' | 'queued' | 'running'; snapshot_id: string | null; computed_at: string | null; items: QualityItem[]; error: string | null };

type Job = { kind: 'panel'; id: string; mode: 'auto' | 'manual_rule'; ac: AbortController } | { kind: 'quality'; ac: AbortController };

export class Conflict extends Error {}
export class NotFound extends Error {}

export function headlineOf(spec: PanelSpec, columns: ResultColumn[], rows: Row[]): Headline {
  try {
    return computeHeadline(spec, columns, rows);
  } catch (e) {
    if (e instanceof ContractError) return null;
    throw e;
  }
}

export function makeResult(spec: PanelSpec, snap: { id: string; asOf: string }, mode: ResultMode, columns: ResultColumn[], rows: Row[], tables: string[]): LastResult {
  return {
    snapshot_id: snap.id, as_of: snap.asOf, computed_at: new Date().toISOString(), mode, columns, rows, tables,
    caveats: effectiveCaveats(spec, columns, rows), headline: headlineOf(spec, columns, rows),
  };
}

export class PanelService {
  readonly store: PanelStore;
  private readonly app: App;
  private readonly jobs = new Map<string, JobStatus>();
  /** 설명 다시 쓰기·삭제 중인 패널 */
  private readonly busy = new Set<string>();
  private readonly queue: Job[] = [];
  private running: Job | null = null;
  private readonly listeners = new Set<(e: PanelEvent) => void>();
  private watchTimer: NodeJS.Timeout | null = null;
  private lastSnapshotId: string | null = null;
  quality: QualityState = { status: 'idle', snapshot_id: null, computed_at: null, items: [], error: null };
  private stopped = false;
  /** 품질 검사를 대기열에 넣은 스냅샷 */
  private qualityTarget: string | null = null;

  constructor(app: App) {
    this.app = app;
    this.store = new PanelStore(join(app.ws.config.outDir, 'panels'));
  }

  /** 서버 시작 뒤: 재계산 판정, 품질 검사, 스냅샷 변경 감시 */
  start(pollMs = 2000): void {
    this.lastSnapshotId = this.app.snapshot()?.id ?? null;
    this.scan();
    this.watchTimer = setInterval(() => this.poll(), pollMs);
    this.watchTimer.unref();
  }

  async stop(): Promise<void> {
    this.stopped = true;
    if (this.watchTimer) clearInterval(this.watchTimer);
    for (const j of this.queue) j.ac.abort();
    this.queue.length = 0;
    this.running?.ac.abort();
    while (this.running) await new Promise((r) => setTimeout(r, 20));
  }

  subscribe(fn: (e: PanelEvent) => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  private emit(type: string, data: Record<string, unknown>): void {
    for (const fn of this.listeners) fn({ type, data });
  }

  jobStatus(id: string): JobStatus {
    return this.jobs.get(id) ?? 'idle';
  }

  private setJob(id: string, s: JobStatus): void {
    this.jobs.set(id, s);
    const p = this.store.get(id);
    if (p) this.emit('panel_status', this.statusOf(p));
  }

  /** 이벤트에는 최소 상태만 */
  statusOf(p: SavedPanel) {
    return { panel_id: p.id, status: p.status, job_status: this.jobStatus(p.id) };
  }

  allStatus() {
    return this.store.list().map((p) => this.statusOf(p));
  }

  private poll(): void {
    const cur = readCurrent(snapshotsDir(this.app.ws.config.outDir));
    const id = cur?.snapshot_id ?? null;
    if (id === this.lastSnapshotId) return;
    this.lastSnapshotId = id;
    const snap = this.app.snapshot();
    // 내용 없이 알리기만 한다(화면이 상태를 다시 읽음)
    this.emit('snapshot_changed', {});
    this.scan();
  }

  currentVersions(snap: Snapshot): PanelVersions {
    return this.app.panelVersions(snap);
  }

  /** 저장 패널마다 재계산 판정 후 대기열에 넣고, 품질 검사를 맨 뒤에 넣는다 */
  scan(): void {
    const snap = this.app.snapshot();
    if (!snap || this.stopped) return;
    const current = this.currentVersions(snap);
    for (const p of this.store.list()) {
      const d = p.legacy ? (p.status === 'review' ? 'none' : 'review') : decide(p, current);
      if (d === 'review') {
        this.store.write({ ...p, status: 'review' });
        this.emit('panel_status', this.statusOf({ ...p, status: 'review' }));
      } else if (d === 'auto') {
        this.enqueuePanel(p.id, 'auto');
      } else if (p.versions.renderer_version !== current.renderer_version && !changedRules(p.versions, current).length) {
        this.store.write({ ...p, versions: { ...p.versions, renderer_version: current.renderer_version } });
      }
    }
    if (this.qualityTarget !== snap.id) this.enqueueQuality(snap.id);
  }

  private enqueuePanel(id: string, mode: 'auto' | 'manual_rule'): void {
    const s = this.jobStatus(id);
    if (s === 'queued' || s === 'running' || s === 'cancelling') return;
    this.queue.push({ kind: 'panel', id, mode, ac: new AbortController() });
    this.setJob(id, 'queued');
    void this.pump();
  }

  private enqueueQuality(snapshotId: string): void {
    this.qualityTarget = snapshotId;
    if (this.queue.some((j) => j.kind === 'quality')) return;
    this.queue.push({ kind: 'quality', ac: new AbortController() });
    if (this.quality.status !== 'running') {
      this.quality = { ...this.quality, status: 'queued' };
      this.emit('quality_status', { status: 'queued', snapshot_id: this.quality.snapshot_id });
    }
    void this.pump();
  }

  private async pump(): Promise<void> {
    if (this.running || this.stopped) return;
    // 패널 재계산을 품질 검사보다 먼저
    let i = this.queue.findIndex((j) => j.kind === 'panel');
    if (i < 0) i = 0;
    const job = this.queue.splice(i, 1)[0];
    if (!job) return;
    this.running = job;
    try {
      if (job.kind === 'panel') await this.runPanelJob(job);
      else await this.runQuality(job.ac.signal);
    } catch (e) {
      console.error(e);
    } finally {
      this.running = null;
      void this.pump();
    }
  }

  /** 대기 중이면 바로 빼고, 실행 중이면 worker 종료를 기다린다 */
  async cancel(id: string): Promise<JobStatus> {
    const s = this.jobStatus(id);
    if (s === 'queued') {
      const i = this.queue.findIndex((j) => j.kind === 'panel' && j.id === id);
      if (i >= 0) this.queue.splice(i, 1);
      this.setJob(id, 'cancelled');
      return 'cancelled';
    }
    if (s === 'running' && this.running?.kind === 'panel' && this.running.id === id) {
      const job = this.running;
      this.setJob(id, 'cancelling');
      job.ac.abort();
      while (this.running === job) await new Promise((r) => setTimeout(r, 20));
      return this.jobStatus(id);
    }
    throw new Conflict('진행 중이거나 대기 중인 재계산이 없어요');
  }

  /** 원본·가명 사본에서 차례로 실행 */
  private runOn(spec: PanelSpec, snap: { real: string; agent: string; asOf: string }, kind: 'interactive' | 'background', signal?: AbortSignal): Promise<PanelRunResult> {
    const cfg = this.app.ws.config;
    return this.app.slots.run(kind, signal, (lease: SlotLease) => runPanel({
      spec, paths: { real: snap.real, agent: snap.agent }, asOf: snap.asOf, params: cfg.params,
      policy: cfg.policy, metrics: this.app.metrics().dict, blocked: this.app.blocked(), heapLimitMb: cfg.run.heapLimitMb, roles: this.app.roles, lease, signal,
    })).catch((e) => {
      if (e instanceof SlotCancelled) return { ok: false, stage: 'cancelled', message: '취소됨' } as PanelRunResult;
      throw e;
    });
  }

  private async runPanelJob(job: Extract<Job, { kind: 'panel' }>): Promise<void> {
    const snap = this.app.snapshot();
    const before = this.store.get(job.id);
    if (!snap || !before) {
      this.jobs.delete(job.id);
      return;
    }
    this.setJob(job.id, 'running');
    const r = await this.runOn(before.spec, snap, 'background', job.ac.signal);
    const p = this.store.get(job.id);
    if (!p) {
      this.jobs.delete(job.id);
      return;
    }
    if (!r.ok && r.stage === 'cancelled') {
      this.setJob(job.id, 'cancelled');
      return;
    }
    const now = new Date().toISOString();
    let next: SavedPanel;
    if (r.ok) {
      const current = this.currentVersions(snap);
      next = {
        ...p,
        status: 'ok',
        last_error: null,
        last_result: makeResult(p.spec, snap, job.mode, r.columns, r.real, r.tables),
        versions: job.mode === 'manual_rule' ? current : { ...p.versions, snapshot_id: snap.id, renderer_version: current.renderer_version },
      };
    } else {
      next = { ...p, status: r.stage === 'id_dependent' ? 'id_dependent' : 'recompute_failed', last_error: { at: now, message: r.message } };
    }
    this.store.write(next);
    this.setJob(job.id, 'idle');
    // 실행 중에 스냅샷이 또 바뀌었으면 다시 판정한다
    if (this.app.snapshot()?.id !== snap.id) this.scan();
  }

  /** [이 규칙으로 다시 계산]: 재검토·재계산 실패 패널만 */
  recompute(id: string): JobStatus {
    const p = this.mustGet(id);
    if (p.legacy) throw new Conflict('지표 사전 이전에 저장한 패널이라 다시 계산할 수 없어요. 에이전트로 재생성해 주세요');
    if (p.status !== 'review' && p.status !== 'recompute_failed') throw new Conflict('재검토 필요·재계산 실패 패널만 다시 계산할 수 있어요');
    if (this.busy.has(id)) throw new Conflict('이 패널에서 다른 작업이 진행 중이에요');
    const s = this.jobStatus(id);
    if (s === 'queued' || s === 'running' || s === 'cancelling') throw new Conflict('이미 재계산 중이에요');
    this.queue.unshift({ kind: 'panel', id, mode: 'manual_rule', ac: new AbortController() });
    this.setJob(id, 'queued');
    void this.pump();
    return 'queued';
  }

  mustGet(id: string): SavedPanel {
    const p = this.store.get(id);
    if (!p) throw new NotFound('없는 패널');
    return p;
  }

  create(p: Omit<SavedPanel, 'id' | 'created_at' | 'status' | 'last_error'> & { created_by: string }): { panel: SavedPanel; created: boolean } {
    const dup = this.store.list().find((x) => x.preview_hash === p.preview_hash && x.created_by === p.created_by);
    if (dup) return { panel: dup, created: false };
    const panel: SavedPanel = { ...p, id: newId(), created_at: new Date().toISOString(), status: 'ok', last_error: null };
    this.store.write(panel);
    this.emit('panels_changed', {});
    // 저장 사이에 스냅샷이 바뀌었으면 바로 판정한다
    const snap = this.app.snapshot();
    if (snap && decide(panel, this.currentVersions(snap)) !== 'none') this.scan();
    return { panel, created: true };
  }

  delete(id: string): void {
    this.mustGet(id);
    const s = this.jobStatus(id);
    if (s === 'queued' || s === 'running' || s === 'cancelling' || this.busy.has(id)) throw new Conflict('재계산이나 다른 작업이 진행 중이에요');
    this.store.delete(id);
    this.jobs.delete(id);
    this.emit('panels_changed', {});
  }

  /** 설명 다시 쓰기·삭제를 한 번에 하나만 */
  async exclusive<T>(id: string, fn: (p: SavedPanel) => Promise<T>): Promise<T> {
    const p = this.mustGet(id);
    const s = this.jobStatus(id);
    if (this.busy.has(id) || s === 'queued' || s === 'running' || s === 'cancelling') throw new Conflict('이 패널에서 다른 작업이 진행 중이에요');
    this.busy.add(id);
    try {
      return await fn(p);
    } finally {
      this.busy.delete(id);
    }
  }

  /** 마지막 결과를 계산한 스냅샷에서 다시 실행해 같은지 확인하고 가명 결과를 돌려준다 */
  async verifyLastResult(p: SavedPanel): Promise<{ columns: ResultColumn[]; agentRows: Row[] }> {
    const dir = snapshotsDir(this.app.ws.config.outDir);
    const f = snapshotFiles(dir, p.last_result.snapshot_id);
    if (!existsSync(f.real) || !existsSync(f.agent)) throw new Conflict('마지막 결과를 계산한 스냅샷이 없어요. 다시 계산한 뒤에 시도해 주세요');
    const r = await this.runOn(p.spec, { real: f.real, agent: f.agent, asOf: p.last_result.as_of }, 'interactive');
    if (!r.ok) throw new Conflict(`마지막 결과를 다시 확인하지 못했어요: ${r.message}`);
    if (!isDeepStrictEqual(r.real, p.last_result.rows)) throw new Conflict('마지막 결과와 다시 실행한 결과가 달라요');
    return { columns: r.columns, agentRows: r.agent };
  }

  updateSummary(id: string, summary: string, snapshotId: string): SavedPanel {
    const p = this.mustGet(id);
    const next = { ...p, summary, summary_snapshot_id: snapshotId };
    this.store.write(next);
    return next;
  }

  // ── 품질 검사 ─────────────────────────────────────────

  qualityChecks(): QualityCheck[] {
    return [...builtinChecks(loadSpec(join(this.app.ws.dir, 'tables.json')), this.app.ws.config.params), ...workspaceChecks(this.app.ws.dir)];
  }

  private async runQuality(signal: AbortSignal): Promise<void> {
    const snap = this.app.snapshot();
    if (!snap) return;
    this.quality = { ...this.quality, status: 'running' };
    this.emit('quality_status', { status: 'running', snapshot_id: this.quality.snapshot_id });
    const cfg = this.app.ws.config;
    const items: QualityItem[] = [];
    let error: string | null = null;
    try {
      for (const c of this.qualityChecks()) {
        if (signal.aborted) break;
        const r = await this.app.slots.run('background', signal, (lease) => runQuery({
          lease, sql: c.sql, path: snap.real, mode: 'panel', asOf: snap.asOf, params: cfg.params,
          readablePrefixes: [...new Set([...cfg.policy.readablePrefixes, 'r_', 'snapshot_'])], blocked: { columns: this.app.blocked().columns, tables: [] }, heapLimitMb: cfg.run.heapLimitMb, signal,
        })).catch((e) => {
          if (e instanceof SlotCancelled) return { ok: false as const, kind: 'cancelled' as const, message: '취소됨' };
          throw e;
        });
        if (!r.ok && r.kind === 'cancelled') break;
        items.push(r.ok
          ? { id: c.id, title: c.title, display: c.display, builtin: c.builtin, columns: r.columns.map((x) => x.name), rows: untagRows(r.rows), error: null, ms: r.ms }
          : { id: c.id, title: c.title, display: c.display, builtin: c.builtin, columns: [], rows: [], error: r.message, ms: null });
      }
      if (!signal.aborted) items.push(await this.seedCheck(snap, signal));
    } catch (e) {
      error = (e as Error).message;
    }
    if (signal.aborted) {
      this.quality = { ...this.quality, status: 'idle' };
    } else {
      this.quality = { status: 'idle', snapshot_id: snap.id, computed_at: new Date().toISOString(), items, error };
    }
    this.emit('quality_status', { status: 'idle', snapshot_id: this.quality.snapshot_id });
  }

  /** 시드 패널을 패널과 같은 규칙으로 실행한다. 실패한 시드는 컨텍스트에서 뺀다 */
  private async seedCheck(snap: Snapshot, signal: AbortSignal): Promise<QualityItem> {
    const base = { id: 'q_seed_panels', title: '시드 패널 검사', display: 'table' as const, builtin: true, columns: ['seed', 'metric', 'result', 'tables', 'message'] };
    const t0 = performance.now();
    try {
      const rows: Row[] = [];
      const failed = new Set<string>();
      for (const s of loadSeedPanels(this.app.ws.dir).panels) {
        if (signal.aborted) break;
        const r = await this.runOn(s.spec, snap, 'background', signal);
        if (!r.ok && r.stage === 'cancelled') break;
        if (!r.ok) failed.add(s.id);
        rows.push([s.id, s.spec.metric ?? '(사전 밖)', r.ok ? '통과' : `실패: ${r.stage}`, (r.ok ? r.tables : r.tables)?.join(', ') ?? null, r.ok ? null : r.message.slice(0, 300)]);
      }
      if (!signal.aborted) this.app.excludedSeeds = failed;
      return { ...base, rows, error: null, ms: Math.round(performance.now() - t0) };
    } catch (e) {
      return { ...base, rows: [], error: (e as Error).message, ms: null };
    }
  }

  // ── 화면용 ───────────────────────────────────────────

  /** 역할·소유에 따라 필드를 명시적으로 고른다 */
  view(p: SavedPanel, snap: Snapshot | null, full: boolean) {
    const r = p.last_result;
    const base = {
      id: p.id, title: p.title, description: p.description, summary: p.summary, summary_snapshot_id: p.summary_snapshot_id,
      metric: p.spec.metric, display: displayToJson(p.spec.display), definition: p.spec.definition,
      status: p.status, job_status: this.jobStatus(p.id), created_by: p.created_by ?? null, created_at: p.created_at, legacy: p.legacy === true,
      last_result: { columns: r.columns.map((c) => ({ name: c.name })), rows: r.rows, headline: r.headline, caveats: r.caveats, as_of: r.as_of, computed_at: r.computed_at, mode: r.mode, snapshot_id: r.snapshot_id },
      full,
    };
    if (!full) return base;
    const current = snap ? this.currentVersions(snap) : null;
    return {
      ...base,
      answers: p.spec.answers,
      sql: p.spec.sql,
      prompt: p.prompt,
      tables: r.tables ?? null,
      last_error: p.last_error,
      versions: p.versions,
      changed_rules: current ? changedRules(p.versions, current) : [],
      generated_model: p.generated_model,
    };
  }
}
