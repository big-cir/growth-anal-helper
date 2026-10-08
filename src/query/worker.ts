// 쿼리 1개를 실행하는 하위 프로세스. 입력 stdin JSON, 출력 stdout JSON.
import { DatabaseSync } from 'node:sqlite';
import { readSync } from 'node:fs';
import { makeQueryAuthorizer } from './authorizer.ts';

export type WorkerInput = {
  path: string;
  sql: string;
  params: Record<string, string | number>;
  readablePrefixes: string[];
  /** 민감 거부할 칸("표.칸")과 표 */
  blockedColumns?: string[];
  blockedTables?: string[];
  heapLimitMb: number;
  maxRows: number;
  overflow: 'more' | 'error';
  cellLimit: number;     // 바이트
  truncateCells: boolean;
  outputLimit: number;   // 바이트
};

export type Tagged = ['n', null] | ['s', string] | ['i', number] | ['f', number];
export type ResultColumn = { name: string; table: string | null; column: string | null };
export type WorkerOutput =
  | { ok: true; columns: ResultColumn[]; rows: Tagged[][]; more: boolean; truncatedCells: number; ms: number; tables: string[] }
  | { ok: false; kind: 'sqlite' | 'type' | 'limit' | 'input' | 'sensitive'; message: string };

export const INPUT_LIMIT = 64 * 1024;
export const SENSITIVE_MESSAGE = '이 도구가 다룰 수 없는 데이터예요';

class Fail extends Error {
  readonly kind: 'type' | 'limit';
  constructor(kind: 'type' | 'limit', message: string) {
    super(message);
    this.kind = kind;
  }
}

const enc = new TextEncoder();

function tag(v: unknown, col: string, o: WorkerInput, counter: { truncated: number }): Tagged {
  if (v === null) return ['n', null];
  if (typeof v === 'string') {
    const bytes = enc.encode(v).length;
    if (bytes <= o.cellLimit) return ['s', v];
    if (!o.truncateCells) throw new Fail('limit', `칸 ${col}의 값이 ${o.cellLimit}바이트를 넘음`);
    counter.truncated++;
    let cut = v.slice(0, o.cellLimit);
    while (enc.encode(cut).length > o.cellLimit) cut = cut.slice(0, -1);
    return ['s', cut];
  }
  if (typeof v === 'number') {
    if (!Number.isFinite(v)) throw new Fail('type', `칸 ${col}: 유한하지 않은 수`);
    if (Number.isInteger(v)) {
      if (!Number.isSafeInteger(v)) throw new Fail('type', `칸 ${col}: 안전 범위 밖 정수`);
      return ['i', v];
    }
    return ['f', v];
  }
  if (v instanceof Uint8Array) throw new Fail('type', `칸 ${col}: BLOB은 지원하지 않음`);
  throw new Fail('type', `칸 ${col}: 지원하지 않는 값 형식(${typeof v})`);
}

export function runInWorker(o: WorkerInput): WorkerOutput {
  const t0 = performance.now();
  let db: DatabaseSync;
  try {
    db = new DatabaseSync(o.path, { readOnly: true });
  } catch (e) {
    return { ok: false, kind: 'sqlite', message: (e as Error).message };
  }
  const seen = { reads: new Set<string>(), denied: new Set<string>(), sensitive: new Set<string>() };
  const blocked = { columns: new Set(o.blockedColumns ?? []), tables: new Set(o.blockedTables ?? []) };
  try {
    db.exec(`PRAGMA hard_heap_limit = ${Math.floor(o.heapLimitMb * 1024 * 1024)}`);
    db.setAuthorizer(makeQueryAuthorizer(o.readablePrefixes, seen, blocked));
    const stmt = db.prepare(o.sql);
    // 거부한 읽기가 하나라도 있으면 실행하지 않는다(민감 거부는 무엇을 막았는지 알리지 않음)
    if (seen.sensitive.size) return { ok: false, kind: 'sensitive', message: SENSITIVE_MESSAGE };
    if (seen.denied.size) return { ok: false, kind: 'sqlite', message: `읽을 수 없는 표: ${[...seen.denied].sort().join(', ')}` };
    stmt.setReadBigInts(false);
    stmt.setReturnArrays(true);
    const columns: ResultColumn[] = stmt.columns().map((c) => ({ name: c.name, table: c.table, column: c.column }));
    const rows: Tagged[][] = [];
    const counter = { truncated: 0 };
    const tables = [...seen.reads].sort();
    let size = Buffer.byteLength(JSON.stringify({ ok: true, columns, rows: [], more: false, truncatedCells: 0, ms: 0, tables })) + 64;
    let more = false;
    const iter = (Object.keys(o.params).length ? stmt.iterate(o.params) : stmt.iterate()) as Iterable<unknown[]>;
    for (const row of iter) {
      if (rows.length === o.maxRows) {
        if (o.overflow === 'error') throw new Fail('limit', `결과가 ${o.maxRows}행을 넘음`);
        more = true;
        break;
      }
      const tagged = row.map((v, i) => tag(v, columns[i].name, o, counter));
      size += Buffer.byteLength(JSON.stringify(tagged)) + 1;
      if (size > o.outputLimit) throw new Fail('limit', `결과가 ${o.outputLimit}바이트를 넘음`);
      rows.push(tagged);
    }
    return { ok: true, columns, rows, more, truncatedCells: counter.truncated, ms: Math.round(performance.now() - t0), tables };
  } catch (e) {
    if (e instanceof Fail) return { ok: false, kind: e.kind, message: e.message };
    const msg = (e as Error).message;
    if (/too large to be represented/i.test(msg)) return { ok: false, kind: 'type', message: '안전 범위 밖 정수' };
    // 민감 거부는 무엇을 막았는지 알리지 않는다
    if (seen.sensitive.size) return { ok: false, kind: 'sensitive', message: SENSITIVE_MESSAGE };
    if (seen.denied.size) return { ok: false, kind: 'sqlite', message: `읽을 수 없는 표: ${[...seen.denied].sort().join(', ')}` };
    return { ok: false, kind: 'sqlite', message: msg };
  } finally {
    db.close();
  }
}

// 하위 프로세스로 실행될 때
if (process.argv[1] && import.meta.filename === process.argv[1]) {
  let out: WorkerOutput;
  try {
    const buf = Buffer.alloc(INPUT_LIMIT + 1);
    let n = 0;
    for (;;) {
      let r = 0;
      try {
        r = readSync(0, buf, n, buf.length - n, null);
      } catch (e) {
        if ((e as NodeJS.ErrnoException).code === 'EAGAIN') continue;
        if ((e as NodeJS.ErrnoException).code === 'EOF') break;
        throw e;
      }
      if (r === 0) break;
      n += r;
      if (n > INPUT_LIMIT) break;
    }
    if (n > INPUT_LIMIT) out = { ok: false, kind: 'input', message: '입력이 64KB를 넘음' };
    else out = runInWorker(JSON.parse(buf.subarray(0, n).toString('utf8')) as WorkerInput);
  } catch (e) {
    out = { ok: false, kind: 'input', message: (e as Error).message };
  }
  process.stdout.write(JSON.stringify(out));
}
