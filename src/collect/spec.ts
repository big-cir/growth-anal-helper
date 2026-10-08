// 수집 명세(tables.json) 검증. 명세에 없는 칸은 수집하지 않는다.
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { sensitiveName } from './sensitive-names.ts';

export type Kind = 'int' | 'ts' | 'text' | 'bool';
/** private: 원본 스냅샷에만 있고 에이전트 사본·쿼리에서는 보이지 않는다 */
export type Role = 'ordinary' | 'private' | { identifier: string };

export type ColumnSpec = {
  expr: string;          // 식별자 또는 `<식별자> IS [NOT] NULL`
  as: string;
  kind: Kind;
  role: Role;
  maxLength: number;     // text 전용
  nullAfterCutoff: boolean; // ts 전용: 기준 시각 뒤 값은 NULL로
};

export type TableSpec = {
  source: string;
  target: string;
  key: string[];
  cutoffColumn: string;
  columns: ColumnSpec[];
};

const IDENT = /^[A-Za-z_][A-Za-z0-9_]*$/;
const EXPR = /^([A-Za-z_][A-Za-z0-9_]*)(?:\s+IS\s+(NOT\s+)?NULL)?$/i;
const KINDS: Kind[] = ['int', 'ts', 'text', 'bool'];

export class SpecError extends Error {}

function fail(where: string, msg: string): never {
  throw new SpecError(`tables.json ${where}: ${msg}`);
}

function ident(v: unknown, where: string): string {
  if (typeof v !== 'string' || !IDENT.test(v)) fail(where, `식별자(^[A-Za-z_][A-Za-z0-9_]*$)여야 함`);
  return v;
}

function parseRole(v: unknown, where: string): Role {
  if (v === 'ordinary' || v === 'private') return v;
  if (v && typeof v === 'object' && !Array.isArray(v)) {
    const keys = Object.keys(v);
    const dom = (v as Record<string, unknown>).identifier;
    if (keys.length === 1 && typeof dom === 'string' && IDENT.test(dom)) return { identifier: dom };
  }
  fail(where, 'role은 "ordinary" | "private" | { "identifier": "<도메인>" } (필수)');
}

export function parseSpec(raw: unknown): TableSpec[] {
  if (!Array.isArray(raw) || raw.length === 0) fail('(root)', '테이블 명세 배열이어야 함');
  const targets = new Set<string>();
  return raw.map((t, ti) => {
    const w = `[${ti}]`;
    if (!t || typeof t !== 'object' || Array.isArray(t)) fail(w, '객체여야 함');
    const o = t as Record<string, unknown>;
    for (const k of Object.keys(o)) if (!['source', 'target', 'key', 'cutoffColumn', 'columns'].includes(k)) fail(`${w}.${k}`, '알 수 없는 키');
    const source = ident(o.source, `${w}.source`);
    const target = ident(o.target, `${w}.target`);
    if (!target.startsWith('r_')) fail(`${w}.target`, 'r_로 시작해야 함');
    if (targets.has(target)) fail(`${w}.target`, `중복: ${target}`);
    targets.add(target);

    if (!Array.isArray(o.columns) || o.columns.length === 0) fail(`${w}.columns`, '비어 있지 않은 배열이어야 함');
    const seen = new Set<string>();
    const columns: ColumnSpec[] = o.columns.map((c, ci) => {
      const cw = `${w}.columns[${ci}]`;
      if (!c || typeof c !== 'object' || Array.isArray(c)) fail(cw, '객체여야 함');
      const co = c as Record<string, unknown>;
      for (const k of Object.keys(co)) if (!['expr', 'as', 'kind', 'role', 'maxLength', 'nullAfterCutoff'].includes(k)) fail(`${cw}.${k}`, '알 수 없는 키');
      if (typeof co.expr !== 'string' || !EXPR.test(co.expr.trim())) fail(`${cw}.expr`, '식별자 하나 또는 `<식별자> IS [NOT] NULL`만 허용');
      const as = ident(co.as, `${cw}.as`);
      if (seen.has(as)) fail(`${cw}.as`, `중복: ${as}`);
      seen.add(as);
      if (!KINDS.includes(co.kind as Kind)) fail(`${cw}.kind`, `${KINDS.join('|')} 중 하나`);
      const kind = co.kind as Kind;
      const isNullExpr = /\s+IS\s+/i.test(co.expr);
      if (isNullExpr && kind !== 'bool') fail(`${cw}.kind`, 'IS [NOT] NULL 식은 bool이어야 함');
      const role = parseRole(co.role, `${cw}.role`);
      if (typeof role === 'object' && kind !== 'int') fail(`${cw}.role`, 'identifier는 kind = "int"에만 허용');
      const hit = sensitiveName(co.expr) ?? sensitiveName(as) ?? sensitiveName(source);
      if (hit && role !== 'private') fail(`${cw}.role`, `민감해 보이는 이름(${hit})이라 "private"로만 수집할 수 있음`);
      let maxLength = 64;
      if (co.maxLength !== undefined) {
        if (kind !== 'text') fail(`${cw}.maxLength`, 'text에만 쓸 수 있음');
        if (typeof co.maxLength !== 'number' || !Number.isInteger(co.maxLength) || co.maxLength < 1 || co.maxLength > 4096) fail(`${cw}.maxLength`, '1~4096 정수');
        maxLength = co.maxLength;
      }
      if (co.nullAfterCutoff !== undefined && typeof co.nullAfterCutoff !== 'boolean') fail(`${cw}.nullAfterCutoff`, 'true/false');
      const nullAfterCutoff = co.nullAfterCutoff === true;
      if (nullAfterCutoff && kind !== 'ts') fail(`${cw}.nullAfterCutoff`, 'ts에만 쓸 수 있음');
      return { expr: co.expr.trim().replace(/\s+/g, ' '), as, kind, role, maxLength, nullAfterCutoff };
    });

    if (!Array.isArray(o.key) || o.key.length === 0) fail(`${w}.key`, '비어 있지 않은 배열이어야 함');
    const key = o.key.map((k, ki) => {
      const name = ident(k, `${w}.key[${ki}]`);
      const col = columns.find((c) => c.as === name);
      if (!col) fail(`${w}.key[${ki}]`, `columns의 as에 없음: ${name}`);
      if (col.kind !== 'int') fail(`${w}.key[${ki}]`, '키는 int만 지원');
      return name;
    });
    if (new Set(key).size !== key.length) fail(`${w}.key`, '중복');
    const cutoffColumn = ident(o.cutoffColumn, `${w}.cutoffColumn`);
    const cc = columns.find((c) => c.as === cutoffColumn);
    if (!cc) fail(`${w}.cutoffColumn`, `columns의 as에 없음: ${cutoffColumn}`);
    if (cc.kind !== 'ts') fail(`${w}.cutoffColumn`, 'ts여야 함');
    if (cc.nullAfterCutoff) fail(`${w}.cutoffColumn`, 'nullAfterCutoff와 함께 쓸 수 없음');
    return { source, target, key, cutoffColumn, columns };
  });
}

export function loadSpec(file: string): TableSpec[] {
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(file, 'utf8'));
  } catch (e) {
    throw new SpecError(`tables.json을 읽지 못함: ${(e as Error).message}`);
  }
  return parseSpec(raw);
}

/** 역할(role)을 뺀 명세 해시 */
export function specHash(specs: TableSpec[]): string {
  const stripped = specs.map((t) => ({ ...t, columns: t.columns.map(({ role: _r, ...c }) => c) }));
  return createHash('sha256').update(JSON.stringify(stripped)).digest('hex');
}

/** 수집 SELECT. wrapText면 text 칸에 's' 접두사를 붙여 문자열 "NULL"과 NULL을 구분한다 */
export function selectSql(t: TableSpec, wrapText: boolean): string {
  const cols = t.columns.map((c) => (wrapText && c.kind === 'text' ? `CONCAT('s', ${c.expr}) AS ${c.as}` : `${c.expr} AS ${c.as}`));
  return `SELECT ${cols.join(', ')} FROM ${t.source} ORDER BY ${t.key.join(', ')}`;
}
