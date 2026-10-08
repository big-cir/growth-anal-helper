// 대화: 요청 시작·취소, SSE 이벤트, 대화 기록(jsonl), 세션 이어가기.
import { appendFileSync, existsSync, mkdirSync, readFileSync } from 'node:fs';
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
  /** 패널 SQL이 참조한 표 */
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
  /** 진행 중이거나 답을 기다리는 요청 */
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
  /** 현재 미리보기의 가명 사본 결과(긴 설명 입력) */
  private previewAgentRows: Row[] | null = null;
  /** [에이전트로 재생성]으로 만든 대화면 원래 패널 */
  origin: { panel_id: string; prompt: string } | null = null;
  /** 만든 사용자. 이전 기록에는 없음 */
  owner: string | null = null;
  drafting = false;
  /** 민감 주제 질문으로 잠긴 대화(새 대화를 열어야 함) */
  locked = false;
  /** 재시작으로 멈춘 요청(새 요청이 시작되기 전까지 화면에 알린다) */
  private restartFailure: { request_id: string; reason: string; message: string } | null = null;

  constructor(app: App, id: string, dir: string) {
    this.app = app;
    this.id = id;
    this.file = join(dir, `${id}.jsonl`);
  }

  /** 대화 기록에서 복구 */
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
      const message = '서버가 다시 시작되어 진행 중이던 요청을 멈췄어요';
      this.record({ type: 'failed', request_id: failedRequest, reason: 'server_restart', message });
      this.restartFailure = { request_id: failedRequest, reason: 'server_restart', message };
    }
  }

  private record(e: Record<string, unknown>): void {
    appendFileSync(this.file, JSON.stringify({ t: new Date().toISOString(), ...e }) + '\n');
  }

  setOwner(user: string): void {
    this.owner = user;
    this.record({ type: 'owner', user });
  }

  setOrigin(panelId: string, prompt: string): void {
    this.origin = { panel_id: panelId, prompt };
    this.record({ type: 'origin', panel_id: panelId, prompt });
  }

  /** 저장할 패널의 처음 요청 문구 */
  firstPrompt(): string {
    return this.origin?.prompt ?? this.inputs[0] ?? '';
  }

  /** 요청 ID와 의미 해시가 현재 완료 미리보기와 같을 때만 */
  completedPreview(requestId: string, previewHash: string): { preview: Preview; agentRows: Row[] | null } | null {
    if (this.locked) return null;
    const p = this.preview;
    if (!p || p.request_id !== requestId || p.preview_hash !== previewHash) return null;
    // 뒤이은 요청이 진행 중이거나 답을 기다리면 받지 않는다
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

  /** 이 대화의 모든 입력을 합쳐서도 본다(여러 번에 나눠 묻기) */
  private sensitive(text: string): boolean {
    return sensitiveTopic(text) !== null || sensitiveTopic([...this.inputs, ...Object.values(this.lastAsk?.answers ?? {}), text].join(' ')) !== null;
  }

  /** 에이전트를 부르지 않고 거절한 뒤 대화를 잠근다. 입력 원문은 기록하지 않는다 */
  private refuseSensitive(id: string): void {
    this.locked = true;
    this.session = null;
    this.preview = null;
    this.previewAgentRows = null;
    this.record({ type: 'locked', request_id: id });
    this.app.audit?.tryWrite({ event: 'sensitive_blocked', user: this.owner ?? undefined, target: this.id });
    this.push('user_input', id, 0, { text: '(기록하지 않은 요청)' });
    this.push('refused', id, 0, { reason: '이 도구는 인증 정보·토큰·연결 정보·설정·개인 연락처를 다루지 않아요. 이 대화는 잠겼어요. 새 대화에서 지표로 물어봐 주세요.', alternatives: [] });
    this.push('done', id, 0, {});
  }

  /** 진행 중 요청이 있으면 취소한 뒤 시작한다. 대기는 마지막 입력 하나만 */
  submit(text: string): string {
    const id = newId();
    if (this.locked || this.sensitive(text)) {
      const wasLocked = this.locked;
      this.queued = null;
      const finish = () => {
        if (!wasLocked) return this.refuseSensitive(id);
        this.push('user_input', id, 0, { text: '(기록하지 않은 요청)' });
        this.push('refused', id, 0, { reason: '이 대화는 잠겼어요. 새 대화를 열어 주세요.', alternatives: [] });
        this.push('done', id, 0, {});
      };
      if (this.active) void this.cancelActive('이전 요청을 취소했습니다').then(finish);
      else finish();
      return id;
    }
    if (this.active) {
      this.queued = { id, text };
      this.push('queued', id, 0, { text });
      void this.cancelActive('이전 요청을 취소했습니다').then(() => {
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
    await this.cancelActive('요청을 멈췄어요');
    return true;
  }

  answer(requestId: string, turnNo: number, answers: Record<string, string | undefined>): boolean {
    const a = this.active;
    if (!a || a.id !== requestId || a.cancelled || !a.req) return false;
    const free = Object.values(answers).filter((v): v is string => typeof v === 'string').join(' ');
    if (free && this.sensitive(free)) {
      void this.cancelActive('요청을 멈췄어요').then(() => this.refuseSensitive(newId()));
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

  /** 승인 전에는 정의와 참조한 표만 보낸다(결과는 서버에만) */
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
    if (!snap) return fail('스냅샷이 없어요. 먼저 collect를 실행해 주세요');
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
      // 상세(파일·칸 이름)는 서버 로그에만
      console.error(`컨텍스트 조립 실패: ${(e as Error).message}`);
      return fail('에이전트 설정(설명서·지표 사전·예시 패널)에 문제가 있어요. 관리자에게 알려 주세요');
    }
    const cfg = app.ws.config;
    const outbound = new Outbound(cfg.agent.dataMode, app.roles);
    const resume = this.session && this.session.contextVersion === ctxKey ? this.session.id : null;
    const hadHistory = this.inputs.length > 1 || this.preview !== null;
    const recovery = hadHistory ? outbound.recoverySummary({ inputs: this.inputs.slice(0, -1), lastAsk: this.lastAsk, current: this.preview?.spec ?? null }) : null;
    if (this.session && !resume) this.push('step', requestId, 0, { kind: 'retry', text: '데이터나 설명서가 바뀌어 새 세션으로 이어갑니다' });

    const paths = { real: snap.real, agent: snap.agent };
    const blocked = app.blocked();
    const req = new AgentRequest(requestId, {
      call: async ({ input, sessionId, budgetUsd, signal }) => {
        try {
          return await app.slots.run('interactive', signal, () => app.runner.call({
            input, sessionId, budgetUsd, signal, systemPrompt, jsonSchema: ACTION_SCHEMA_ARG, model: cfg.agent.model, timeoutMs: cfg.agent.callTimeoutMs,
          }));
        } catch (e) {
          if (e instanceof SlotCancelled) return { ok: false, type: 'cancelled', message: '취소됨', sessionId: null, costUsd: 0, ms: 0 };
          throw e;
        }
      },
      probe: async (sql, signal) => {
        try {
          return await app.slots.run('interactive', signal, (lease) => runQuery({
            lease, sql, path: paths.agent, mode: 'probe', asOf: snap.asOf, params: cfg.params, readablePrefixes: cfg.policy.readablePrefixes, blocked, heapLimitMb: cfg.run.heapLimitMb, signal,
          }));
        } catch (e) {
          if (e instanceof SlotCancelled) return { ok: false, kind: 'cancelled', message: '취소됨' };
          throw e;
        }
      },
      panel: async (spec, signal) => {
        try {
          return await app.slots.run('interactive', signal, (lease) => runPanel({
            spec, paths, asOf: snap.asOf, params: cfg.params, policy: cfg.policy, metrics, blocked, heapLimitMb: cfg.run.heapLimitMb, roles: app.roles, lease, signal,
          }));
        } catch (e) {
          if (e instanceof SlotCancelled) return { ok: false, stage: 'cancelled', message: '취소됨' };
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
      onIsolationFailure: () => app.disableAgent('에이전트 격리 점검 실패: 서버를 다시 시작하기 전까지 에이전트를 쓸 수 없어요'),
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
          // 에이전트 세션을 버린다(다음 요청은 새 세션)
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

  async stopAll(): Promise<void> {
    await Promise.all([...this.items.values()].map((c) => c.stop()));
  }
}
