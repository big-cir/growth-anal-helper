// collect, derive and test-derived commands.
import { DatabaseSync } from 'node:sqlite';
import { appendFileSync, copyFileSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { isDeepStrictEqual } from 'node:util';
import type { Workspace } from '../workspace.ts';
import { assertOutputsIgnored, guardedPaths } from '../workspace.ts';
import { loadSpec, specHash, type TableSpec } from '../collect/spec.ts';
import { collect } from '../collect/collector.ts';
import { acquireSnapshotLock, buildCalendar, finalize, loadDerivedRoles, type BuildInputs, type FinalizeResult } from '../collect/finalize.ts';
import { ga4Roles, ga4Tables, importGa4, loadGa4Plan, type Ga4ImportOptions, type Ga4Plan } from '../collect/ga4/importer.ts';
import { MysqlSource } from '../collect/sources/mysql.ts';
import { PostgresSource } from '../collect/sources/postgres.ts';
import { SqliteSource } from '../collect/sources/sqlite.ts';
import type { SourceAdapter } from '../collect/sources/source.ts';
import { readCurrent, snapshotsDir } from './store.ts';
import { ENGINE_TABLES_SQL, rawDdl } from './raw-ddl.ts';
import { execDerivedSql } from './derived-authorizer.ts';

export class PipelineError extends Error {}

export function loadGa4(ws: Workspace): Ga4Plan | null {
  return ws.config.ga4 ? loadGa4Plan(ws.dir, ws.config.ga4) : null;
}

export function loadBuildInputs(ws: Workspace, ga4: Ga4Plan | null = loadGa4(ws)): BuildInputs {
  const derivedFile = join(ws.dir, 'derived.sql');
  if (!existsSync(derivedFile)) throw new PipelineError(`derived SQL not found: ${derivedFile}`);
  return {
    ga4: ga4 ? { specHash: ga4.specHash, roles: ga4Roles(ga4), tables: ga4Tables(ga4) } : null,
    specs: loadSpec(join(ws.dir, 'tables.json')),
    derivedSql: readFileSync(derivedFile, 'utf8'),
    derivedRoles: loadDerivedRoles(join(ws.dir, 'derived-columns.json')),
    params: ws.config.params,
  };
}

export function makeSource(ws: Workspace): SourceAdapter {
  const s = ws.config.datasource;
  if (s.kind === 'mysql') return new MysqlSource(s);
  if (s.kind === 'postgres') return new PostgresSource(s);
  if (!existsSync(s.path)) throw new PipelineError(`source SQLite file not found: ${s.path}`);
  return new SqliteSource(s.path);
}

/** Deletes temporary files left by processes that have exited */
function removeStaleTemps(dir: string): void {
  for (const f of readdirSync(dir)) {
    const m = /^tmp-(\d+)-/.exec(f);
    if (!m) continue;
    let alive = true;
    try {
      process.kill(Number(m[1]), 0);
    } catch {
      alive = false;
    }
    if (!alive) rmSync(join(dir, f), { force: true });
  }
}

function prepareOut(ws: Workspace): string {
  assertOutputsIgnored(guardedPaths(ws), ws.dir);
  const dir = snapshotsDir(ws.config.outDir);
  mkdirSync(dir, { recursive: true });
  removeStaleTemps(dir);
  return dir;
}

/** Holds the lock throughout: DB collect → GA4 import → finalize. On failure only the temporary files are deleted */
export async function runCollect(ws: Workspace, o: { log?: (m: string) => void; onSource?: (s: SourceAdapter) => void; ga4?: Ga4ImportOptions } = {}): Promise<FinalizeResult> {
  const plan = loadGa4(ws);
  const inputs = loadBuildInputs(ws, plan);
  const dir = prepareOut(ws);
  const release = acquireSnapshotLock(dir);
  try {
    const source = makeSource(ws);
    o.onSource?.(source);
    const c = await collect({ specs: inputs.specs, source, snapshotsDir: dir, log: o.log });
    if (plan) {
      const db = new DatabaseSync(c.tmpPath);
      try {
        await importGa4(db, plan, { log: o.log, ...o.ga4 });
      } catch (e) {
        db.close();
        rmSync(c.tmpPath, { force: true });
        rmSync(`${c.tmpPath}-journal`, { force: true });
        throw e;
      }
      db.close();
    }
    return finalize({ tmpPath: c.tmpPath, snapshotsDir: dir, inputs, meta: { cutoff: c.cutoff, startedAt: c.startedAt, finishedAt: c.finishedAt }, applyCutoffFirst: true, lockHeld: true });
  } finally {
    release();
  }
}

/** Rebuilds derived tables and the pseudonymized copy from the current snapshot's raw tables (the source is not read) */
export function runDerive(ws: Workspace): FinalizeResult {
  const inputs = loadBuildInputs(ws);
  const dir = prepareOut(ws);
  const release = acquireSnapshotLock(dir);
  try {
    return deriveLocked(dir, inputs);
  } finally {
    release();
  }
}

function deriveLocked(dir: string, inputs: BuildInputs): FinalizeResult {
  const cur = readCurrent(dir);
  if (!cur) throw new PipelineError('no current snapshot: run collect first');
  const tmpPath = join(dir, `tmp-${process.pid}-${Date.now()}.sqlite`);
  copyFileSync(cur.file, tmpPath);
  const db = new DatabaseSync(tmpPath);
  let meta: { cutoff: string; startedAt: string; finishedAt: string };
  try {
    const m = db.prepare('SELECT source_cutoff_at, collection_started_at, collection_finished_at, spec_hash, ga4_spec_hash FROM snapshot_meta').get() as Record<string, string>;
    if (m.spec_hash !== specHash(inputs.specs)) {
      throw new PipelineError('tables.json changed beyond roles: run collect, not derive');
    }
    if (m.ga4_spec_hash !== (inputs.ga4?.specHash ?? '')) {
      throw new PipelineError('GA4 settings (workspace.json ga4, ga4-reports.json) changed: run collect, not derive');
    }
    meta = { cutoff: m.source_cutoff_at, startedAt: m.collection_started_at, finishedAt: m.collection_finished_at };
    const derived = db.prepare("SELECT name FROM sqlite_master WHERE type IN ('table', 'view') AND name LIKE 'd\\_%' ESCAPE '\\'").all() as { name: string }[];
    for (const { name } of derived) db.exec(`DROP TABLE IF EXISTS "${name}"`);
    db.close();
  } catch (e) {
    db.close();
    rmSync(tmpPath, { force: true });
    throw e;
  }
  return finalize({ tmpPath, snapshotsDir: dir, inputs, meta, applyCutoffFirst: false, lockHeld: true });
}

export type DerivedCaseResult = { name: string; ok: boolean; message?: string };

/** Loads tests/<case>/input.sql, runs derived.sql and compares with expected.json */
export function runTestDerived(ws: Workspace): DerivedCaseResult[] {
  const testsDir = join(ws.dir, 'tests');
  if (!existsSync(testsDir)) throw new PipelineError(`fixtures folder not found: ${testsDir}`);
  const specs = loadSpec(join(ws.dir, 'tables.json'));
  const derivedSql = readFileSync(join(ws.dir, 'derived.sql'), 'utf8');
  const cases = readdirSync(testsDir).filter((d) => statSync(join(testsDir, d)).isDirectory()).sort();
  return cases.map((name) => runCase(name, join(testsDir, name), specs, derivedSql));
}

function runCase(name: string, dir: string, specs: TableSpec[], derivedSql: string): DerivedCaseResult {
  const db = new DatabaseSync(':memory:');
  try {
    db.exec(readFileSync(ENGINE_TABLES_SQL, 'utf8'));
    for (const t of specs) db.exec(rawDdl(t));
    db.exec(readFileSync(join(dir, 'input.sql'), 'utf8'));
    execDerivedSql(db, derivedSql);
    const params: BuildInputs['params'] = {};
    for (const r of db.prepare('SELECT key, value FROM snapshot_params').all() as { key: string; value: string }[]) params[r.key] = r.value;
    const asOf = (db.prepare('SELECT source_cutoff_at a FROM snapshot_meta').get() as { a: string } | undefined)?.a;
    if (!asOf) return { name, ok: false, message: 'input.sql has no snapshot_meta row' };
    buildCalendar(db, params, asOf);
    const expected = JSON.parse(readFileSync(join(dir, 'expected.json'), 'utf8')) as { query: string; rows: unknown[] }[];
    for (const [i, e] of expected.entries()) {
      const actual = db.prepare(e.query).all().map((r) => ({ ...r }));
      if (!isDeepStrictEqual(actual, e.rows)) {
        return { name, ok: false, message: `query ${i + 1} mismatch\n  query:    ${e.query}\n  expected: ${JSON.stringify(e.rows)}\n  actual:   ${JSON.stringify(actual)}` };
      }
    }
    return { name, ok: true };
  } catch (e) {
    return { name, ok: false, message: (e as Error).message };
  } finally {
    db.close();
  }
}
