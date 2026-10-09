// Conversations: start and cancel requests, SSE events, conversation log (jsonl), session resume.
import { appendFileSync, existsSync, mkdirSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { join } from 'node:path';
import type { App, Snapshot } from './app.ts';
import { AgentRequest, type LoopEvent } from '../agent/loop.ts';
import { sensitiveTopic } from '../agent/sensitive.ts';
import { Outbound } from '../agent/outbound.ts';
import type { AskQuestion } from '../agent/actions.ts';
import { ACTION_SCHEMA_ARG } from '../agent/actions.ts';
import { SlotCancelled, runQuery } from '../query/executor.ts';
import { runPanel } from '../panels/run.ts';
import { computeHeadline, effectiveCaveats, ContractError, type Row, type Headline } from '../panels/contract.ts';
import { semanticHash, contextVersionKey, type ContextVersion } from '../panels/hash.ts';
import { displayToJson, type PanelSpec } from '../panels/spec.ts';
import type { ResultColumn } from '../query/worker.ts';
import { tr } from '../i18n.ts';

export const ID_RE = /^[a-z0-9]{12}$/;
export const newId = () => randomBytes(9).toString('base64url').toLowerCase().replace(/[^a-z0-9]/g, '').padEnd(12, '0').slice(0, 12);

export type Preview = {
  request_id: string;
  spec: PanelSpec;
  display: Record<string, unknown>;
  columns: ResultColumn[];
  rows: Row[];
  caveats: string[];
  headline: Headline;
  preview_hash: string;
  snapshot_id: string;
  as_of: string;
  plan: string | null;
  versions: ContextVersion;
  /** Tables the panel SQL reads */
  tables: string[];
};

type Active = { id: string; cancelled: boolean; run: Promise<void>; req: AgentRequest | null };

export type ServerEvent = { event_id: number; conversation_id: string; request_id: string | null; turn_no: number; type: string; data: Record<string, unknown> };

const BUFFER = 500;

export class Conversation {
  readonly id: string;
  private readonly app: App;
  private readonly file: string;
  private nextEventId = 1;
  private readonly buffer: ServerEvent[] = [];
  private readonly listeners = new Set<(e: ServerEvent) => void>();

  request: AgentRequest | null = null;
  requestId: string | null = null;
  /** Request in progress or waiting for an answer */
  private active: Active | null = null;
  private queued: { id: string; text: string } | null = null;
  private cancelling: Promise<void> | null = null;

  preview: Preview | null = null;
  session: { id: string; contextVersion: string } | null = null;
  inputs: string[] = [];
  lastAsk: { questions: AskQuestion[]; answers: Record<string, string | undefined> } | null = null;
  private lastPlan: string | null = null;
  private snap: Snapshot | null = null;
  private ctx: ContextVersion | null = null;
  /** Pseudonymized result of the current preview (input for the description) */
  private previewAgentRows: Row[] | null = null;
  /** Original panel when this conversation came from [Regenerate with agent] */
  origin: { panel_id: string; prompt: string } | null = null;
  /** Creator; missing in older logs */
  owner: string | null = null;
  drafting = false;
  /** Locked by a sensitive-topic question (a new conversation is needed) */
  locked = false;
  /** Request stopped by a restart (shown until a new request starts) */
  private restartFailure: { request_id: string; reason: string; message: string } | null = null;

  constructor(app: App, id: string, dir: string) {
    this.app = app;
    this.id = id;
    this.file = join(dir, `${id}.jsonl`);
  }

  /** Restore from the conversation log */
  load(): void {
    if (!existsSync(this.file)) return;
    let failedRequest: string | null = null;
    for (const line of readFileSync(this.file, 'utf8').split('\n')) {
      if (!line.trim()) continue;
      let e: Record<string, unknown>;
      try {
        e = JSON.parse(line);
      } catch {
        continue;
      }
      if (e.type === 'user_input') this.inputs.push(String(e.text));
      if (e.type === 'question') this.lastAsk = { questions: e.questions as AskQuestion[], answers: {} };
      if (e.type === 'answers' && this.lastAsk) {
        for (const a of e.answers as { id: string; answer: string }[]) this.lastAsk.answers[a.id] = a.answer;
      }
      if (e.type === 'session') this.session = { id: String(e.session_id), contextVersion: String(e.context_version) };
      if (e.type === 'session_discarded') this.session = null;
      if (e.type === 'locked') {
        this.locked = true;
        this.preview = null;
        this.previewAgentRows = null;
      }
      if (e.type === 'preview') {
        this.preview = e.preview as Preview;
        this.previewAgentRows = Array.isArray(e.agent_rows) ? (e.agent_rows as Row[]) : null;
      }
      if (e.type === 'owner') this.owner = String(e.user);
      if (e.type === 'origin') this.origin = { panel_id: String(e.panel_id), prompt: String(e.prompt) };
      if (e.type === 'request_started') failedRequest = String(e.request_id);
      if (e.type === 'done' || e.type === 'failed' || e.type === 'cancelled') failedRequest = null;
    }
    if (failedRequest) {
      const message = tr('The server restarted, so the running request was stopped', '서버가 다시 시작되어 진행 중이던 요청을 멈췄어요');
      this.record({ type: 'failed', request_id: failedRequest, reason: 'server_restart', message });
      this.restartFailure = { request_id: failedRequest, reason: 'server_restart', message };
    }
  }

  private record(e: Record<string, unknown>): void {
    appendFileSync(this.file, JSON.stringify({ t: new Date().toISOString(), ...e }) + '\n');
  }

  hasInput(): boolean {
    return this.inputs.length > 0;
  }

  isLocked(): boolean {
    return this.locked;
  }

  setOwner(user: string): void {
    this.owner = user;
    this.record({ type: 'owner', user });
  }

  setOrigin(panelId: string, prompt: string): void {
    this.origin = { panel_id: panelId, prompt };
    this.record({ type: 'origin', panel_id: panelId, prompt });
  }

  /** Original request text of the panel to save */
  firstPrompt(): string {
    return this.origin?.prompt ?? this.inputs[0] ?? '';
  }

  /** Only when the request ID and semantic hash match the current finished preview */
  completedPreview(requestId: string, previewHash: string): { preview: Preview; agentRows: Row[] | null } | null {
    if (this.locked) return null;
    const p = this.preview;
    if (!p || p.request_id !== requestId || p.preview_hash !== previewHash) return null;
    // Not while a later request is running or waiting for an answer
    if (this.active || this.queued) return null;
    return { preview: p, agentRows: this.previewAgentRows };
  }

  subscribe(fn: (e: ServerEvent) => void, lastEventId: number | null): { replay: ServerEvent[] | 'resync'; unsubscribe: () => void } {
    this.listeners.add(fn);
    let replay: ServerEvent[] | 'resync' = [];
    if (lastEventId !== null) {
      const oldest = this.buffer[0]?.event_id ?? this.nextEventId;
      replay = lastEventId + 1 < oldest ? 'resync' : this.buffer.filter((e) => e.event_id > lastEventId);
    }
    return { replay, unsubscribe: () => this.listeners.delete(fn) };
  }

  private push(type: string, requestId: string | null, turnNo: number, data: Record<string, unknown>): void {
    const e: ServerEvent = { event_id: this.nextEventId++, conversation_id: this.id, request_id: requestId, turn_no: turnNo, type, data };
    this.buffer.push(e);
    if (this.buffer.length > BUFFER) this.buffer.shift();
    for (const fn of this.listeners) fn(e);
  }


  state() {
    return {
      conversation_id: this.id,
      request: this.request ? { request_id: this.requestId, state: this.request.state, turn_no: this.request.turnNo } : null,
      question: this.request?.state === 'waiting_user' && this.request.pendingAsk ? { request_id: this.requestId, turn_no: this.request.pendingAsk.turnNo, questions: this.request.pendingAsk.questions } : null,
      offdict: this.request?.state === 'waiting_user' && this.request.pendingOffdict ? { request_id: this.requestId, turn_no: this.request.pendingOffdict.turnNo, ...this.offdictView(this.request.pendingOffdict) } : null,
      preview: this.preview,
      locked: this.locked,
      failed: this.request ? null : this.restartFailure,
      last_event_id: this.nextEventId - 1,
    };
  }

  private cancelActive(message: string): Promise<void> {
    if (this.cancelling) return this.cancelling;
    const a = this.active!;
    const req = a.req;
    if (req?.finished) {
      this.active = null;
      return Promise.resolve();
    }
    a.cancelled = true;
    this.cancelling = (async () => {
      if (req) await req.cancel();
      await a.run;
      this.record({ type: 'cancelled', request_id: a.id });
      this.push('cancelled', a.id, req?.turnNo ?? 0, { message });
      if (this.active === a) this.active = null;
      this.cancelling = null;
    })();
    return this.cancelling;
  }

  /** Also checks all inputs of this conversation together (questions split across turns) */
  private sensitive(text: string): boolean {
    return sensitiveTopic(text) !== null || sensitiveTopic([...this.inputs, ...Object.values(this.lastAsk?.answers ?? {}), text].join(' ')) !== null;
  }

  /** Refuse without calling the agent and lock the conversation. The input itself is not logged */
  private refuseSensitive(id: string): void {
    this.locked = true;
    this.session = null;
    this.preview = null;
    this.previewAgentRows = null;
    this.record({ type: 'locked', request_id: id });
    this.app.audit?.tryWrite({ event: 'sensitive_blocked', user: this.owner ?? undefined, target: this.id });
    this.push('user_input', id, 0, { text: tr('(request not recorded)', '(기록하지 않은 요청)') });
    this.push('refused', id, 0, { reason: tr('This tool does not handle credentials, tokens, connection details, settings or personal contacts. This conversation is locked. Please ask about metrics in a new conversation.', '이 도구는 인증 정보·토큰·연결 정보·설정·개인 연락처를 다루지 않아요. 이 대화는 잠겼어요. 새 대화에서 지표로 물어봐 주세요.'), alternatives: [] });
    this.push('done', id, 0, {});
  }

  /** Cancels a running request first. Only the last waiting input is kept */
  submit(text: string): string {
    const id = newId();
    if (this.locked || this.sensitive(text)) {
      const wasLocked = this.locked;
      this.queued = null;
      const finish = () => {
        if (!wasLocked) return this.refuseSensitive(id);
        this.push('user_input', id, 0, { text: tr('(request not recorded)', '(기록하지 않은 요청)') });
        this.push('refused', id, 0, { reason: tr('This conversation is locked. Please open a new one.', '이 대화는 잠겼어요. 새 대화를 열어 주세요.'), alternatives: [] });
        this.push('done', id, 0, {});
      };
      if (this.active) void this.cancelActive(tr('Cancelled the previous request', '이전 요청을 취소했습니다')).then(finish);
      else finish();
      return id;
    }
    if (this.active) {
      this.queued = { id, text };
      this.push('queued', id, 0, { text });
      void this.cancelActive(tr('Cancelled the previous request', '이전 요청을 취소했습니다')).then(() => {
        const q = this.queued;
        this.queued = null;
        if (q && !this.active) this.begin(q.id, q.text);
      });
      return id;
    }
    this.begin(id, text);
    return id;
  }

  private begin(id: string, text: string): void {
    const a: Active = { id, cancelled: false, run: Promise.resolve(), req: null };
    this.active = a;
    this.request = null;
    a.run = this.start(a, text).finally(() => {
      if (this.active === a && !a.cancelled && (!a.req || a.req.finished)) this.active = null;
    });
  }

  async stop(): Promise<boolean> {
    this.queued = null;
    if (!this.active) return false;
    await this.cancelActive(tr('Request stopped', '요청을 멈췄어요'));
    return true;
  }

  answer(requestId: string, turnNo: number, answers: Record<string, string | undefined>): boolean {
    const a = this.active;
    if (!a || a.id !== requestId || a.cancelled || !a.req) return false;
    const free = Object.values(answers).filter((v): v is string => typeof v === 'string').join(' ');
    if (free && this.sensitive(free)) {
      void this.cancelActive(tr('Request stopped', '요청을 멈췄어요')).then(() => this.refuseSensitive(newId()));
      return true;
    }
    const req = a.req;
    const p = req.answer(turnNo, answers);
    if (p === null) return false;
    a.run = p.finally(() => {
      if (this.active === a && !a.cancelled && req.finished) this.active = null;
    });
    return true;
  }

  offdict(requestId: string, turnNo: number, approve: boolean): boolean {
    const a = this.active;
    if (!a || a.id !== requestId || a.cancelled || !a.req) return false;
    const req = a.req;
    const p = req.approveOffdict(turnNo, approve);
    if (p === null) return false;
    a.run = p.finally(() => {
      if (this.active === a && !a.cancelled && req.finished) this.active = null;
    });
    return true;
  }

  /** Before approval only the definition and tables are sent (results stay on the server) */
  private offdictView(o: { spec: PanelSpec; columns: ResultColumn[]; rows: Row[]; tables: string[] }) {
    return { spec: o.spec, tables: o.tables, caveats: effectiveCaveats(o.spec, o.columns, o.rows) };
  }

  private async start(a: Active, text: string): Promise<void> {
    const app = this.app;
    const requestId = a.id;
    this.requestId = requestId;
    this.inputs.push(text);
    this.record({ type: 'user_input', request_id: requestId, text });
    this.push('user_input', requestId, 0, { text });

    await app.waitAgent();
    if (a.cancelled) return;
    const fail = (message: string) => {
      this.record({ type: 'failed', request_id: requestId, reason: 'unavailable', message });
      this.push('failed', requestId, 0, { reason: 'unavailable', message });
    };
    if (app.agent.state !== 'ok') return fail(app.agent.message);
    const snap = app.snapshot();
    if (!snap) return fail(tr('No snapshot yet. Run collect first', '스냅샷이 없어요. 먼저 collect를 실행해 주세요'));
    this.snap = snap;
    const ctx = app.contextVersion(snap);
    this.ctx = ctx;
    const ctxKey = contextVersionKey(ctx);
    let systemPrompt: string;
    let metrics: ReturnType<App['metrics']>['dict'];
    try {
      metrics = app.metrics().dict;
      systemPrompt = app.systemPrompt(snap, ctx);
    } catch (e) {
      // Details (file and column names) go to the server log only
      console.error(`Building the agent context failed: ${(e as Error).message}`);
      return fail(tr('There is a problem with the agent setup (guide, metric dictionary or example panels). Please tell an admin', '에이전트 설정(설명서·지표 사전·예시 패널)에 문제가 있어요. 관리자에게 알려 주세요'));
    }
    const cfg = app.ws.config;
    const outbound = new Outbound(cfg.agent.dataMode, app.roles);
    const resume = this.session && this.session.contextVersion === ctxKey ? this.session.id : null;
    const hadHistory = this.inputs.length > 1 || this.preview !== null;
    const recovery = hadHistory ? outbound.recoverySummary({ inputs: this.inputs.slice(0, -1), lastAsk: this.lastAsk, current: this.preview?.spec ?? null }) : null;
    if (this.session && !resume) this.push('step', requestId, 0, { kind: 'retry', text: tr('Data or guide changed; continuing in a new session', '데이터나 설명서가 바뀌어 새 세션으로 이어갑니다') });

    const paths = { real: snap.real, agent: snap.agent };
    const blocked = app.blocked();
    const req = new AgentRequest(requestId, {
      call: async ({ input, sessionId, budgetUsd, signal }) => {
        try {
          return await app.slots.run('interactive', signal, () => app.runner.call({
            input, sessionId, budgetUsd, signal, systemPrompt, jsonSchema: ACTION_SCHEMA_ARG, model: cfg.agent.model, timeoutMs: cfg.agent.callTimeoutMs,
          }));
        } catch (e) {
          if (e instanceof SlotCancelled) return { ok: false, type: 'cancelled', message: tr('Cancelled', '취소됨'), sessionId: null, costUsd: 0, ms: 0 };
          throw e;
        }
      },
      probe: async (sql, signal) => {
        try {
          return await app.slots.run('interactive', signal, (lease) => runQuery({
            lease, sql, path: paths.agent, mode: 'probe', asOf: snap.asOf, params: cfg.params, readablePrefixes: cfg.policy.readablePrefixes, blocked, heapLimitMb: cfg.run.heapLimitMb, signal,
          }));
        } catch (e) {
          if (e instanceof SlotCancelled) return { ok: false, kind: 'cancelled', message: tr('Cancelled', '취소됨') };
          throw e;
        }
      },
      panel: async (spec, signal) => {
        try {
          return await app.slots.run('interactive', signal, (lease) => runPanel({
            spec, paths, asOf: snap.asOf, params: cfg.params, policy: cfg.policy, metrics, blocked, heapLimitMb: cfg.run.heapLimitMb, roles: app.roles, lease, signal,
          }));
        } catch (e) {
          if (e instanceof SlotCancelled) return { ok: false, stage: 'cancelled', message: tr('Cancelled', '취소됨') };
          throw e;
        }
      },
      sleep: (ms, signal) => new Promise((res) => {
        const t = setTimeout(res, ms);
        signal.addEventListener('abort', () => { clearTimeout(t); res(); }, { once: true });
      }),
      outbound,
      limits: { maxTurns: cfg.agent.maxTurns, maxProbes: cfg.agent.maxProbes, maxFixes: cfg.agent.maxFixes, callBudgetUsd: cfg.agent.callBudgetUsd, requestBudgetUsd: cfg.agent.requestBudgetUsd },
      emit: (ev, turnNo) => this.onLoopEvent(requestId, ev, turnNo, ctxKey),
      onIsolationFailure: () => app.disableAgent(tr('Agent isolation check failed: the agent is unavailable until the server restarts', '에이전트 격리 점검 실패: 서버를 다시 시작하기 전까지 에이전트를 쓸 수 없어요')),
    });
    if (a.cancelled) return;
    a.req = req;
    this.request = req;
    this.lastPlan = null;
    await req.run({ text, current: this.preview?.spec ?? null, resumeSessionId: resume, recovery });
  }

  private onLoopEvent(requestId: string, ev: LoopEvent, turnNo: number, ctxKey: string): void {
    if (requestId !== this.requestId) return;
    switch (ev.type) {
      case 'request_started':
        this.record({ type: 'request_started', request_id: requestId });
        this.push('request_started', requestId, turnNo, {});
        return;
      case 'session':
        this.session = { id: ev.sessionId, contextVersion: ctxKey };
        this.record({ type: 'session', request_id: requestId, session_id: ev.sessionId, context_version: ctxKey });
        return;
      case 'question':
        this.lastAsk = { questions: ev.questions, answers: {} };
        this.record({ type: 'question', request_id: requestId, questions: ev.questions });
        this.push('question', requestId, turnNo, { questions: ev.questions });
        return;
      case 'answers':
        if (this.lastAsk) for (const a of ev.answers) this.lastAsk.answers[a.id] = a.answer;
        this.record({ type: 'answers', request_id: requestId, answers: ev.answers });
        this.push('answers', requestId, turnNo, { answers: ev.answers });
        return;
      case 'plan':
        this.lastPlan = ev.text;
        this.record({ type: 'plan', request_id: requestId, text: ev.text });
        this.push('plan', requestId, turnNo, { text: ev.text });
        return;
      case 'preview': {
        const snap = this.snap!;
        let headline: Headline = null;
        try {
          headline = computeHeadline(ev.spec, ev.columns, ev.rows);
        } catch (e) {
          if (!(e instanceof ContractError)) throw e;
        }
        const preview: Preview = {
          request_id: requestId, spec: ev.spec, display: displayToJson(ev.spec.display), columns: ev.columns, rows: ev.rows,
          caveats: effectiveCaveats(ev.spec, ev.columns, ev.rows), headline,
          preview_hash: semanticHash(ev.spec, snap.paramsHash), snapshot_id: snap.id, as_of: snap.asOf, plan: this.lastPlan, versions: this.ctx!, tables: ev.tables,
        };
        if (ev.spec.metric === null) this.record({ type: 'offdict_approved', request_id: requestId, preview_hash: preview.preview_hash });
        this.preview = preview;
        this.previewAgentRows = ev.agentRows;
        this.record({ type: 'preview', request_id: requestId, preview, agent_rows: ev.agentRows });
        this.push('preview', requestId, turnNo, { preview });
        return;
      }
      case 'offdict': {
        const view = this.offdictView(ev);
        this.record({ type: 'offdict', request_id: requestId, spec: ev.spec, tables: ev.tables });
        this.push('offdict', requestId, turnNo, view);
        return;
      }
      case 'offdict_answer':
        if (!ev.approve) this.record({ type: 'offdict_rejected', request_id: requestId });
        this.push('offdict_answer', requestId, turnNo, { approve: ev.approve });
        return;
      case 'refused':
        this.record({ type: 'refused', request_id: requestId, reason: ev.reason, alternatives: ev.alternatives });
        this.push('refused', requestId, turnNo, { reason: ev.reason, alternatives: ev.alternatives });
        return;
      case 'failed':
        if (ev.reason === 'sensitive') {
          // Drop the agent session (the next request starts a new one)
          this.session = null;
          this.record({ type: 'session_discarded', request_id: requestId });
          this.app.audit?.tryWrite({ event: 'sensitive_blocked', user: this.owner ?? undefined, target: this.id });
        }
        this.record({ type: 'failed', request_id: requestId, reason: ev.reason, message: ev.message });
        this.push('failed', requestId, turnNo, { reason: ev.reason, message: ev.message });
        return;
      case 'done':
        this.record({ type: 'done', request_id: requestId });
        this.push('done', requestId, turnNo, {});
        return;
      case 'cancelled':
        return;
      case 'step':
        this.push('step', requestId, turnNo, { kind: ev.kind, text: ev.text, ms: ev.ms ?? null, rows: ev.rows ?? null });
        return;
    }
  }
}

export class ConversationHub {
  private readonly app: App;
  private readonly dir: string;
  private readonly items = new Map<string, Conversation>();

  constructor(app: App) {
    this.app = app;
    this.dir = join(app.ws.config.outDir, 'conversations');
    mkdirSync(this.dir, { recursive: true });
  }

  create(owner: string): Conversation {
    const id = newId();
    const c = new Conversation(this.app, id, this.dir);
    c.setOwner(owner);
    this.items.set(id, c);
    return c;
  }

  get(id: string): Conversation | null {
    if (!ID_RE.test(id)) return null;
    const hit = this.items.get(id);
    if (hit) return hit;
    if (!existsSync(join(this.dir, `${id}.jsonl`))) return null;
    const c = new Conversation(this.app, id, this.dir);
    c.load();
    this.items.set(id, c);
    return c;
  }

  /** Latest conversation for this user to continue: has input and is not locked (checks the last 200 files) */
  latestFor(user: { username: string; role: string }): string | null {
    const files = readdirSync(this.dir)
      .filter((f) => f.endsWith('.jsonl') && ID_RE.test(f.slice(0, -6)))
      .map((f) => ({ id: f.slice(0, -6), mtime: statSync(join(this.dir, f)).mtimeMs }))
      .sort((a, b) => b.mtime - a.mtime)
      .slice(0, 200);
    for (const { id } of files) {
      const c = this.get(id);
      if (!c || !(c.owner === user.username || (c.owner === null && user.role === 'admin'))) continue;
      if (c.hasInput() && !c.isLocked()) return id;
    }
    return null;
  }

  async stopAll(): Promise<void> {
    await Promise.all([...this.items.values()].map((c) => c.stop()));
  }
}
