// Runs a panel on the real copy and the pseudonymized copy, then checks the contract, invariants and that both results agree.
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
  /** Panels read only panelReadablePrefixes tables */
  policy: { panelReadablePrefixes: string[] };
  metrics: MetricDict;
  /** Columns and tables to refuse (private columns, engine tables) */
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

/** Equal ignoring row order */
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
    const hint = /^[^:]*(unreadable|not readable|cannot read|can't read)[^:]*:/i.test(real.message) ? ` (panels can only read panel tables ${o.policy.panelReadablePrefixes.map((p) => `${p}*`).join(', ')})` : '';
    return { ok: false, stage: real.kind === 'lint' ? 'lint' : 'exec', message: real.message + hint };
  }
  if (o.spec.metric !== null) {
    const problem = metricTablesProblem(o.metrics, o.spec.metric, real.tables);
    if (problem) return { ok: false, stage: 'metric_tables', message: problem, tables: real.tables };
  }
  const ids = directIdentifierColumns(real.columns, o.roles);
  if (ids.length) {
    return { ok: false, stage: 'id_column', message: `ID columns cannot be in a panel result: ${ids.join(', ')} (use aggregates such as counts or rates)` };
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

  if (agent.tables.join(',') !== real.tables.join(',')) return { ok: false, stage: 'exec', message: 'the real and pseudonymized copies read different tables (their schemas differ)' };
  const agentRows = untagRows(agent.rows);
  const realRows = untagRows(real.rows);
  const v = checkInvariants(o.spec, agent.columns, agentRows);
  if (v.length) return { ok: false, stage: 'invariant', message: `invariant violations:\n${formatViolations(v)}`, violations: v };
  if (checkInvariants(o.spec, real.columns, realRows).length) {
    return { ok: false, stage: 'id_dependent', message: 'panel depends on ID values: do not depend on the size, range or order of IDs' };
  }

  if (!sameResult(real.rows, agent.rows)) {
    return { ok: false, stage: 'id_dependent', message: 'panel depends on ID values: the real and pseudonymized results differ. Do not depend on the size, range or order of IDs' };
  }
  return { ok: true, columns: real.columns, real: realRows, agent: agentRows, ms: Math.round(performance.now() - t0), tables: real.tables };
}
