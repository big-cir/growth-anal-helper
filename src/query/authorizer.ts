// Query permissions: reads of allowed-prefix tables and allowed functions only.
import { constants as C } from 'node:sqlite';

export const ALLOWED_FUNCTIONS = new Set([
  // aggregates
  'count', 'sum', 'total', 'avg', 'min', 'max', 'group_concat',
  // window functions
  'row_number', 'rank', 'dense_rank', 'lag', 'lead', 'first_value', 'last_value', 'ntile',
  // scalars (including LIKE and GLOB)
  'abs', 'coalesce', 'ifnull', 'nullif', 'iif', 'round', 'length', 'lower', 'upper', 'substr', 'trim', 'instr', 'replace', 'like', 'glob',
  // dates
  'date', 'time', 'datetime', 'julianday', 'strftime', 'unixepoch',
]);

export type Seen = { reads: Set<string>; denied: Set<string>; sensitive: Set<string> };
/** Columns ("table.column") and tables to block. Hits are recorded as sensitive denials */
export type Blocked = { columns: Set<string>; tables: Set<string> };

/**
 * reads: allowed table names, denied: denied table names, sensitive: sensitive denials (table.column)
 * cteOnly: WITH names (lowercase) that are not real schema objects. count(*) over a materialized WITH result
 * makes SQLite report a read of that name with column "" and DB null; it is not a real table, so it passes
 */
export function makeQueryAuthorizer(readablePrefixes: string[], seen?: Seen, blocked?: Blocked, cteOnly?: Set<string>) {
  const readable = (t: string | null) => t !== null && readablePrefixes.some((p) => t.startsWith(p));
  return (code: number, a1: string | null, a2: string | null, a3: string | null = null): number => {
    switch (code) {
      case C.SQLITE_SELECT:
        return C.SQLITE_OK;
      case C.SQLITE_READ:
        // When recording, denied reads return IGNORE so preparation continues; the query is rejected before running by checking seen
        if (a1 !== null && blocked && (blocked.tables.has(a1) || (a2 !== null && blocked.columns.has(`${a1}.${a2}`)))) {
          if (!seen) return C.SQLITE_DENY;
          seen.sensitive.add(a2 !== null ? `${a1}.${a2}` : a1);
          return C.SQLITE_IGNORE;
        }
        if (a1 !== null && a2 === '' && a3 === null && cteOnly?.has(a1.toLowerCase())) return C.SQLITE_OK;
        if (readable(a1)) {
          seen?.reads.add(a1!);
          return C.SQLITE_OK;
        }
        if (!seen || a1 === null) return C.SQLITE_DENY;
        seen.denied.add(a1);
        return C.SQLITE_IGNORE;
      case C.SQLITE_FUNCTION:
        return a2 !== null && ALLOWED_FUNCTIONS.has(a2.toLowerCase()) ? C.SQLITE_OK : C.SQLITE_DENY;
      default:
        return C.SQLITE_DENY;
    }
  };
}
