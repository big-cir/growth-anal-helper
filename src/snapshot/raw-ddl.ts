// 명세에서 r_* DDL을 만든다.
import type { Kind, TableSpec } from '../collect/spec.ts';

const SQL_TYPE: Record<Kind, string> = { int: 'INTEGER', ts: 'TEXT', text: 'TEXT', bool: 'INTEGER' };

export function rawDdl(t: TableSpec): string {
  const cols = t.columns.map((c) => `${c.as} ${SQL_TYPE[c.kind]}${t.key.includes(c.as) ? ' NOT NULL' : ''}`);
  return `CREATE TABLE ${t.target} (${cols.join(', ')}, PRIMARY KEY (${t.key.join(', ')}))`;
}

export const ENGINE_TABLES_SQL = new URL('./engine-tables.sql', import.meta.url);
