// Child process that runs one query. Input: stdin JSON, output: stdout JSON.
import { DatabaseSync } from 'node:sqlite';
import { readSync } from 'node:fs';
import { makeQueryAuthorizer } from './authorizer.ts';
import { cteNames } from './sql-lint.ts';
import { tr } from '../i18n.ts';

export type WorkerInput = {
  path: string;
  sql: string;
  params: Record<string, string | number>;
  readablePrefixes: string[];
  /** Columns ("table.column") and tables to deny as sensitive */
  blockedColumns?: string[];
  blockedTables?: string[];
  heapLimitMb: number;
  maxRows: number;
  overflow: 'more' | 'error';
  cellLimit: number;     // bytes
  truncateCells: boolean;
  outputLimit: number;   // bytes
};

export type Tagged = ['n', null] | ['s', string] | ['i', number] | ['f', number];
export type ResultColumn = { name: string; table: string | null; column: string | null };
export type WorkerOutput =
  | { ok: true; columns: ResultColumn[]; rows: Tagged[][]; more: boolean; truncatedCells: number; ms: number; tables: string[] }
  | { ok: false; kind: 'sqlite' | 'type' | 'limit' | 'input' | 'sensitive'; message: string };

export const INPUT_LIMIT = 64 * 1024;
export const SENSITIVE_MESSAGE = "This tool can't work with that data";
/** SENSITIVE_MESSAGE in the configured language (for the web UI) */
export const sensitiveMessage = () => tr(SENSITIVE_MESSAGE, '이 도구가 다룰 수 없는 데이터예요');

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
    if (!o.truncateCells) throw new Fail('limit', `value of column ${col} exceeds ${o.cellLimit} bytes`);
    counter.truncated++;
    let cut = v.slice(0, o.cellLimit);
    while (enc.encode(cut).length > o.cellLimit) cut = cut.slice(0, -1);
    return ['s', cut];
  }
  if (typeof v === 'number') {
    if (!Number.isFinite(v)) throw new Fail('type', `column ${col}: non-finite number`);
    if (Number.isInteger(v)) {
      if (!Number.isSafeInteger(v)) throw new Fail('type', `column ${col}: integer outside the safe range`);
      return ['i', v];
    }
    return ['f', v];
  }
  if (v instanceof Uint8Array) throw new Fail('type', `column ${col}: BLOB is not supported`);
  throw new Fail('type', `column ${col}: unsupported value type (${typeof v})`);
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
    // A WITH name that matches a real schema object or a blocked table gets no exception
    const taken = new Set((db.prepare('SELECT lower(name) AS n FROM sqlite_schema').all() as { n: string }[]).map((r) => r.n));
    for (const t of blocked.tables) taken.add(t.toLowerCase());
    const cteOnly = new Set([...cteNames(o.sql)].filter((n) => !taken.has(n)));
    db.setAuthorizer(makeQueryAuthorizer(o.readablePrefixes, seen, blocked, cteOnly));
    const stmt = db.prepare(o.sql);
    // Do not run if any read was denied (sensitive denials do not say what was blocked)
    if (seen.sensitive.size) return { ok: false, kind: 'sensitive', message: SENSITIVE_MESSAGE };
    if (seen.denied.size) return { ok: false, kind: 'sqlite', message: `tables not readable: ${[...seen.denied].sort().join(', ')}` };
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
        if (o.overflow === 'error') throw new Fail('limit', `result exceeds ${o.maxRows} rows`);
        more = true;
        break;
      }
      const tagged = row.map((v, i) => tag(v, columns[i].name, o, counter));
      size += Buffer.byteLength(JSON.stringify(tagged)) + 1;
      if (size > o.outputLimit) throw new Fail('limit', `result exceeds ${o.outputLimit} bytes`);
      rows.push(tagged);
    }
    return { ok: true, columns, rows, more, truncatedCells: counter.truncated, ms: Math.round(performance.now() - t0), tables };
  } catch (e) {
    if (e instanceof Fail) return { ok: false, kind: e.kind, message: e.message };
    const msg = (e as Error).message;
    if (/too large to be represented/i.test(msg)) return { ok: false, kind: 'type', message: 'integer outside the safe range' };
    // Sensitive denials do not say what was blocked
    if (seen.sensitive.size) return { ok: false, kind: 'sensitive', message: SENSITIVE_MESSAGE };
    if (seen.denied.size) return { ok: false, kind: 'sqlite', message: `tables not readable: ${[...seen.denied].sort().join(', ')}` };
    return { ok: false, kind: 'sqlite', message: msg };
  } finally {
    db.close();
  }
}

// When run as a child process
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
    if (n > INPUT_LIMIT) out = { ok: false, kind: 'input', message: 'input exceeds 64KB' };
    else out = runInWorker(JSON.parse(buf.subarray(0, n).toString('utf8')) as WorkerInput);
  } catch (e) {
    out = { ok: false, kind: 'input', message: (e as Error).message };
  }
  process.stdout.write(JSON.stringify(out));
}
