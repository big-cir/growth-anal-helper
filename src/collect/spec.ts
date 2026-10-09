// Collection spec (tables.json) validation. Columns not in the spec are never collected.
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { sensitiveName } from './sensitive-names.ts';
import type { Dialect } from './sources/source.ts';

export type Kind = 'int' | 'ts' | 'text' | 'bool';
/** private: only in the real snapshot; hidden from the agent copy and queries */
export type Role = 'ordinary' | 'private' | { identifier: string };

export type ColumnSpec = {
  expr: string;          // identifier or `<identifier> IS [NOT] NULL`
  as: string;
  kind: Kind;
  role: Role;
  maxLength: number;     // text only
  nullAfterCutoff: boolean; // ts only: values after the cutoff become NULL
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
  if (typeof v !== 'string' || !IDENT.test(v)) fail(where, `must be an identifier (^[A-Za-z_][A-Za-z0-9_]*$)`);
  return v;
}

function parseRole(v: unknown, where: string): Role {
  if (v === 'ordinary' || v === 'private') return v;
  if (v && typeof v === 'object' && !Array.isArray(v)) {
    const keys = Object.keys(v);
    const dom = (v as Record<string, unknown>).identifier;
    if (keys.length === 1 && typeof dom === 'string' && IDENT.test(dom)) return { identifier: dom };
  }
  fail(where, 'role must be "ordinary" | "private" | { "identifier": "<domain>" } (required)');
}

export function parseSpec(raw: unknown): TableSpec[] {
  if (!Array.isArray(raw) || raw.length === 0) fail('(root)', 'must be an array of table specs');
  const targets = new Set<string>();
  return raw.map((t, ti) => {
    const w = `[${ti}]`;
    if (!t || typeof t !== 'object' || Array.isArray(t)) fail(w, 'must be an object');
    const o = t as Record<string, unknown>;
    for (const k of Object.keys(o)) if (!['source', 'target', 'key', 'cutoffColumn', 'columns'].includes(k)) fail(`${w}.${k}`, 'unknown key');
    const source = ident(o.source, `${w}.source`);
    const target = ident(o.target, `${w}.target`);
    if (!target.startsWith('r_')) fail(`${w}.target`, 'must start with r_');
    if (target.startsWith('r_ga4_')) fail(`${w}.target`, 'r_ga4_ is reserved for GA4 tables');
    if (targets.has(target)) fail(`${w}.target`, `duplicate: ${target}`);
    targets.add(target);

    if (!Array.isArray(o.columns) || o.columns.length === 0) fail(`${w}.columns`, 'must be a non-empty array');
    const seen = new Set<string>();
    const columns: ColumnSpec[] = o.columns.map((c, ci) => {
      const cw = `${w}.columns[${ci}]`;
      if (!c || typeof c !== 'object' || Array.isArray(c)) fail(cw, 'must be an object');
      const co = c as Record<string, unknown>;
      for (const k of Object.keys(co)) if (!['expr', 'as', 'kind', 'role', 'maxLength', 'nullAfterCutoff'].includes(k)) fail(`${cw}.${k}`, 'unknown key');
      if (typeof co.expr !== 'string' || !EXPR.test(co.expr.trim())) fail(`${cw}.expr`, 'only an identifier or `<identifier> IS [NOT] NULL`');
      const as = ident(co.as, `${cw}.as`);
      if (seen.has(as)) fail(`${cw}.as`, `duplicate: ${as}`);
      seen.add(as);
      if (!KINDS.includes(co.kind as Kind)) fail(`${cw}.kind`, `one of ${KINDS.join('|')}`);
      const kind = co.kind as Kind;
      const isNullExpr = /\s+IS\s+/i.test(co.expr);
      if (isNullExpr && kind !== 'bool') fail(`${cw}.kind`, 'IS [NOT] NULL expressions must be bool');
      const role = parseRole(co.role, `${cw}.role`);
      if (typeof role === 'object' && kind !== 'int') fail(`${cw}.role`, 'identifier is only allowed for kind = "int"');
      const hit = sensitiveName(co.expr) ?? sensitiveName(as) ?? sensitiveName(source);
      if (hit && role !== 'private') fail(`${cw}.role`, `looks sensitive (${hit}), so it can only be collected as "private"`);
      let maxLength = 64;
      if (co.maxLength !== undefined) {
        if (kind !== 'text') fail(`${cw}.maxLength`, 'only for text');
        if (typeof co.maxLength !== 'number' || !Number.isInteger(co.maxLength) || co.maxLength < 1 || co.maxLength > 4096) fail(`${cw}.maxLength`, 'integer 1-4096');
        maxLength = co.maxLength;
      }
      if (co.nullAfterCutoff !== undefined && typeof co.nullAfterCutoff !== 'boolean') fail(`${cw}.nullAfterCutoff`, 'true/false');
      const nullAfterCutoff = co.nullAfterCutoff === true;
      if (nullAfterCutoff && kind !== 'ts') fail(`${cw}.nullAfterCutoff`, 'only for ts');
      return { expr: co.expr.trim().replace(/\s+/g, ' '), as, kind, role, maxLength, nullAfterCutoff };
    });

    if (!Array.isArray(o.key) || o.key.length === 0) fail(`${w}.key`, 'must be a non-empty array');
    const key = o.key.map((k, ki) => {
      const name = ident(k, `${w}.key[${ki}]`);
      const col = columns.find((c) => c.as === name);
      if (!col) fail(`${w}.key[${ki}]`, `not among columns[].as: ${name}`);
      if (col.kind !== 'int') fail(`${w}.key[${ki}]`, 'keys must be int');
      return name;
    });
    if (new Set(key).size !== key.length) fail(`${w}.key`, 'duplicate');
    const cutoffColumn = ident(o.cutoffColumn, `${w}.cutoffColumn`);
    const cc = columns.find((c) => c.as === cutoffColumn);
    if (!cc) fail(`${w}.cutoffColumn`, `not among columns[].as: ${cutoffColumn}`);
    if (cc.kind !== 'ts') fail(`${w}.cutoffColumn`, 'must be ts');
    if (cc.nullAfterCutoff) fail(`${w}.cutoffColumn`, 'cannot be combined with nullAfterCutoff');
    return { source, target, key, cutoffColumn, columns };
  });
}

export function loadSpec(file: string): TableSpec[] {
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(file, 'utf8'));
  } catch (e) {
    throw new SpecError(`cannot read tables.json: ${(e as Error).message}`);
  }
  return parseSpec(raw);
}

/** Spec hash without roles */
export function specHash(specs: TableSpec[]): string {
  const stripped = specs.map((t) => ({ ...t, columns: t.columns.map(({ role: _r, ...c }) => c) }));
  return createHash('sha256').update(JSON.stringify(stripped)).digest('hex');
}

/** Collection SELECT. Timestamps and bools are cast to the engine format, since their text form differs by database */
export function selectSql(t: TableSpec, dialect: Dialect): string {
  const cols = t.columns.map((c) => `${columnExpr(c, dialect)} AS ${c.as}`);
  return `SELECT ${cols.join(', ')} FROM ${t.source} ORDER BY ${t.key.join(', ')}`;
}

function columnExpr(c: ColumnSpec, dialect: Dialect): string {
  if (dialect !== 'postgres') return c.expr;
  if (c.kind === 'ts') return `to_char(${c.expr}, 'YYYY-MM-DD HH24:MI:SS.US')`;
  if (c.kind === 'bool') return `CAST(${c.expr} AS int)`;
  return c.expr;
}
