// Source adapter: streams results row by row.
export type RawValue = string | null;

export type SelectResult = { columns: string[]; rows: number; ms: number };

export type Dialect = 'mysql' | 'postgres' | 'sqlite';

export interface SourceAdapter {
  readonly dialect: Dialect;
  now(): Promise<string>;
  /** onColumns before the first row, onRow per row. Stops if a callback throws */
  selectStream(sql: string, onRow: (values: RawValue[]) => void, onColumns?: (columns: string[]) => void): Promise<SelectResult>;
  abort(): void;
}

/** Single-row, single-column result */
export async function singleValue(src: SourceAdapter, sql: string): Promise<string> {
  let value: RawValue = null;
  let rows = 0;
  await src.selectStream(sql, (v) => {
    if (++rows > 1) throw new Error(`more than one row: ${sql}`);
    value = v[0];
  });
  if (value === null) throw new Error(`no value: ${sql}`);
  return value;
}
