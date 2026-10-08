// 수집: 테이블마다 소스 조회 한 번, 행을 흘려받아 임시 SQLite에 넣는다.
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

function convert(c: ColumnSpec, v: RawValue, wrapText: boolean, where: string): Cell {
  if (v === null) return null;
  switch (c.kind) {
    case 'int': {
      if (!/^-?\d+$/.test(v)) throw new CollectError(`${where}: 정수가 아님`);
      const n = Number(v);
      if (!Number.isSafeInteger(n)) throw new CollectError(`${where}: 안전 범위 밖 정수`);
      return n;
    }
    case 'ts':
      try {
        return normalizeTs(v);
      } catch {
        throw new CollectError(`${where}: 시각 형식이 아님`);
      }
    case 'bool':
      if (v === '0' || v === '1') return Number(v);
      throw new CollectError(`${where}: bool(0/1)이 아님`);
    case 'text': {
      let s = v;
      if (wrapText) {
        if (!s.startsWith('s')) throw new CollectError(`${where}: text 접두사 없음`);
        s = s.slice(1);
      }
      if ([...s].length > c.maxLength) throw new CollectError(`${where}: maxLength(${c.maxLength}) 초과`);
      return s;
    }
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

/** 임시 스냅샷 파일을 만든다. 실패하면 임시 파일을 지운다 */
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
      const res = await o.source.selectStream(selectSql(t, o.source.wrapText), (raw) => {
        if (raw.length !== t.columns.length) throw new CollectError(`${t.target}: 열 개수 불일치`);
        const row = t.columns.map((c, i) => convert(c, raw[i], o.source.wrapText, `${t.target}.${c.as} (행 ${n + 1})`));
        const key = keyIdx.map((i) => row[i]);
        if (key.some((v) => v === null)) throw new CollectError(`${t.target}: 키가 NULL (행 ${n + 1})`);
        const k = key as number[];
        if (prevKey && !keyGreater(k, prevKey)) throw new CollectError(`${t.target}: 키 순서 위반 (행 ${n + 1}) — ORDER BY가 지켜지지 않음`);
        prevKey = k;
        try {
          ins.run(...row);
        } catch (e) {
          throw new CollectError(`${t.target}: 삽입 실패 (행 ${n + 1}): ${(e as Error).message}`);
        }
        n++;
        if (n % COMMIT_EVERY === 0) {
          db.exec('COMMIT');
          db.exec('BEGIN');
        }
      }, (cols) => {
        sawHeader = true;
        if (cols.join('\t') !== expected.join('\t')) throw new CollectError(`${t.target}: 헤더 불일치 (${cols.join(',')} ≠ ${expected.join(',')})`);
      });
      db.exec('COMMIT');
      if (!sawHeader && res.rows > 0) throw new CollectError(`${t.target}: 헤더 없이 행이 옴`);
      db.prepare('INSERT INTO r_collect_log (table_name, rows, ms, collected_at) VALUES (?, ?, ?, ?)').run(t.target, n, res.ms, localNow());
      log(`${t.target}: ${n}행 ${res.ms}ms`);
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
