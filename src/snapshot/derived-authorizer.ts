// derived.sql permissions: may create and write d_* tables only (no views); other tables are read-only.
import { constants as C, type DatabaseSync } from 'node:sqlite';
import type { Role } from '../collect/spec.ts';

const SCHEMA_TABLE = 'sqlite_master';

const isDerived = (name: string | null) => name !== null && name.startsWith('d_');

export function derivedSqlAuthorizer(code: number, a1: string | null, a2: string | null, dbName: string | null): number {
  if (dbName !== null && dbName !== 'main') return C.SQLITE_DENY;
  switch (code) {
    case C.SQLITE_SELECT:
    case C.SQLITE_READ:
    case C.SQLITE_FUNCTION:
    case C.SQLITE_RECURSIVE:
    case C.SQLITE_TRANSACTION:
    case C.SQLITE_SAVEPOINT:
      return C.SQLITE_OK;
    case C.SQLITE_INSERT:
    case C.SQLITE_UPDATE:
    case C.SQLITE_DELETE:
      return isDerived(a1) || a1 === SCHEMA_TABLE ? C.SQLITE_OK : C.SQLITE_DENY;
    case C.SQLITE_CREATE_TABLE:
    case C.SQLITE_DROP_TABLE:
      return isDerived(a1) ? C.SQLITE_OK : C.SQLITE_DENY;
    case C.SQLITE_CREATE_INDEX:
    case C.SQLITE_DROP_INDEX:
      return isDerived(a2) ? C.SQLITE_OK : C.SQLITE_DENY;
    case C.SQLITE_REINDEX:
      return C.SQLITE_OK;
    default:
      return C.SQLITE_DENY;
  }
}

/** Splits statements on ; outside strings, identifiers and comments */
export function splitStatements(sql: string): string[] {
  const out: string[] = [];
  let start = 0;
  let i = 0;
  while (i < sql.length) {
    const c = sql[i];
    const n = sql[i + 1];
    if (c === '-' && n === '-') {
      const e = sql.indexOf('\n', i);
      i = e < 0 ? sql.length : e + 1;
    } else if (c === '/' && n === '*') {
      const e = sql.indexOf('*/', i + 2);
      i = e < 0 ? sql.length : e + 2;
    } else if (c === "'" || c === '"' || c === '`' || c === '[') {
      const close = c === '[' ? ']' : c;
      let j = i + 1;
      while (j < sql.length && !(sql[j] === close && sql[j + 1] !== close)) j += sql[j] === close ? 2 : 1;
      i = j + 1;
    } else if (c === ';') {
      out.push(sql.slice(start, i + 1));
      start = i + 1;
      i++;
    } else i++;
  }
  if (sql.slice(start).trim()) out.push(sql.slice(start));
  return out.filter((x) => x.replace(/--[^\n]*|\/\*[\s\S]*?\*\//g, '').trim().replace(/;$/, '').trim() !== '');
}

export class DerivedTaintError extends Error {}

/** d_ table written by a statement (target of CREATE TABLE, INSERT INTO, UPDATE, REPLACE INTO) */
function targetTable(stmt: string): string | null {
  const body = stmt.replace(/--[^\n]*|\/\*[\s\S]*?\*\//g, ' ');
  const m = /^\s*(?:CREATE\s+(?:TEMP\w*\s+)?TABLE(?:\s+IF\s+NOT\s+EXISTS)?|INSERT\s+(?:OR\s+\w+\s+)?INTO|REPLACE\s+INTO|UPDATE(?:\s+OR\s+\w+)?)\s+["`\[]?(d_[A-Za-z0-9_]*)/i.exec(body);
  return m ? m[1] : null;
}

/**
 * Runs derived SQL statement by statement, recording the private columns each target table reads.
 * A table that reads any private column must declare all its columns private (no exceptions).
 */
export function execDerivedSql(db: DatabaseSync, sql: string, roles?: Map<string, Role>): void {
  let readsPrivate: string[] = [];
  db.setAuthorizer((code, a1, a2, dbName) => {
    const r = derivedSqlAuthorizer(code, a1, a2, dbName);
    if (r === C.SQLITE_OK && code === C.SQLITE_READ && roles && a1 && a2 && roles.get(`${a1}.${a2}`) === 'private') readsPrivate.push(`${a1}.${a2}`);
    return r;
  });
  const tainted = new Map<string, Set<string>>();
  try {
    for (const stmt of splitStatements(sql)) {
      readsPrivate = [];
      db.exec(stmt);
      const t = targetTable(stmt);
      if (t && readsPrivate.length) {
        const set = tainted.get(t) ?? new Set();
        for (const x of readsPrivate) set.add(x);
        tainted.set(t, set);
      }
    }
  } finally {
    db.setAuthorizer(null);
  }
  if (!roles) return;
  const problems: string[] = [];
  for (const [t, from] of tainted) {
    const cols = (db.prepare(`PRAGMA table_info("${t}")`).all() as { name: string }[]).map((c) => c.name);
    const pub = cols.filter((c) => roles.get(`${t}.${c}`) !== 'private');
    if (pub.length) problems.push(`${t} reads private columns (${[...from].join(', ')}), so all its columns must be private: ${pub.map((c) => `${t}.${c}`).join(', ')}`);
  }
  if (problems.length) throw new DerivedTaintError(problems.join(' / '));
}
