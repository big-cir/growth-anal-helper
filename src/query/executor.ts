// SQL execution path: static checks → child process. No time limit; cancelled with an AbortSignal.
import { spawn } from 'node:child_process';
import { lintSql } from './sql-lint.ts';
import { INPUT_LIMIT, type ResultColumn, type Tagged, type WorkerInput, type WorkerOutput } from './worker.ts';

const WORKER = new URL('./worker.ts', import.meta.url).pathname;

export type QueryMode = 'probe' | 'panel';

export const LIMITS = {
  probe: { maxRows: 50, overflow: 'more', truncateCells: true },
  panel: { maxRows: 5000, overflow: 'error', truncateCells: false },
  cellBytes: 4 * 1024,
  outputBytes: 4 * 1024 * 1024,
} as const;

export type QueryRequest = {
  /** Slot obtained from ExecutionSlots */
  lease: SlotLease;
  sql: string;
  path: string;
  mode: QueryMode;
  asOf: string;
  params: Record<string, string | number | string[]>;
  readablePrefixes: string[];
  /** Columns and tables to deny as sensitive */
  blocked?: { columns: string[]; tables: string[] };
  heapLimitMb: number;
  signal?: AbortSignal;
};

export type QueryResult =
  | { ok: true; columns: ResultColumn[]; rows: Tagged[][]; more: boolean; truncatedCells: number; ms: number; tables: string[] }
  | { ok: false; kind: 'lint' | 'sqlite' | 'type' | 'limit' | 'input' | 'cancelled' | 'crash' | 'sensitive'; message: string };

export function scalarParams(params: QueryRequest['params']): Record<string, string | number> {
  const out: Record<string, string | number> = {};
  for (const [k, v] of Object.entries(params)) if (!Array.isArray(v)) out[k] = v;
  return out;
}

export function runQuery(req: QueryRequest): Promise<QueryResult> {
  if (!(req.lease instanceof SlotLease) || !req.lease[BEGIN]()) {
    return Promise.resolve({ ok: false, kind: 'input', message: 'cannot run a query without an execution slot (or two at once on the same slot)' });
  }
  return runWithLease(req).finally(() => req.lease[END]());
}

function runWithLease(req: QueryRequest): Promise<QueryResult> {
  const scalars = scalarParams(req.params);
  const lint = lintSql(req.sql, Object.keys(scalars));
  if (!lint.ok) return Promise.resolve({ ok: false, kind: 'lint', message: lint.message });
  if (req.signal?.aborted) return Promise.resolve({ ok: false, kind: 'cancelled', message: 'cancelled' });

  const bound: Record<string, string | number> = {};
  for (const p of lint.params) bound[p] = p === 'as_of' ? req.asOf : scalars[p];
  const lim = LIMITS[req.mode];
  const input: WorkerInput = {
    path: req.path,
    sql: req.sql,
    params: bound,
    readablePrefixes: req.readablePrefixes,
    blockedColumns: req.blocked?.columns,
    blockedTables: req.blocked?.tables,
    heapLimitMb: req.heapLimitMb,
    maxRows: lim.maxRows,
    overflow: lim.overflow,
    cellLimit: LIMITS.cellBytes,
    truncateCells: lim.truncateCells,
    outputLimit: LIMITS.outputBytes - 64 * 1024,
  };
  const payload = JSON.stringify(input);
  if (Buffer.byteLength(payload) > INPUT_LIMIT) return Promise.resolve({ ok: false, kind: 'input', message: 'SQL and path exceed 64KB' });

  return new Promise((resolve) => {
    const child = spawn(process.execPath, [WORKER], { detached: true, stdio: ['pipe', 'pipe', 'pipe'] });
    let settled = false;
    let pending: QueryResult | null = null;
    const chunks: Buffer[] = [];
    let outBytes = 0;
    let errText = '';
    const finish = (r: QueryResult) => {
      if (settled) return;
      settled = true;
      req.signal?.removeEventListener('abort', onAbort);
      resolve(r);
    };
    const killGroup = () => {
      try {
        process.kill(-child.pid!, 'SIGKILL');
      } catch {
        // already exited
      }
    };
    const onAbort = () => {
      killGroup();
    };
    req.signal?.addEventListener('abort', onAbort, { once: true });

    child.stdout!.on('data', (b: Buffer) => {
      if (pending) return;
      outBytes += b.length;
      if (outBytes > LIMITS.outputBytes) {
        pending = { ok: false, kind: 'limit', message: 'result output exceeds the limit' };
        chunks.length = 0;
        killGroup();
        return;
      }
      chunks.push(b);
    });
    child.stderr!.on('data', (b: Buffer) => {
      if (errText.length < 4096) errText += b.toString('utf8');
    });
    child.on('error', (e) => finish({ ok: false, kind: 'crash', message: e.message }));
    child.on('close', (code, signal) => {
      if (pending) return finish(pending);
      if (req.signal?.aborted) return finish({ ok: false, kind: 'cancelled', message: 'cancelled' });
      if (code !== 0) {
        const oom = /out of memory|heap/i.test(errText);
        return finish({ ok: false, kind: oom ? 'limit' : 'crash', message: oom ? `exceeded the memory limit (${req.heapLimitMb}MB)` : `worker exited abnormally (${code ?? signal})` });
      }
      try {
        const r = JSON.parse(Buffer.concat(chunks).toString('utf8')) as WorkerOutput;
        finish(r.ok ? r : { ok: false, kind: r.kind, message: r.message });
      } catch {
        finish({ ok: false, kind: 'crash', message: 'cannot read worker output' });
      }
    });
    child.stdin!.on('error', () => {});
    child.stdin!.end(payload);
  });
}

export function untag(v: Tagged): string | number | null {
  return v[1];
}

export class SlotCancelled extends Error {}

const ISSUER = Symbol('ExecutionSlots');
const BEGIN = Symbol('begin');
const END = Symbol('end');

/** A held execution slot. Runs one query at a time */
export class SlotLease {
  readonly kind: 'interactive' | 'background';
  #active = true;
  #busy = false;
  #releaseRequested = false;
  readonly #onRelease: () => void;

  constructor(token: symbol, kind: 'interactive' | 'background', onRelease: () => void) {
    if (token !== ISSUER) throw new Error('SlotLease can only be obtained from ExecutionSlots.acquire');
    this.kind = kind;
    this.#onRelease = onRelease;
  }

  get active(): boolean {
    return this.#active;
  }

  /** If a query is running, released when it ends */
  release(): void {
    if (!this.#active) return;
    if (this.#busy) {
      this.#releaseRequested = true;
      return;
    }
    this.#active = false;
    this.#onRelease();
  }

  [BEGIN](): boolean {
    if (!this.#active || this.#busy || this.#releaseRequested) return false;
    this.#busy = true;
    return true;
  }

  [END](): void {
    this.#busy = false;
    if (this.#releaseRequested) {
      this.#releaseRequested = false;
      this.release();
    }
  }
}

/** Execution slots shared by agent calls and queries. Interactive requests first; at most backgroundMax background jobs */
export class ExecutionSlots {
  private readonly total: number;
  private readonly backgroundMax: number;
  private inUse = 0;
  private bgInUse = 0;
  private readonly waiting: { kind: 'interactive' | 'background'; grant: () => void; reject: (e: Error) => void }[] = [];

  constructor(total = 2, backgroundMax = 1) {
    this.total = total;
    this.backgroundMax = backgroundMax;
  }

  async run<T>(kind: 'interactive' | 'background', signal: AbortSignal | undefined, fn: (lease: SlotLease) => Promise<T>): Promise<T> {
    const lease = await this.acquire(kind, signal);
    try {
      return await fn(lease);
    } finally {
      lease.release();
    }
  }

  get stats() {
    return { inUse: this.inUse, backgroundInUse: this.bgInUse, waiting: this.waiting.length };
  }

  acquire(kind: 'interactive' | 'background', signal?: AbortSignal): Promise<SlotLease> {
    if (signal?.aborted) return Promise.reject(new SlotCancelled('cancelled'));
    return new Promise((resolve, reject) => {
      const entry = {
        kind,
        grant: () => {
          signal?.removeEventListener('abort', onAbort);
          this.inUse++;
          if (kind === 'background') this.bgInUse++;
          resolve(new SlotLease(ISSUER, kind, () => {
            this.inUse--;
            if (kind === 'background') this.bgInUse--;
            this.pump();
          }));
        },
        reject,
      };
      const onAbort = () => {
        const i = this.waiting.indexOf(entry);
        if (i >= 0) this.waiting.splice(i, 1);
        reject(new SlotCancelled('cancelled'));
      };
      signal?.addEventListener('abort', onAbort, { once: true });
      this.waiting.push(entry);
      this.pump();
    });
  }

  private pump(): void {
    while (this.inUse < this.total) {
      let i = this.waiting.findIndex((w) => w.kind === 'interactive');
      if (i < 0 && this.bgInUse < this.backgroundMax) i = this.waiting.findIndex((w) => w.kind === 'background');
      if (i < 0) return;
      const [w] = this.waiting.splice(i, 1);
      w.grant();
    }
  }
}
