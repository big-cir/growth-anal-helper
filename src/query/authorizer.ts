// 쿼리 실행 권한: 허용 접두사 테이블 읽기와 허용 함수만.
import { constants as C } from 'node:sqlite';

export const ALLOWED_FUNCTIONS = new Set([
  // 집계
  'count', 'sum', 'total', 'avg', 'min', 'max', 'group_concat',
  // 창 함수
  'row_number', 'rank', 'dense_rank', 'lag', 'lead', 'first_value', 'last_value', 'ntile',
  // 스칼라 (LIKE·GLOB 포함)
  'abs', 'coalesce', 'ifnull', 'nullif', 'iif', 'round', 'length', 'lower', 'upper', 'substr', 'trim', 'instr', 'replace', 'like', 'glob',
  // 날짜
  'date', 'time', 'datetime', 'julianday', 'strftime', 'unixepoch',
]);

export type Seen = { reads: Set<string>; denied: Set<string>; sensitive: Set<string> };
/** 막을 칸("표.칸")과 표. 여기에 걸리면 민감 거부로 따로 기록한다 */
export type Blocked = { columns: Set<string>; tables: Set<string> };

/** reads: 승인한 표 이름, denied: 거부한 표 이름, sensitive: 민감 거부(표.칸) */
export function makeQueryAuthorizer(readablePrefixes: string[], seen?: Seen, blocked?: Blocked) {
  const readable = (t: string | null) => t !== null && readablePrefixes.some((p) => t.startsWith(p));
  return (code: number, a1: string | null, a2: string | null): number => {
    switch (code) {
      case C.SQLITE_SELECT:
        return C.SQLITE_OK;
      case C.SQLITE_READ:
        // 기록할 곳이 있으면 거부한 읽기도 끝까지 훑게 IGNORE로 두고, 실행 전에 seen을 보고 거부한다
        if (a1 !== null && blocked && (blocked.tables.has(a1) || (a2 !== null && blocked.columns.has(`${a1}.${a2}`)))) {
          if (!seen) return C.SQLITE_DENY;
          seen.sensitive.add(a2 !== null ? `${a1}.${a2}` : a1);
          return C.SQLITE_IGNORE;
        }
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
