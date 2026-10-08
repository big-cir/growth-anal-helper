// 패널 실행: 원본과 가명 사본에서 차례로 실행하고 계약·불변식·결과 일치를 확인한다.
import type { Role } from '../collect/spec.ts';
import { runQuery, type SlotLease } from '../query/executor.ts';
import type { ResultColumn, Tagged } from '../query/worker.ts';
import { checkContract, checkInvariants, ContractError, formatViolations, untagRows, type Row, type Violation } from './contract.ts';
import type { PanelSpec } from './spec.ts';
import { metricTablesProblem, type MetricDict } from './metrics.ts';

export type PanelRunInput = {
  spec: PanelSpec;
  paths: { real: string; agent: string };
  asOf: string;
  params: Record<string, string | number | string[]>;
  /** 패널은 panelReadablePrefixes의 표만 읽는다 */
  policy: { panelReadablePrefixes: string[] };
  metrics: MetricDict;
  /** 민감 거부할 칸·표(private 칸, 엔진 운영 표) */
  blocked: { columns: string[]; tables: string[] };
  heapLimitMb: number;
  roles: Map<string, Role>;
  lease: SlotLease;
  signal?: AbortSignal;
};

export type PanelStage = 'lint' | 'exec' | 'contract' | 'invariant' | 'id_column' | 'id_dependent' | 'metric_tables' | 'sensitive' | 'cancelled';

export type PanelRunResult =
  | { ok: true; columns: ResultColumn[]; real: Row[]; agent: Row[]; ms: number; tables: string[] }
  | { ok: false; stage: PanelStage; message: string; violations?: Violation[]; tables?: string[] };

export function directIdentifierColumns(columns: ResultColumn[], roles: Map<string, Role>): string[] {
  return columns
    .filter((c) => c.table !== null && c.column !== null)
    .filter((c) => {
      const r = roles.get(`${c.table}.${c.column}`);
      return r !== undefined && r !== 'ordinary';
    })
    .map((c) => c.name);
}

const TAG_ORDER: Record<Tagged[0], number> = { n: 0, i: 1, f: 2, s: 3 };

function compareTagged(a: Tagged, b: Tagged): number {
  if (a[0] !== b[0]) return TAG_ORDER[a[0]] - TAG_ORDER[b[0]];
  if (a[0] === 'n') return 0;
  if (a[0] === 's') return a[1] < (b[1] as string) ? -1 : a[1] > (b[1] as string) ? 1 : 0;
  return (a[1] as number) - (b[1] as number);
}

function compareRow(a: Tagged[], b: Tagged[]): number {
  for (let i = 0; i < a.length; i++) {
    const c = compareTagged(a[i], b[i]);
    if (c !== 0) return c;
  }
  return 0;
}

/** 행 순서를 무시하고 같은지 */
export function sameResult(a: Tagged[][], b: Tagged[][]): boolean {
  if (a.length !== b.length) return false;
  const sa = [...a].sort(compareRow);
  const sb = [...b].sort(compareRow);
  return sa.every((row, i) => row.length === sb[i].length && compareRow(row, sb[i]) === 0);
}

export async function runPanel(o: PanelRunInput): Promise<PanelRunResult> {
  const t0 = performance.now();
  const base = { sql: o.spec.sql, mode: 'panel' as const, asOf: o.asOf, params: o.params, readablePrefixes: o.policy.panelReadablePrefixes, blocked: o.blocked, heapLimitMb: o.heapLimitMb, lease: o.lease, signal: o.signal };

  const real = await runQuery({ ...base, path: o.paths.real });
  if (!real.ok) {
    if (real.kind === 'cancelled') return { ok: false, stage: 'cancelled', message: real.message };
    if (real.kind === 'sensitive') return { ok: false, stage: 'sensitive', message: real.message };
    const hint = /^읽을 수 없는 표/.test(real.message) ? ` (패널은 패널용 표 ${o.policy.panelReadablePrefixes.map((p) => `${p}*`).join(', ')}만 읽을 수 있음)` : '';
    return { ok: false, stage: real.kind === 'lint' ? 'lint' : 'exec', message: real.message + hint };
  }
  if (o.spec.metric !== null) {
    const problem = metricTablesProblem(o.metrics, o.spec.metric, real.tables);
    if (problem) return { ok: false, stage: 'metric_tables', message: problem, tables: real.tables };
  }
  const ids = directIdentifierColumns(real.columns, o.roles);
  if (ids.length) {
    return { ok: false, stage: 'id_column', message: `ID 칸은 패널 결과에 넣을 수 없음: ${ids.join(', ')} (개수·비율 같은 집계로 바꾸세요)` };
  }
  try {
    checkContract(o.spec, real.columns);
  } catch (e) {
    if (e instanceof ContractError) return { ok: false, stage: 'contract', message: e.message };
    throw e;
  }

  const agent = await runQuery({ ...base, path: o.paths.agent });
  if (!agent.ok) {
    if (agent.kind === 'cancelled') return { ok: false, stage: 'cancelled', message: agent.message };
    if (agent.kind === 'sensitive') return { ok: false, stage: 'sensitive', message: agent.message };
    return { ok: false, stage: 'exec', message: agent.message };
  }

  if (agent.tables.join(',') !== real.tables.join(',')) return { ok: false, stage: 'exec', message: '원본과 가명 사본에서 참조한 표가 다름(스냅샷 사본의 스키마가 다름)' };
  const agentRows = untagRows(agent.rows);
  const realRows = untagRows(real.rows);
  const v = checkInvariants(o.spec, agent.columns, agentRows);
  if (v.length) return { ok: false, stage: 'invariant', message: `불변식 위반:\n${formatViolations(v)}`, violations: v };
  if (checkInvariants(o.spec, real.columns, realRows).length) {
    return { ok: false, stage: 'id_dependent', message: 'ID 값에 의존하는 패널: ID의 크기·범위·순서에 의존하지 마세요' };
  }

  if (!sameResult(real.rows, agent.rows)) {
    return { ok: false, stage: 'id_dependent', message: 'ID 값에 의존하는 패널: 원본과 가명 사본의 결과가 다름. ID의 크기·범위·순서에 의존하지 마세요' };
  }
  return { ok: true, columns: real.columns, real: realRows, agent: agentRows, ms: Math.round(performance.now() - t0), tables: real.tables };
}
