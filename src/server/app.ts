// 서버 상태: 워크스페이스, 스냅샷, 시스템 프롬프트, 실행 슬롯, 에이전트 상태.
import { DatabaseSync } from 'node:sqlite';
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Workspace } from '../workspace.ts';
import type { Role } from '../collect/spec.ts';
import { allRoles, loadDerivedRoles } from '../collect/finalize.ts';
import { loadSpec } from '../collect/spec.ts';
import { readCurrent, snapshotsDir } from '../snapshot/store.ts';
import { parsePanelSpec } from '../panels/spec.ts';
import { docsVersion, policyVersion, promptVersion, schemaVersion, contextVersionKey, RENDERER_VERSION, type ContextVersion } from '../panels/hash.ts';
import { PATTERN_CONTRACT_VERSION } from '../panels/contract.ts';
import type { PanelVersions } from '../panels/store.ts';
import { ExecutionSlots } from '../query/executor.ts';
import { ClaudeRunner } from '../agent/claude.ts';
import { ACTION_SCHEMA_ARG, ACTION_SCHEMA_TEXT } from '../agent/actions.ts';
import { buildContext, ContextError, ENGINE_GUIDE, snapshotSchema, type SeedPanel } from '../agent/context.ts';
import { loadMetrics, metricsContext, type MetricDict } from '../panels/metrics.ts';
import { AGENT_EXCLUDED_TABLES } from '../snapshot/pseudonymize.ts';
import { secretAssignment, secretShape } from '../agent/sensitive.ts';
import type { AuditLog } from '../auth/audit.ts';
import { Outbound } from '../agent/outbound.ts';
import { summarize, SUMMARY_GUIDE, SUMMARY_SCHEMA_ARG, type SummaryOutcome } from '../agent/summarize.ts';
import type { PanelSpec } from '../panels/spec.ts';
import type { Row } from '../panels/contract.ts';
import type { ResultColumn } from '../query/worker.ts';

export type Snapshot = {
  id: string;
  real: string;
  agent: string;
  asOf: string;
  rawHash: string;
  derivedHash: string;
  rolesHash: string;
  paramsHash: string;
};

export type AgentStatus = { state: 'checking' | 'ok' | 'failed' | 'disabled'; message: string };

export class AppError extends Error {}

function readSnapshot(dir: string): Snapshot | null {
  const cur = readCurrent(dir);
  if (!cur || !existsSync(cur.file) || !existsSync(cur.agent_file)) return null;
  const db = new DatabaseSync(cur.file, { readOnly: true });
  try {
    const m = db.prepare('SELECT snapshot_id, source_cutoff_at, raw_hash, derived_hash, roles_hash, params_hash FROM snapshot_meta').get() as Record<string, string>;
    return {
      id: m.snapshot_id, real: cur.file, agent: cur.agent_file, asOf: m.source_cutoff_at,
      rawHash: m.raw_hash, derivedHash: m.derived_hash, rolesHash: m.roles_hash, paramsHash: m.params_hash,
    };
  } finally {
    db.close();
  }
}

export function loadSeedPanels(wsDir: string): { panels: SeedPanel[]; files: { name: string; text: string }[] } {
  const dir = join(wsDir, 'seed-panels');
  if (!existsSync(dir)) return { panels: [], files: [] };
  const files = readdirSync(dir).filter((f) => f.endsWith('.json')).sort().map((name) => ({ name, text: readFileSync(join(dir, name), 'utf8') }));
  const panels = files.map((f) => {
    const raw = JSON.parse(f.text) as { id?: string };
    return { id: raw.id ?? f.name.replace(/\.json$/, ''), spec: parsePanelSpec(raw) };
  });
  return { panels, files };
}

export class App {
  readonly ws: Workspace;
  readonly slots: ExecutionSlots;
  readonly runner: ClaudeRunner;
  readonly roles: Map<string, Role>;
  agent: AgentStatus = { state: 'checking', message: '에이전트 격리 점검 중' };
  /** 시드 패널 검사에 실패해 컨텍스트에서 뺀 시드 */
  excludedSeeds = new Set<string>();
  private agentReady: Promise<void> = Promise.resolve();
  private contextCache: { key: string; text: string } | null = null;

  constructor(ws: Workspace) {
    this.ws = ws;
    this.slots = new ExecutionSlots(ws.config.agent.concurrency, 1);
    this.runner = new ClaudeRunner({ bin: ws.config.agent.bin, cwd: join(ws.config.outDir, '.agent-cwd'), logDir: join(ws.config.outDir, 'logs') });
    this.roles = allRoles(loadSpec(join(ws.dir, 'tables.json')), loadDerivedRoles(join(ws.dir, 'derived-columns.json')));
    this.checkContextAssets();
    const { dict } = this.metrics();
    const snap = this.snapshot();
    if (snap) {
      const pol = ws.config.policy;
      const have = snapshotSchema(snap.agent, pol.readablePrefixes, pol.panelReadablePrefixes, '').tables;
      const missing = [...new Set([...dict.dimension_tables, ...dict.metrics.flatMap((m) => m.tables)])].filter((t) => !have.includes(t));
      if (missing.length) throw new ContextError(`지표 사전의 표가 스냅샷에 없음: ${missing.join(', ')}`);
    }
  }

  /** 에이전트 쿼리에서 민감 거부할 칸(private)과 엔진 운영 표 */
  blocked(): { columns: string[]; tables: string[] } {
    return { columns: [...this.roles].filter(([, r]) => r === 'private').map(([k]) => k), tables: [...AGENT_EXCLUDED_TABLES] };
  }

  /**
   * 에이전트 컨텍스트에 들어가는 운영자 파일(설명서, 지표 사전, 시드 패널)에
   * private 칸 이름·엔진 운영 표 이름·비밀처럼 생긴 값·"키=비밀값"이 있으면 거부
   */
  checkContextAssets(): void {
    const files: { name: string; text: string }[] = [{ name: 'guide.md', text: this.guide() }];
    const metricsFile = join(this.ws.dir, 'metrics.json');
    if (existsSync(metricsFile)) files.push({ name: 'metrics.json', text: readFileSync(metricsFile, 'utf8') });
    for (const f of loadSeedPanels(this.ws.dir).files) files.push({ name: `seed-panels/${f.name}`, text: f.text });
    const privateCols = this.blocked().columns;
    const publicNames = new Set([...this.roles].filter(([, r]) => r !== 'private').map(([k]) => k.split('.')[1]));
    // 한정 이름(표.칸)과, 공개 칸과 겹치지 않는 비한정 이름
    const needles = [...privateCols, ...privateCols.map((c) => c.split('.')[1]).filter((n) => !publicNames.has(n))];
    const problems: string[] = [];
    for (const f of files) {
      const hitCol = needles.find((c) => new RegExp(`(^|[^A-Za-z0-9_])${c.replace('.', '\\.')}($|[^A-Za-z0-9_])`).test(f.text));
      if (hitCol) problems.push(`${f.name}: private 칸 ${hitCol}`);
      const hitTable = [...AGENT_EXCLUDED_TABLES].find((t) => new RegExp(`\\b${t}\\b`).test(f.text));
      if (hitTable) problems.push(`${f.name}: 엔진 운영 표 ${hitTable}`);
      const shape = secretShape(f.text);
      if (shape) problems.push(`${f.name}: 비밀처럼 생긴 값(${shape})`);
      if (secretAssignment(f.text)) problems.push(`${f.name}: 비밀 값 대입 형태`);
    }
    if (problems.length) throw new ContextError(`에이전트에게 줄 수 없는 내용이 있어요 — ${problems.join(' / ')}`);
  }

  /** 서버가 정한 감사 기록(없으면 기록하지 않음) */
  audit: AuditLog | null = null;

  /** 지표 사전. 읽을 때마다 검증한다 */
  metrics(): { dict: MetricDict; text: string } {
    const seeds = loadSeedPanels(this.ws.dir).panels.map((p) => ({ id: p.id, metric: p.spec.metric }));
    return loadMetrics(this.ws.dir, this.ws.config.policy.panelReadablePrefixes, seeds);
  }

  snapshot(): Snapshot | null {
    return readSnapshot(snapshotsDir(this.ws.config.outDir));
  }

  guide(): string {
    const p = join(this.ws.dir, 'guide.md');
    return existsSync(p) ? readFileSync(p, 'utf8') : '';
  }

  contextVersion(snap: Snapshot): ContextVersion {
    const seeds = loadSeedPanels(this.ws.dir);
    return {
      snapshot_id: snap.id,
      schema_version: schemaVersion(readFileSync(join(this.ws.dir, 'derived.sql'), 'utf8'), snap.rolesHash),
      policy_version: policyVersion(this.ws.config.policy.readablePrefixes, this.ws.config.policy.panelReadablePrefixes),
      docs_version: docsVersion(this.guide(), seeds.files, this.metrics().text),
      prompt_version: promptVersion(ENGINE_GUIDE, ACTION_SCHEMA_TEXT),
    };
  }

  panelVersions(snap: Snapshot): PanelVersions {
    return { ...this.contextVersion(snap), pattern_contract_version: PATTERN_CONTRACT_VERSION, renderer_version: RENDERER_VERSION };
  }

  systemPrompt(snap: Snapshot, ctx: ContextVersion): string {
    const excluded = [...this.excludedSeeds].sort();
    const key = `${contextVersionKey(ctx)}|${excluded.join(',')}`;
    if (this.contextCache?.key === key) return this.contextCache.text;
    const guide = this.guide();
    const p = this.ws.config.params;
    const pol = this.ws.config.policy;
    const today = new Date();
    const pad = (n: number) => String(n).padStart(2, '0');
    const { dict } = this.metrics();
    const schema = snapshotSchema(snap.agent, pol.readablePrefixes, pol.panelReadablePrefixes, guide);
    this.checkContextAssets();
    const missing = [...new Set([...dict.dimension_tables, ...dict.metrics.flatMap((m) => m.tables)])].filter((t) => !schema.tables.includes(t));
    if (missing.length) throw new ContextError(`지표 사전의 표가 스냅샷에 없음: ${missing.join(', ')}`);
    const text = buildContext({
      schema: schema.text,
      metrics: metricsContext(dict),
      guide,
      seedPanels: loadSeedPanels(this.ws.dir).panels.filter((s) => !this.excludedSeeds.has(s.id)),
      state: {
        asOf: snap.asOf,
        today: `${today.getFullYear()}-${pad(today.getMonth() + 1)}-${pad(today.getDate())}`,
        calendarStart: typeof p.calendar_start === 'string' ? p.calendar_start : null,
        params: p,
      },
    });
    this.contextCache = { key, text };
    return text;
  }

  /** 긴 설명 만들기(새 세션). 가명 사본 결과만 보낸다 */
  async summarize(spec: PanelSpec, columns: ResultColumn[], agentRows: Row[]): Promise<SummaryOutcome> {
    await this.agentReady;
    if (this.agent.state !== 'ok') return { status: 'failed', message: this.agent.message };
    const cfg = this.ws.config.agent;
    try {
      return await summarize({
        spec, columns, agentRows,
        outbound: new Outbound(cfg.dataMode, this.roles),
        call: ({ input, sessionId }) => this.slots.run('interactive', undefined, () => this.runner.call({
          input, sessionId, systemPrompt: SUMMARY_GUIDE, jsonSchema: SUMMARY_SCHEMA_ARG, budgetUsd: cfg.callBudgetUsd, model: cfg.model, timeoutMs: cfg.callTimeoutMs,
        })).then((r) => {
          if (!r.ok && r.type === 'isolation') this.disableAgent('에이전트 격리 점검 실패: 서버를 다시 시작하기 전까지 에이전트를 쓸 수 없어요');
          return r;
        }),
      });
    } catch (e) {
      return { status: 'failed', message: (e as Error).message };
    }
  }

  /** 서버 시작 시 에이전트 점검 호출 */
  startIsolationCheck(): Promise<void> {
    this.agentReady = (async () => {
      const r = await this.slots.run('interactive', undefined, () => this.runner.call({
        input: '격리 점검입니다. refuse 행동으로 reason에 "ok", alternatives는 빈 배열로 답하세요.',
        systemPrompt: '격리 점검용 호출입니다.',
        jsonSchema: ACTION_SCHEMA_ARG,
        budgetUsd: Math.min(0.2, this.ws.config.agent.callBudgetUsd),
        model: this.ws.config.agent.model,
        timeoutMs: this.ws.config.agent.callTimeoutMs,
      }));
      if (r.ok) this.agent = { state: 'ok', message: '에이전트 격리 점검 통과' };
      else if (r.type === 'isolation') this.agent = { state: 'failed', message: r.message };
      else this.agent = { state: 'failed', message: `에이전트를 시작하지 못함: ${r.message}` };
    })();
    return this.agentReady;
  }

  waitAgent(): Promise<void> {
    return this.agentReady;
  }

  disableAgent(message: string): void {
    this.agent = { state: 'disabled', message };
  }
}
