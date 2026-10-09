// Collection: one source query per table; rows are streamed into a temporary SQLite file.
import { DatabaseSync } from 'node:sqlite';
import { readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { normalizeTs } from '../time.ts';
import type { RawValue, SourceAdapter } from './sources/source.ts';
import { selectSql, type ColumnSpec, type TableSpec } from './spec.ts';
import { ENGINE_TABLES_SQL, rawDdl } from '../snapshot/raw-ddl.ts';

export type CollectResult = { tmpPath: string; cutoff: string; startedAt: string; finishedAt: string };

const COMMIT_EVERY = 10_000;

export class CollectError extends Error {}

type Cell = number | string | null;

function convert(c: ColumnSpec, v: RawValue, where: string): Cell {
  if (v === null) return null;
  switch (c.kind) {
    case 'int': {
      if (!/^-?\d+$/.test(v)) throw new CollectError(`${where}: not an integer`);
      const n = Number(v);
      if (!Number.isSafeInteger(n)) throw new CollectError(`${where}: integer outside the safe range`);
      return n;
    }
    case 'ts':
      try {
        return normalizeTs(v);
      } catch {
        throw new CollectError(`${where}: not a timestamp`);
      }
    case 'bool':
      if (v === '0' || v === '1') return Number(v);
      throw new CollectError(`${where}: not a bool (0/1)`);
    case 'text':
      if ([...v].length > c.maxLength) throw new CollectError(`${where}: exceeds maxLength (${c.maxLength})`);
      return v;
  }
}

function keyGreater(a: number[], b: number[]): boolean {
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return a[i] > b[i];
  }
  return false;
}

export function localNow(): string {
  const d = new Date();
  const p = (n: number, w = 2) => String(n).padStart(w, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}000`;
}

/** Builds the temporary snapshot file. Deletes it on failure */
export async function collect(o: {
  specs: TableSpec[];
  source: SourceAdapter;
  snapshotsDir: string;
  log?: (msg: string) => void;
}): Promise<CollectResult> {
  const log = o.log ?? (() => {});
  const startedAt = localNow();
  const cutoff = normalizeTs(await o.source.now());
  const tmpPath = join(o.snapshotsDir, `tmp-${process.pid}-${Date.now()}.sqlite`);
  const db = new DatabaseSync(tmpPath);
  try {
    db.exec('PRAGMA journal_mode = DELETE');
    db.exec(readFileSync(ENGINE_TABLES_SQL, 'utf8'));
    for (const t of o.specs) db.exec(rawDdl(t));

    for (const t of o.specs) {
      const keyIdx = t.key.map((k) => t.columns.findIndex((c) => c.as === k));
      const ins = db.prepare(`INSERT INTO ${t.target} VALUES (${t.columns.map(() => '?').join(', ')})`);
      let prevKey: number[] | null = null;
      let n = 0;
      const expected = t.columns.map((c) => c.as);
      let sawHeader = false;
      db.exec('BEGIN');
      const res = await o.source.selectStream(selectSql(t, o.source.dialect), (raw) => {
        if (raw.length !== t.columns.length) throw new CollectError(`${t.target}: column count mismatch`);
        const row = t.columns.map((c, i) => convert(c, raw[i], `${t.target}.${c.as} (row ${n + 1})`));
        const key = keyIdx.map((i) => row[i]);
        if (key.some((v) => v === null)) throw new CollectError(`${t.target}: key is NULL (row ${n + 1})`);
        const k = key as number[];
        if (prevKey && !keyGreater(k, prevKey)) throw new CollectError(`${t.target}: key order violation (row ${n + 1}): ORDER BY not respected`);
        prevKey = k;
        try {
          ins.run(...row);
        } catch (e) {
          throw new CollectError(`${t.target}: insert failed (row ${n + 1}): ${(e as Error).message}`);
        }
        n++;
        if (n % COMMIT_EVERY === 0) {
          db.exec('COMMIT');
          db.exec('BEGIN');
        }
      }, (cols) => {
        sawHeader = true;
        if (cols.join('\t') !== expected.join('\t')) throw new CollectError(`${t.target}: header mismatch (${cols.join(',')} ≠ ${expected.join(',')})`);
      });
      db.exec('COMMIT');
      if (!sawHeader && res.rows > 0) throw new CollectError(`${t.target}: rows arrived without a header`);
      db.prepare('INSERT INTO r_collect_log (table_name, rows, ms, collected_at) VALUES (?, ?, ?, ?)').run(t.target, n, res.ms, localNow());
      log(`${t.target}: ${n} rows ${res.ms}ms`);
    }
    db.close();
    return { tmpPath, cutoff, startedAt, finishedAt: localNow() };
  } catch (e) {
    if (db.isTransaction) db.exec('ROLLBACK');
    db.close();
    rmSync(tmpPath, { force: true });
    rmSync(`${tmpPath}-journal`, { force: true });
    throw e;
  }
}
