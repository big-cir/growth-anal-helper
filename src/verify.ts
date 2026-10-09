// Source cross-check: runs each query pair from verify.json on the source DB and the current snapshot and compares the values.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { allRoles } from './collect/finalize.ts';
import type { SourceAdapter } from './collect/sources/source.ts';
import { ExecutionSlots, runQuery } from './query/executor.ts';
import { bindSourceSql } from './query/sql-lint.ts';
import { loadBuildInputs, makeSource } from './snapshot/pipeline.ts';
import { readCurrent, snapshotsDir } from './snapshot/store.ts';
import { addDays, weekStart } from './time.ts';
import type { Workspace } from './workspace.ts';

export class VerifyError extends Error {}
export class VerifyCancelled extends Error {
  constructor() {
    super('cancelled');
  }
}

export type VerifyCheck = { id: string; title: string; source_sql: string; snapshot_sql: string };
export type VerifyItem = {
  id: string; title: string; ok: boolean;
  source: Record<string, string | null> | null;
  snapshot: Record<string, string | null> | null;
  error: string | null;
};
export type VerifyReport = { snapshot_id: string; as_of: string; week_start: string; week_end: string; items: VerifyItem[]; ok: boolean };

/** Byte limit for one row (column names + values) and column count limit */
const ROW_LIMIT = 4096;
const MAX_COLUMNS = 20;
const MAX_CHECKS = 20;

export function loadVerifyChecks(wsDir: string): VerifyCheck[] {
  const path = join(wsDir, 'verify.json');
  if (!existsSync(path)) throw new VerifyError('verify.json not found');
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(path, 'utf8'));
  } catch {
    throw new VerifyError('verify.json: invalid JSON');
  }
  const checks = (raw as { checks?: unknown })?.checks;
  if (!Array.isArray(checks) || checks.length === 0 || checks.length > MAX_CHECKS) throw new VerifyError(`verify.json: checks must be an array of 1-${MAX_CHECKS} items`);
  const ids = new Set<string>();
  return checks.map((c, i) => {
    const o = c as Record<string, unknown>;
    if (!o || typeof o !== 'object' || Array.isArray(o)) throw new VerifyError(`verify.json: checks[${i}] must be an object`);
    for (const k of Object.keys(o)) if (!['id', 'title', 'source_sql', 'snapshot_sql'].includes(k)) throw new VerifyError(`verify.json: checks[${i}] has unknown key ${k}`);
    if (typeof o.id !== 'string' || !/^[a-z][a-z0-9_]{0,47}$/.test(o.id) || ids.has(o.id)) throw new VerifyError(`verify.json: checks[${i}].id must be a unique lowercase identifier`);
    ids.add(o.id);
    if (typeof o.title !== 'string' || o.title.length === 0 || o.title.length > 80) throw new VerifyError(`verify.json: ${o.id}.title must be 1-80 characters`);
    for (const k of ['source_sql', 'snapshot_sql'] as const) {
      if (typeof o[k] !== 'string' || (o[k] as string).trim() === '') throw new VerifyError(`verify.json: ${o.id}.${k} is empty`);
    }
    return { id: o.id, title: o.title, source_sql: o.source_sql as string, snapshot_sql: o.snapshot_sql as string };
  });
}

/** Target week: the given Monday, or the Monday five weeks before the cutoff's week. The week must end before the cutoff */
export function verifyWeek(asOf: string, week?: string): { start: string; end: string } {
  let start: string;
  if (week !== undefined) {
    if (!/^\d{4}-\d{2}-\d{2}$/.test(week) || new Date(`${week}T00:00:00Z`).toISOString().slice(0, 10) !== week) throw new VerifyError('--week must be a real date YYYY-MM-DD');
    start = `${week} 00:00:00.000000`;
    if (weekStart(start) !== start) throw new VerifyError('--week must be a Monday');
  } else {
    start = addDays(weekStart(asOf), -35);
  }
  const end = addDays(start, 7);
  if (end > asOf) throw new VerifyError(`target week (${start.slice(0, 10)}) does not end before the cutoff`);
  return { start, end };
}

/** Normalizes a decimal string to (sign, digits, exponent). Null if it is not a number */
function decimalKey(v: string): string | null {
  const m = /^([+-]?)(\d+)(?:\.(\d+))?(?:e([+-]?\d+))?$/i.exec(v.trim());
  if (!m) return null;
  const frac = m[3] ?? '';
  let digits = (m[2] + frac).replace(/^0+/, '');
  if (digits === '') return '0';
  let exp = BigInt(m[4] ?? '0') - BigInt(frac.length);
  const trail = digits.length - digits.replace(/0+$/, '').length;
  digits = digits.slice(0, digits.length - trail);
  exp += BigInt(trail);
  return `${m[1] === '-' ? '-' : ''}${digits}e${exp}`;
}

/** Equal values: both NULL, exact numeric match if both are decimals, otherwise exact text */
export function sameValue(a: string | null, b: string | null): boolean {
  if (a === null || b === null) return a === b;
  const ka = decimalKey(a);
  const kb = decimalKey(b);
  if (ka !== null && kb !== null) return ka === kb;
  return a === b;
}

function compare(src: Record<string, string | null>, snap: Record<string, string | null>): string | null {
  const a = Object.keys(src);
  const b = Object.keys(snap);
  if (a.length !== b.length || a.some((k) => !(k in snap))) return `column names differ (source ${a.join(', ')} / snapshot ${b.join(', ')})`;
  const diff = a.filter((k) => !sameValue(src[k], snap[k]));
  return diff.length ? `different values: ${diff.join(', ')}` : null;
}

async function sourceRow(source: SourceAdapter, sql: string): Promise<Record<string, string | null>> {
  let columns: string[] = [];
  const rows: (string | null)[][] = [];
  await source.selectStream(sql, (v) => {
    if (rows.length >= 1) throw new Error('more than one row');
    rowProblem(columns, v);
    rows.push(v);
  }, (c) => {
    if (c.length > MAX_COLUMNS) throw new Error(`more than ${MAX_COLUMNS} columns`);
    columns = c;
  });
  if (rows.length !== 1) throw new Error('the result must be exactly one row');
  return Object.fromEntries(columns.map((c, i) => [c, rows[0][i]]));
}

/** Checks column count, duplicate names and size of a one-row result */
function rowProblem(columns: string[], values: (string | null)[]): void {
  if (columns.length > MAX_COLUMNS) throw new Error(`more than ${MAX_COLUMNS} columns`);
  if (new Set(columns).size !== columns.length) throw new Error('duplicate column names');
  const bytes = [...columns, ...values].reduce((n, x) => n + (x === null ? 0 : Buffer.byteLength(x)), 0);
  if (bytes > ROW_LIMIT) throw new Error(`the row exceeds ${ROW_LIMIT} bytes`);
}

export async function runVerify(ws: Workspace, o: { week?: string; onSource?: (s: SourceAdapter) => void; source?: SourceAdapter; signal?: AbortSignal } = {}): Promise<VerifyReport> {
  const checks = loadVerifyChecks(ws.dir);
  const cur = readCurrent(snapshotsDir(ws.config.outDir));
  if (!cur || !existsSync(cur.file)) throw new VerifyError('no snapshot: run collect first');
  const db = new DatabaseSync(cur.file, { readOnly: true });
  const meta = db.prepare('SELECT snapshot_id, source_cutoff_at FROM snapshot_meta').get() as { snapshot_id: string; source_cutoff_at: string };
  db.close();
  const { start, end } = verifyWeek(meta.source_cutoff_at, o.week);
  const values = { week_start: start, week_end: end, as_of: meta.source_cutoff_at };

  const inputs = loadBuildInputs(ws);
  const roles = allRoles(inputs.specs, inputs.derivedRoles);
  const blocked = { columns: [...roles].filter(([, r]) => r === 'private').map(([k]) => k), tables: [] };
  const cfg = ws.config;
  const slots = new ExecutionSlots(1, 1);
  const source = o.source ?? makeSource(ws);
  o.onSource?.(source);

  const items: VerifyItem[] = [];
  for (const c of checks) {
    if (o.signal?.aborted) throw new VerifyCancelled();
    const item: VerifyItem = { id: c.id, title: c.title, ok: false, source: null, snapshot: null, error: null };
    items.push(item);
    const bound = bindSourceSql(c.source_sql, values);
    if (!bound.ok) {
      item.error = `source SQL: ${bound.message}`;
      continue;
    }
    try {
      item.source = await sourceRow(source, bound.sql);
    } catch (e) {
      if (o.signal?.aborted) throw new VerifyCancelled();
      item.error = `source: ${(e as Error).message}`;
      continue;
    }
    const r = await slots.run('interactive', o.signal, (lease) => runQuery({
      lease, sql: c.snapshot_sql, path: cur.file, mode: 'panel', asOf: meta.source_cutoff_at,
      params: { ...cfg.params, week_start: start, week_end: end },
      readablePrefixes: [...new Set([...cfg.policy.readablePrefixes, 'r_', 'snapshot_'])], blocked, heapLimitMb: cfg.run.heapLimitMb, signal: o.signal,
    })).catch((e) => {
      if (o.signal?.aborted) throw new VerifyCancelled();
      throw e;
    });
    if (!r.ok && (r.kind === 'cancelled' || o.signal?.aborted)) throw new VerifyCancelled();
    if (!r.ok) {
      item.error = `snapshot: ${r.message}`;
      continue;
    }
    if (r.rows.length !== 1) {
      item.error = 'snapshot: the result must be exactly one row';
      continue;
    }
    const names = r.columns.map((x) => x.name);
    const vals = r.rows[0].map((v) => (v[1] === null ? null : String(v[1])));
    try {
      rowProblem(names, vals);
    } catch (e) {
      item.error = `snapshot: ${(e as Error).message}`;
      continue;
    }
    item.snapshot = Object.fromEntries(names.map((n, i) => [n, vals[i]]));
    const problem = compare(item.source, item.snapshot);
    item.ok = problem === null;
    item.error = problem;
  }
  const report: VerifyReport = { snapshot_id: meta.snapshot_id, as_of: meta.source_cutoff_at, week_start: start, week_end: end, items, ok: items.every((i) => i.ok) };
  const logDir = join(cfg.outDir, 'logs');
  mkdirSync(logDir, { recursive: true });
  writeFileSync(join(logDir, `verify-${new Date().toISOString().replace(/[:.]/g, '-')}.json`), `${JSON.stringify(report, null, 2)}\n`, { mode: 0o600 });
  return report;
}
