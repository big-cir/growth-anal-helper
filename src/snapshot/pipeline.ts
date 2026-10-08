// collect · derive · test-derived 명령.
import { DatabaseSync } from 'node:sqlite';
import { appendFileSync, copyFileSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { isDeepStrictEqual } from 'node:util';
import type { Workspace } from '../workspace.ts';
import { assertOutputsIgnored, guardedPaths } from '../workspace.ts';
import { loadSpec, specHash, type TableSpec } from '../collect/spec.ts';
import { collect } from '../collect/collector.ts';
import { buildCalendar, finalize, loadDerivedRoles, type BuildInputs, type FinalizeResult } from '../collect/finalize.ts';
import { CommandSourceAdapter } from '../collect/sources/command.ts';
import { SqliteSource } from '../collect/sources/sqlite.ts';
import type { SourceAdapter } from '../collect/sources/source.ts';
import { readCurrent, snapshotsDir } from './store.ts';
import { ENGINE_TABLES_SQL, rawDdl } from './raw-ddl.ts';
import { execDerivedSql } from './derived-authorizer.ts';

export class PipelineError extends Error {}

export function loadBuildInputs(ws: Workspace): BuildInputs {
  const derivedFile = join(ws.dir, 'derived.sql');
  if (!existsSync(derivedFile)) throw new PipelineError(`파생 SQL이 없음: ${derivedFile}`);
  return {
    specs: loadSpec(join(ws.dir, 'tables.json')),
    derivedSql: readFileSync(derivedFile, 'utf8'),
    derivedRoles: loadDerivedRoles(join(ws.dir, 'derived-columns.json')),
    params: ws.config.params,
  };
}

export function makeSource(ws: Workspace, logDir: string): SourceAdapter {
  const s = ws.config.source;
  if (s.type === 'sqlite') {
    if (!existsSync(s.path)) throw new PipelineError(`소스 SQLite 파일이 없음: ${s.path}`);
    return new SqliteSource(s.path);
  }
  mkdirSync(logDir, { recursive: true });
  const logFile = join(logDir, 'source-stderr.log');
  return new CommandSourceAdapter(s, (line) => appendFileSync(logFile, `${new Date().toISOString()} ${line}\n`));
}

/** 끝난 프로세스가 남긴 임시 파일을 지운다 */
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

export async function runCollect(ws: Workspace, o: { log?: (m: string) => void; onSource?: (s: SourceAdapter) => void } = {}): Promise<FinalizeResult> {
  const inputs = loadBuildInputs(ws);
  const dir = prepareOut(ws);
  const source = makeSource(ws, join(ws.config.outDir, 'logs'));
  o.onSource?.(source);
  const c = await collect({ specs: inputs.specs, source, snapshotsDir: dir, log: o.log });
  return finalize({ tmpPath: c.tmpPath, snapshotsDir: dir, inputs, meta: { cutoff: c.cutoff, startedAt: c.startedAt, finishedAt: c.finishedAt }, applyCutoffFirst: true });
}

/** 현재 스냅샷의 원본 표로 파생·가명 사본만 다시 만든다(소스는 읽지 않음) */
export function runDerive(ws: Workspace): FinalizeResult {
  const inputs = loadBuildInputs(ws);
  const dir = prepareOut(ws);
  const cur = readCurrent(dir);
  if (!cur) throw new PipelineError('현재 스냅샷이 없음: 먼저 collect를 실행하세요');
  const tmpPath = join(dir, `tmp-${process.pid}-${Date.now()}.sqlite`);
  copyFileSync(cur.file, tmpPath);
  const db = new DatabaseSync(tmpPath);
  let meta: { cutoff: string; startedAt: string; finishedAt: string };
  try {
    const m = db.prepare('SELECT source_cutoff_at, collection_started_at, collection_finished_at, spec_hash FROM snapshot_meta').get() as Record<string, string>;
    if (m.spec_hash !== specHash(inputs.specs)) {
      throw new PipelineError('tables.json이 역할(role) 외에도 바뀌었음: derive가 아니라 collect를 실행하세요');
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
  return finalize({ tmpPath, snapshotsDir: dir, inputs, meta, applyCutoffFirst: false });
}

export type DerivedCaseResult = { name: string; ok: boolean; message?: string };

/** tests/<case>/input.sql을 넣고 derived.sql을 돌린 뒤 expected.json과 비교한다 */
export function runTestDerived(ws: Workspace): DerivedCaseResult[] {
  const testsDir = join(ws.dir, 'tests');
  if (!existsSync(testsDir)) throw new PipelineError(`픽스처 폴더가 없음: ${testsDir}`);
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
    if (!asOf) return { name, ok: false, message: 'input.sql에 snapshot_meta 행이 없음' };
    buildCalendar(db, params, asOf);
    const expected = JSON.parse(readFileSync(join(dir, 'expected.json'), 'utf8')) as { query: string; rows: unknown[] }[];
    for (const [i, e] of expected.entries()) {
      const actual = db.prepare(e.query).all().map((r) => ({ ...r }));
      if (!isDeepStrictEqual(actual, e.rows)) {
        return { name, ok: false, message: `질의 ${i + 1} 불일치\n  질의: ${e.query}\n  기대: ${JSON.stringify(e.rows)}\n  실제: ${JSON.stringify(actual)}` };
      }
    }
    return { name, ok: true };
  } catch (e) {
    return { name, ok: false, message: (e as Error).message };
  } finally {
    db.close();
  }
}
