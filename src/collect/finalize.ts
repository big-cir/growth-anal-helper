// 스냅샷 확정: 기준 시각 정리 → 검증 → 해시 → 파생 → 가명 사본 → 교체.
import { DatabaseSync } from 'node:sqlite';
import { createHash } from 'node:crypto';
import { closeSync, existsSync, openSync, readFileSync, renameSync, rmSync, writeSync } from 'node:fs';
import { join } from 'node:path';
import type { Role, TableSpec } from './spec.ts';
import { specHash } from './spec.ts';
import { pseudonymize } from '../snapshot/pseudonymize.ts';
import { execDerivedSql } from '../snapshot/derived-authorizer.ts';
import { fsyncPath, snapshotFiles, writeCurrent } from '../snapshot/store.ts';
import { addDays, normalizeTs, weekStart } from '../time.ts';

export class FinalizeError extends Error {}

export type BuildInputs = {
  specs: TableSpec[];
  derivedSql: string;
  derivedRoles: Map<string, Role>;
  params: Record<string, string | number | string[]>;
};

export type FinalizeResult = { snapshotId: string; reused: boolean; files: ReturnType<typeof snapshotFiles> };

const sha = (s: string) => createHash('sha256').update(s).digest('hex');

export function allRoles(specs: TableSpec[], derivedRoles: Map<string, Role>): Map<string, Role> {
  const m = new Map<string, Role>();
  for (const t of specs) for (const c of t.columns) m.set(`${t.target}.${c.as}`, c.role);
  for (const [k, v] of derivedRoles) {
    if (!k.startsWith('d_')) throw new FinalizeError(`derived-columns.json: d_* 칸만 선언할 수 있음: ${k}`);
    m.set(k, v);
  }
  return m;
}

export function rolesHash(roles: Map<string, Role>): string {
  const arr = [...roles].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)).map(([k, v]) => [k, typeof v === 'string' ? v : { identifier: v.identifier }]);
  return sha(JSON.stringify(arr));
}

export function paramsHash(params: BuildInputs['params']): string {
  const sorted = Object.keys(params).sort().map((k) => [k, params[k]]);
  return sha(JSON.stringify(sorted));
}

export function loadDerivedRoles(file: string): Map<string, Role> {
  if (!existsSync(file)) throw new FinalizeError(`역할 선언 파일이 없음: ${file}`);
  const raw = JSON.parse(readFileSync(file, 'utf8')) as Record<string, unknown>;
  const m = new Map<string, Role>();
  for (const [k, v] of Object.entries(raw)) {
    if (!/^d_[A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*$/.test(k)) throw new FinalizeError(`derived-columns.json: 잘못된 칸 이름 ${k}`);
    if (v === 'ordinary' || v === 'private') m.set(k, v);
    else if (v && typeof v === 'object' && typeof (v as { identifier?: unknown }).identifier === 'string' && Object.keys(v).length === 1) m.set(k, { identifier: (v as { identifier: string }).identifier });
    else throw new FinalizeError(`derived-columns.json: ${k}의 역할은 "ordinary" | "private" | { "identifier": "<도메인>" }`);
  }
  return m;
}

/** 기준 시각 뒤 행 삭제, nullAfterCutoff 칸은 NULL로 */
export function applyCutoff(db: DatabaseSync, specs: TableSpec[], cutoff: string): void {
  for (const t of specs) {
    const dropped = Number(db.prepare(`DELETE FROM ${t.target} WHERE ${t.cutoffColumn} > ?`).run(cutoff).changes);
    let nulled = 0;
    for (const c of t.columns.filter((x) => x.nullAfterCutoff)) {
      nulled += Number(db.prepare(`UPDATE ${t.target} SET ${c.as} = NULL WHERE ${c.as} > ?`).run(cutoff).changes);
    }
    db.prepare('UPDATE r_collect_log SET dropped_after_cutoff = ?, nulled_after_cutoff = ? WHERE table_name = ?').run(dropped, nulled, t.target);
  }
}

function validate(db: DatabaseSync, specs: TableSpec[]): void {
  for (const t of specs) {
    const cols = (db.prepare(`PRAGMA table_info(${t.target})`).all() as { name: string }[]).map((r) => r.name);
    if (cols.join(',') !== t.columns.map((c) => c.as).join(',')) throw new FinalizeError(`${t.target}: 칸 목록이 명세와 다름`);
    const n = (db.prepare(`SELECT count(*) n FROM ${t.target}`).get() as { n: number }).n;
    if (n === 0) throw new FinalizeError(`${t.target}: 행이 없음`);
  }
}

export function rawHash(db: DatabaseSync, specs: TableSpec[]): string {
  const parts: string[] = [];
  for (const t of [...specs].sort((a, b) => (a.target < b.target ? -1 : 1))) {
    const h = createHash('sha256').update(`${t.target}\n`);
    const stmt = db.prepare(`SELECT ${t.columns.map((c) => c.as).join(', ')} FROM ${t.target} ORDER BY ${t.key.join(', ')}`);
    stmt.setReturnArrays(true);
    for (const row of stmt.iterate() as Iterable<unknown[]>) h.update(JSON.stringify(row) + '\n');
    parts.push(h.digest('hex'));
  }
  return sha(parts.join('\n'));
}

/** calendar_start 주부터 기준 시각까지 주 목록 */
export function buildCalendar(db: DatabaseSync, params: BuildInputs['params'], asOf: string): void {
  db.exec('CREATE TABLE d_calendar_week (week_start TEXT PRIMARY KEY, week_end TEXT NOT NULL)');
  const start = params.calendar_start;
  if (typeof start !== 'string') return;
  const ins = db.prepare('INSERT INTO d_calendar_week VALUES (?, ?)');
  for (let w = weekStart(normalizeTs(start)); w < asOf; w = addDays(w, 7)) ins.run(w, addDays(w, 7));
}

export function build(db: DatabaseSync, inp: BuildInputs, meta: { cutoff: string; startedAt: string; finishedAt: string }) {
  const roles = allRoles(inp.specs, inp.derivedRoles);
  const hashes = {
    raw_hash: rawHash(db, inp.specs),
    derived_hash: sha(inp.derivedSql),
    roles_hash: rolesHash(roles),
    params_hash: paramsHash(inp.params),
    spec_hash: specHash(inp.specs),
  };
  db.exec('DELETE FROM snapshot_meta; DELETE FROM snapshot_params');
  db.prepare(`INSERT INTO snapshot_meta (source_cutoff_at, collection_started_at, collection_finished_at, raw_hash, derived_hash, roles_hash, params_hash, spec_hash)
              VALUES (?, ?, ?, ?, ?, ?, ?, ?)`).run(meta.cutoff, meta.startedAt, meta.finishedAt, hashes.raw_hash, hashes.derived_hash, hashes.roles_hash, hashes.params_hash, hashes.spec_hash);
  const insParam = db.prepare('INSERT INTO snapshot_params VALUES (?, ?)');
  for (const [k, v] of Object.entries(inp.params)) for (const x of Array.isArray(v) ? v : [v]) insParam.run(k, String(x));

  try {
    execDerivedSql(db, inp.derivedSql, roles);
  } catch (e) {
    throw new FinalizeError(`derived.sql 실행 실패: ${(e as Error).message}`);
  }
  buildCalendar(db, inp.params, meta.cutoff);

  for (const t of inp.specs) {
    for (const c of t.columns) {
      if (typeof c.role === 'object' && c.as !== t.key[0]) db.exec(`CREATE INDEX IF NOT EXISTS ix_${t.target}_${c.as} ON ${t.target}(${c.as})`);
    }
  }
  const snapshotId = `${meta.cutoff.slice(0, 10).replace(/-/g, '')}-${meta.cutoff.slice(11, 19).replace(/:/g, '')}-${hashes.raw_hash.slice(0, 8)}-${hashes.derived_hash.slice(0, 8)}-${hashes.roles_hash.slice(0, 8)}-${hashes.params_hash.slice(0, 8)}`;
  db.prepare('UPDATE snapshot_meta SET snapshot_id = ?').run(snapshotId);
  return { snapshotId, hashes, roles };
}

/** snapshots/ 잠금. 남은 잠금은 자동으로 지우지 않고 안내한다 */
export function withSnapshotLock<T>(dir: string, fn: () => T): T {
  const lock = join(dir, '.lock');
  let fd: number | null = null;
  for (let attempt = 0; fd === null; attempt++) {
    try {
      fd = openSync(lock, 'wx');
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== 'EEXIST') throw e;
      let text: string;
      try {
        text = readFileSync(lock, 'utf8');
      } catch (re) {
        if ((re as NodeJS.ErrnoException).code === 'ENOENT' && attempt < 3) continue;
        throw re;
      }
      const owner = Number(text.trim());
      let alive = Number.isInteger(owner) && owner > 0;
      if (alive) {
        try {
          process.kill(owner, 0);
        } catch {
          alive = false;
        }
      }
      if (alive) throw new FinalizeError(`다른 수집·파생이 진행 중(pid ${owner}). 끝난 뒤 다시 실행하세요`);
      throw new FinalizeError(`낡은 잠금이 남아 있음(pid ${text.trim() || '?'}은 없음). 다른 실행이 없는지 확인한 뒤 ${lock}을 지우고 다시 실행하세요`);
    }
  }
  try {
    writeSync(fd, String(process.pid));
    closeSync(fd);
    return fn();
  } finally {
    rmSync(lock, { force: true });
  }
}

type MetaHashes = Record<'raw_hash' | 'derived_hash' | 'roles_hash' | 'params_hash' | 'spec_hash', string>;

function readMetaHashes(path: string): MetaHashes | null {
  try {
    const db = new DatabaseSync(path, { readOnly: true });
    const m = db.prepare('SELECT raw_hash, derived_hash, roles_hash, params_hash, spec_hash FROM snapshot_meta').get() as MetaHashes | undefined;
    db.close();
    return m ?? null;
  } catch {
    return null;
  }
}

/** 실패하면 임시 파일을 지우고 current.json은 그대로 둔다. failAt은 테스트용 */
export function finalize(o: {
  tmpPath: string;
  snapshotsDir: string;
  inputs: BuildInputs;
  meta: { cutoff: string; startedAt: string; finishedAt: string };
  applyCutoffFirst: boolean;
  failAt?: 'cutoff' | 'build' | 'pseudonymize' | 'rename' | 'rename-1' | 'rename-2';
}): FinalizeResult {
  const tmpAgent = `${o.tmpPath}.agent`;
  const tmpMap = `${o.tmpPath}.map`;
  const cleanup = () => {
    for (const p of [o.tmpPath, tmpAgent, tmpMap]) {
      rmSync(p, { force: true });
      rmSync(`${p}-journal`, { force: true });
    }
  };
  try {
    const db = new DatabaseSync(o.tmpPath);
    let snapshotId: string;
    let roles: Map<string, Role>;
    try {
      db.exec('PRAGMA journal_mode = DELETE');
      if (o.applyCutoffFirst) {
        if (o.failAt === 'cutoff') throw new FinalizeError('강제 실패(cutoff)');
        applyCutoff(db, o.inputs.specs, o.meta.cutoff);
        validate(db, o.inputs.specs);
      }
      if (o.failAt === 'build') throw new FinalizeError('강제 실패(build)');
      ({ snapshotId, roles } = build(db, o.inputs, o.meta));
    } finally {
      db.close();
    }

    if (o.failAt === 'pseudonymize') throw new FinalizeError('강제 실패(pseudonymize)');
    pseudonymize({ srcPath: o.tmpPath, agentPath: tmpAgent, mapPath: tmpMap, roles });
    for (const p of [o.tmpPath, tmpAgent, tmpMap]) fsyncPath(p);

    return withSnapshotLock(o.snapshotsDir, () => place(o, snapshotId, tmpAgent, tmpMap, cleanup));
  } catch (e) {
    cleanup();
    throw e;
  }
}

/** 같은 세트가 있으면 재사용, 없으면 세 파일을 옮기고 포인터를 바꾼다 */
function place(o: Parameters<typeof finalize>[0], snapshotId: string, tmpAgent: string, tmpMap: string, cleanup: () => void): FinalizeResult {
  const files = snapshotFiles(o.snapshotsDir, snapshotId);
  const existing = [files.real, files.agent, files.map].filter((p) => existsSync(p));
  if (existing.length > 0) {
    const mine = readMetaHashes(o.tmpPath);
    const theirs = readMetaHashes(files.real);
    if (existing.length === 3 && mine && theirs && JSON.stringify(mine) === JSON.stringify(theirs)) {
      cleanup();
      writeCurrent(o.snapshotsDir, snapshotId);
      return { snapshotId, reused: true, files };
    }
    throw new FinalizeError(`같은 snapshot_id(${snapshotId})의 파일이 이미 있지만 일부가 없거나 내용이 다름. 사람이 확인해야 함`);
  }
  if (o.failAt === 'rename') throw new FinalizeError('강제 실패(rename)');
  // 중간에 실패하면 옮긴 파일을 지운다
  const moved: string[] = [];
  try {
    for (const [from, to] of [[tmpMap, files.map], [tmpAgent, files.agent], [o.tmpPath, files.real]] as const) {
      renameSync(from, to);
      moved.push(to);
      if (o.failAt === `rename-${moved.length}`) throw new FinalizeError(`강제 실패(rename-${moved.length})`);
    }
    fsyncPath(o.snapshotsDir);
  } catch (e) {
    for (const p of moved) rmSync(p, { force: true });
    throw e;
  }
  writeCurrent(o.snapshotsDir, snapshotId);
  return { snapshotId, reused: false, files };
}
