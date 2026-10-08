// SQLite 파일 소스.
import { DatabaseSync } from 'node:sqlite';
import type { RawValue, SelectResult, SourceAdapter } from './source.ts';

export class SqliteSource implements SourceAdapter {
  readonly wrapText = false;
  private readonly path: string;

  constructor(path: string) {
    this.path = path;
  }

  private open(): DatabaseSync {
    return new DatabaseSync(this.path, { readOnly: true });
  }

  async now(): Promise<string> {
    const db = this.open();
    try {
      const row = db.prepare("SELECT strftime('%Y-%m-%d %H:%M:%f', 'now', 'localtime') AS now").get() as { now: string };
      return row.now;
    } finally {
      db.close();
    }
  }

  async selectStream(sql: string, onRow: (values: RawValue[]) => void, onColumns?: (columns: string[]) => void): Promise<SelectResult> {
    const t0 = performance.now();
    const db = this.open();
    try {
      const stmt = db.prepare(sql);
      const columns = stmt.columns().map((c) => c.name);
      onColumns?.(columns);
      stmt.setReturnArrays(true);
      let rows = 0;
      for (const row of stmt.iterate() as Iterable<unknown[]>) {
        onRow(row.map(toRaw));
        rows++;
      }
      return { columns, rows, ms: Math.round(performance.now() - t0) };
    } finally {
      db.close();
    }
  }

  abort(): void {
  }
}

function toRaw(v: unknown): RawValue {
  if (v === null || v === undefined) return null;
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'bigint') return String(v);
  throw new Error(`소스 값 형식을 지원하지 않음: ${typeof v}`);
}
