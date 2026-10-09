// State machine for one request (one user input). A turn is one agent call.
import type { PanelRunResult } from '../panels/run.ts';
import type { PanelSpec } from '../panels/spec.ts';
import type { QueryResult } from '../query/executor.ts';
import type { ResultColumn } from '../query/worker.ts';
import type { Row } from '../panels/contract.ts';
import { ActionError, parseAction, type Action, type AskQuestion } from './actions.ts';
import type { CallResult } from './runner.ts';
import { OutboundBlocked, type Outbound } from './outbound.ts';
import { sensitiveMessage } from '../query/worker.ts';
import { secretAssignment, secretShape } from './sensitive.ts';
import { tr } from '../i18n.ts';

export type RequestState = 'calling' | 'querying' | 'checking' | 'waiting_user' | 'done' | 'failed' | 'cancelling' | 'cancelled';

export type FailReason =
  | 'turn_limit' | 'probe_limit' | 'fix_limit' | 'request_budget' | 'call_budget' | 'isolation'
  | 'timeout' | 'process' | 'rate_limit' | 'resume' | 'schema' | 'error' | 'outbound_blocked' | 'server_restart' | 'sensitive';

export type LoopEvent =
  | { type: 'request_started' }
  | { type: 'session'; sessionId: string }
  | { type: 'step'; kind: 'calling' | 'retry' | 'querying' | 'query_done' | 'checking' | 'check_failed'; text: string; ms?: number; rows?: number }
  | { type: 'plan'; text: string }
  | { type: 'question'; questions: AskQuestion[] }
  | { type: 'answers'; answers: { id: string; answer: string; defaulted: boolean }[] }
  | { type: 'preview'; spec: PanelSpec; columns: ResultColumn[]; rows: Row[]; agentRows: Row[]; tables: string[] }
  | { type: 'offdict'; spec: PanelSpec; columns: ResultColumn[]; rows: Row[]; tables: string[] }
  | { type: 'offdict_answer'; approve: boolean }
  | { type: 'refused'; reason: string; alternatives: string[] }
  | { type: 'done' }
  | { type: 'failed'; reason: FailReason; message: string }
  | { type: 'cancelled' };

export type Limits = { maxTurns: number; maxProbes: number; maxFixes: number; callBudgetUsd: number; requestBudgetUsd: number };

export type LoopDeps = {
  call(o: { input: string; sessionId: string | null; budgetUsd: number; signal: AbortSignal }): Promise<CallResult>;
  probe(sql: string, signal: AbortSignal): Promise<QueryResult>;
  panel(spec: PanelSpec, signal: AbortSignal): Promise<PanelRunResult>;
  sleep(ms: number, signal: AbortSignal): Promise<void>;
  outbound: Outbound;
  limits: Limits;
  emit(ev: LoopEvent, turnNo: number): void;
  onIsolationFailure(): void;
  /** Progress heartbeat interval (ms) */
  heartbeatMs?: number;
};

export type StartInput = {
  text: string;
  current: PanelSpec | null;
  resumeSessionId: string | null;
  /** Summary of the earlier conversation for the first turn of a new session */
  recovery: string | null;
};

export const MIN_CALL_BUDGET = 0.02;
const RATE_WAITS = [10_000, 30_000];

class Stop extends Error {}

/** True when there are rows, at least one number column, and every number is 0 (NULL counts as 0) */
export function allZero(rows: Row[]): boolean {
  let numbers = 0;
  for (const row of rows) for (const v of row) {
    if (typeof v !== 'number') continue;
    numbers++;
    if (v !== 0) return false;
  }
  return rows.length > 0 && numbers > 0;
}

export class AgentRequest {
  readonly id: string;
  state: RequestState = 'calling';
  turnNo = 0;
  counts = { turns: 0, probes: 0, fixes: 0 };
  costTotal = 0;
  sessionId: string | null = null;
  pendingAsk: { questions: AskQuestion[]; turnNo: number } | null = null;
  /** Waiting for approval of an off-dictionary panel */
  pendingOffdict: { spec: PanelSpec; columns: ResultColumn[]; rows: Row[]; agentRows: Row[]; tables: string[]; turnNo: number } | null = null;

  private readonly deps: LoopDeps;
  private readonly ac = new AbortController();
  /** A fix is counted when the next call starts */
  private fixPending = false;
  /** The all-zero warning is sent once per request */
  private zeroWarned = false;
  private work: Promise<void> = Promise.resolve();
  private start: StartInput | null = null;

  constructor(id: string, deps: LoopDeps) {
    this.id = id;
    this.deps = deps;
  }

  get finished(): boolean {
    return this.state === 'done' || this.state === 'failed' || this.state === 'cancelled';
  }

  run(start: StartInput): Promise<void> {
    this.start = start;
    this.sessionId = start.resumeSessionId;
    this.emit({ type: 'request_started' });
    this.work = this.guard(async () => {
      const input = this.deps.outbound.request(start.text, start.current, start.resumeSessionId ? null : start.recovery);
      await this.drive(input);
    });
    return this.work;
  }

  /** Accepted only for the current question's turnNo */
  answer(turnNo: number, given: Record<string, string | undefined>): Promise<void> | null {
    if (this.state !== 'waiting_user' || !this.pendingAsk || this.pendingAsk.turnNo !== turnNo) return null;
    const ask = this.pendingAsk;
    this.pendingAsk = null;
    this.state = 'calling';
    this.work = this.guard(async () => {
      const input = this.deps.outbound.answers(ask.questions, given);
      this.emit({
        type: 'answers',
        answers: ask.questions.map((q) => {
          const a = given[q.id];
          return a === undefined || a.trim() === ''
            ? { id: q.id, answer: q.options.find((o) => o.is_default)!.label, defaulted: true }
            : { id: q.id, answer: a, defaulted: false };
        }),
      });
      await this.drive(input);
    });
    return this.work;
  }

  /** Off-dictionary panel: approve to preview it, reject to have it rebuilt from a dictionary metric */
  approveOffdict(turnNo: number, approve: boolean): Promise<void> | null {
    const p = this.pendingOffdict;
    if (this.state !== 'waiting_user' || !p || p.turnNo !== turnNo) return null;
    this.pendingOffdict = null;
    this.emit({ type: 'offdict_answer', approve });
    if (approve) {
      this.state = 'done';
      this.emit({ type: 'preview', spec: p.spec, columns: p.columns, rows: p.rows, agentRows: p.agentRows, tables: p.tables });
      this.emit({ type: 'done' });
      return Promise.resolve();
    }
    this.state = 'calling';
    this.work = this.guard(() => this.drive(this.deps.outbound.offdictRejected()));
    return this.work;
  }

  async cancel(): Promise<void> {
    if (this.finished || this.state === 'cancelling') return this.work;
    const wasWaiting = this.state === 'waiting_user';
    this.state = 'cancelling';
    this.ac.abort();
    if (!wasWaiting) await this.work;
    this.state = 'cancelled';
    this.pendingAsk = null;
    this.pendingOffdict = null;
    this.emit({ type: 'cancelled' });
  }

  private emit(ev: LoopEvent): void {
    this.deps.emit(ev, this.turnNo);
  }

  private get cancelled(): boolean {
    return this.ac.signal.aborted;
  }

  private fail(reason: FailReason, message: string): never {
    this.state = 'failed';
    this.emit({ type: 'failed', reason, message });
    throw new Stop();
  }

  private async guard(fn: () => Promise<void>): Promise<void> {
    try {
      await fn();
    } catch (e) {
      if (e instanceof Stop) return;
      if (e instanceof OutboundBlocked) {
        this.state = 'failed';
        this.emit({ type: 'failed', reason: 'outbound_blocked', message: e.message });
        return;
      }
      if (this.cancelled) return;
      this.state = 'failed';
      this.emit({ type: 'failed', reason: 'error', message: (e as Error).message });
    }
  }

  private async nextAction(firstInput: string): Promise<Action> {
    const L = this.deps.limits;
    let input = firstInput;
    let processRetried = false;
    let rateRetries = 0;
    let schemaRetried = false;
    let resumeRecovered = false;
    for (;;) {
      if (this.cancelled) throw new Stop();
      if (this.counts.turns >= L.maxTurns) this.fail('turn_limit', tr(`Reached the call limit for one request (${L.maxTurns})`, `한 요청의 호출 한도(${L.maxTurns}회)에 도달했어요`));
      const remaining = L.requestBudgetUsd - this.costTotal;
      if (remaining < MIN_CALL_BUDGET) this.fail('request_budget', tr('Reached the request budget', '요청 한도에 도달했어요'));
      this.state = 'calling';
      if (this.fixPending) {
        this.fixPending = false;
        this.counts.fixes++;
      }
      this.emit({ type: 'step', kind: 'calling', text: tr('The agent is thinking', '에이전트가 생각하는 중') });
      const res = await this.deps.call({ input, sessionId: this.sessionId, budgetUsd: Math.min(L.callBudgetUsd, remaining), signal: this.ac.signal });
      this.counts.turns++;
      this.turnNo++;
      this.costTotal += res.costUsd;
      if (this.cancelled) throw new Stop();

      if (!res.ok) {
        if (res.sessionId && (res.type === 'rate_limit' || res.type === 'error') && res.sessionId !== this.sessionId) {
          this.sessionId = res.sessionId;
          this.emit({ type: 'session', sessionId: res.sessionId });
        }
        switch (res.type) {
          case 'cancelled':
            throw new Stop();
          case 'isolation':
            this.deps.onIsolationFailure();
            this.fail('isolation', tr('Agent isolation check failed: the agent has been turned off', '에이전트 격리 점검 실패: 에이전트 기능을 껐어요'));
          case 'budget':
            this.fail('call_budget', tr('Reached the call budget', '호출 한도에 도달했어요'));
          case 'rate_limit':
            if (rateRetries < RATE_WAITS.length) {
              const wait = RATE_WAITS[rateRetries++];
              this.emit({ type: 'step', kind: 'retry', text: tr(`The API is busy. Retrying in ${wait / 1000}s`, `API가 바빠요. ${wait / 1000}초 뒤 다시 시도`) });
              await this.deps.sleep(wait, this.ac.signal);
              continue;
            }
            this.fail('rate_limit', tr('The API is still rate-limited or overloaded. Please try again later', 'API 제한·과부하가 계속돼요. 잠시 뒤 다시 시도해 주세요'));
          case 'resume':
            if (!resumeRecovered && this.start) {
              resumeRecovered = true;
              this.sessionId = null;
              input = this.deps.outbound.request(this.start.text, this.start.current, this.start.recovery);
              this.emit({ type: 'step', kind: 'retry', text: tr('Could not resume the earlier session, continuing in a new one', '이전 세션을 이어갈 수 없어 새 세션으로 이어가요') });
              continue;
            }
            this.fail('resume', tr('Could not resume the session', '세션을 이어갈 수 없어요'));
          default:
            if (!processRetried) {
              processRetried = true;
              this.emit({ type: 'step', kind: 'retry', text: tr(`Call failed (${res.message}), retrying`, `호출 실패(${res.message}), 다시 시도`) });
              continue;
            }
            this.fail(res.type === 'timeout' ? 'timeout' : res.type === 'error' ? 'error' : 'process', res.message);
        }
      }

      if (res.sessionId !== this.sessionId) {
        this.sessionId = res.sessionId;
        this.emit({ type: 'session', sessionId: res.sessionId });
      }
      // If agent output (SQL included) contains a secret-looking value, drop it and stop before parsing
      const raw = JSON.stringify(res.structured ?? null);
      if (secretShape(raw) || secretAssignment(raw)) this.fail('sensitive', sensitiveMessage());
      try {
        return parseAction(res.structured);
      } catch (e) {
        if (!(e instanceof ActionError)) throw e;
        if (schemaRetried) this.fail('schema', tr(`The reply does not match the action schema: ${e.message}`, `응답이 행동 스키마에 맞지 않아요: ${e.message}`));
        schemaRetried = true;
        input = this.deps.outbound.schemaMismatch(e.message);
      }
    }
  }

  private async withHeartbeat<T>(kind: 'querying' | 'checking', label: string, fn: () => Promise<T>): Promise<T> {
    const t0 = Date.now();
    this.emit({ type: 'step', kind, text: label });
    const timer = setInterval(() => this.emit({ type: 'step', kind, text: `${label} (${Math.round((Date.now() - t0) / 1000)}${tr('s', '초')})`, ms: Date.now() - t0 }), this.deps.heartbeatMs ?? 5000);
    try {
      return await fn();
    } finally {
      clearInterval(timer);
    }
  }

  private async drive(firstInput: string): Promise<void> {
    const L = this.deps.limits;
    let input = firstInput;
    for (;;) {
      const action = await this.nextAction(input);
      if (this.cancelled) throw new Stop();
      switch (action.action) {
        case 'ask':
          this.pendingAsk = { questions: action.questions, turnNo: this.turnNo };
          this.state = 'waiting_user';
          this.emit({ type: 'question', questions: action.questions });
          return;
        case 'refuse':
          this.state = 'done';
          this.emit({ type: 'refused', reason: action.reason, alternatives: action.alternatives });
          this.emit({ type: 'done' });
          return;
        case 'probe': {
          this.emit({ type: 'plan', text: action.plan });
          if (this.counts.probes >= L.maxProbes) this.fail('probe_limit', tr(`Reached the probe limit (${L.maxProbes})`, `탐색 한도(${L.maxProbes}회)에 도달했어요`));
          this.counts.probes++; // failures count too
          this.state = 'querying';
          const r = await this.withHeartbeat('querying', tr(`Running probe query: ${action.purpose}`, `탐색 쿼리 실행: ${action.purpose}`), () => this.deps.probe(action.sql, this.ac.signal));
          if (this.cancelled) throw new Stop();
          // An attempt to read sensitive data ends the request without returning anything to the agent
          if (!r.ok && r.kind === 'sensitive') this.fail('sensitive', sensitiveMessage());
          this.emit(r.ok
            ? { type: 'step', kind: 'query_done', text: `${action.purpose}: ${r.rows.length}${r.more ? '+' : ''} ${tr('rows', '행')}`, ms: r.ms, rows: r.rows.length }
            : { type: 'step', kind: 'query_done', text: `${action.purpose}: ${tr('failed', '실패')} (${r.message})` });
          input = this.deps.outbound.probeResult(r, L.maxProbes - this.counts.probes);
          break;
        }
        case 'panel': {
          this.emit({ type: 'plan', text: action.plan });
          this.state = 'checking';
          const r = await this.withHeartbeat('checking', tr('Checking and running the panel', '패널 검사·실행'), () => this.deps.panel(action.panel, this.ac.signal));
          if (this.cancelled) throw new Stop();
          if (r.ok && r.real.some((row) => row.some((v) => typeof v === 'string' && (secretShape(v) || secretAssignment(v))))) this.fail('sensitive', sensitiveMessage());
          // All-zero/NULL numbers usually mean a join or format mistake. Ask once to check; accept the same result if sent again
          if (r.ok && !this.zeroWarned && allZero(r.real)) {
            this.zeroWarned = true;
            this.emit({ type: 'step', kind: 'check_failed', text: tr('All result numbers are 0 → asking to check the conditions', '결과 숫자가 모두 0 → 조건 확인 요청') });
            input = this.deps.outbound.zeroResult();
            break;
          }
          if (r.ok) {
            if (action.panel.metric === null) {
              this.pendingOffdict = { spec: action.panel, columns: r.columns, rows: r.real, agentRows: r.agent, tables: r.tables, turnNo: this.turnNo };
              this.state = 'waiting_user';
              this.emit({ type: 'offdict', spec: action.panel, columns: r.columns, rows: r.real, tables: r.tables });
              return;
            }
            this.state = 'done';
            this.emit({ type: 'preview', spec: action.panel, columns: r.columns, rows: r.real, agentRows: r.agent, tables: r.tables });
            this.emit({ type: 'done' });
            return;
          }
          if (r.stage === 'cancelled') throw new Stop();
          if (r.stage === 'sensitive') this.fail('sensitive', sensitiveMessage());
          this.emit({ type: 'step', kind: 'check_failed', text: tr(`Panel check failed (${r.stage}) → fixing`, `패널 검사 실패(${r.stage}) → 수정 중`) });
          if (this.counts.fixes >= L.maxFixes) this.fail('fix_limit', tr(`Reached the panel fix limit (${L.maxFixes}): ${r.message}`, `패널 수정 한도(${L.maxFixes}회)에 도달했어요: ${r.message}`));
          this.fixPending = true;
          input = this.deps.outbound.panelFailure(r, L.maxFixes - this.counts.fixes - 1);
          break;
        }
      }
    }
  }
}
