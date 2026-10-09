// Pseudonymized copy for the agent: a new DB where identifier columns are mapped 1:1 to random values per domain, without private columns or engine bookkeeping tables.
import { DatabaseSync } from 'node:sqlite';
import { randomInt } from 'node:crypto';
import type { Role } from '../collect/spec.ts';

export class PseudonymizeError extends Error {}

export const PSEUDO_MIN = 1_000_000_000;
export const PSEUDO_MAX = 2 ** 47;

/** Tables and columns created by the engine (all ordinary) */
export const ENGINE_COLUMNS: Record<string, string[]> = {
  snapshot_meta: ['snapshot_id', 'source_cutoff_at', 'collection_started_at', 'collection_finished_at', 'raw_hash', 'derived_hash', 'roles_hash', 'params_hash', 'spec_hash', 'ga4_spec_hash'],
  snapshot_params: ['key', 'value'],
  r_collect_log: ['table_name', 'rows', 'ms', 'collected_at', 'dropped_after_cutoff', 'nulled_after_cutoff'],
  d_calendar_week: ['week_start', 'week_end'],
  r_ga4_collect_log: ['report', 'table_name', 'rows', 'row_count', 'dropped_small', 'dropped_unobserved', 'dropped_unmapped', 'dropped_collision', 'suppressed_cells', 'subject_to_thresholding', 'data_loss_from_other_row', 'sampled', 'sampling_summary', 'truncated', 'schema_restricted', 'empty_reason', 'time_zone', 'data_through', 'calls', 'quota_day_remaining', 'quota_hour_remaining', 'collected_at'],
};
export const ENGINE_TABLES = new Set(Object.keys(ENGINE_COLUMNS));
/** Engine bookkeeping tables left out of the agent copy */
export const AGENT_EXCLUDED_TABLES = new Set(['snapshot_meta', 'snapshot_params', 'r_collect_log', 'r_ga4_collect_log']);

type IdCol = { table: string; column: string; domain: string };

function q(name: string): string {
  return `"${name.replace(/"/g, '""')}"`;
}

function userTables(db: DatabaseSync): string[] {
  return (db.prepare("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY name").all() as { name: string }[]).map((r) => r.name);
}

function columnsOf(db: DatabaseSync, table: string): string[] {
  return (db.prepare(`PRAGMA table_info(${q(table)})`).all() as { name: string }[]).map((r) => r.name);
}

/** Matches every r_* and d_* column with its declared role and returns the identifier columns */
export function checkRoles(db: DatabaseSync, roles: Map<string, Role>): IdCol[] {
  const actual = new Set<string>();
  const missing: string[] = [];
  const ids: IdCol[] = [];
  const unknown = userTables(db).filter((t) => !ENGINE_TABLES.has(t) && !t.startsWith('r_') && !t.startsWith('d_'));
  if (unknown.length) throw new PseudonymizeError(`only r_*, d_* and engine tables are allowed (cannot tell how to pseudonymize): ${unknown.join(', ')}`);
  for (const t of userTables(db)) {
    if (ENGINE_TABLES.has(t)) {
      if (columnsOf(db, t).join(',') !== ENGINE_COLUMNS[t].join(',')) throw new PseudonymizeError(`columns of engine table ${t} changed (derived SQL cannot change engine tables)`);
      continue;
    }
    for (const c of columnsOf(db, t)) {
      const name = `${t}.${c}`;
      actual.add(name);
      const role = roles.get(name);
      if (role === undefined) missing.push(name);
      else if (typeof role === 'object') ids.push({ table: t, column: c, domain: role.identifier });
    }
  }
  const extra = [...roles.keys()].filter((k) => !actual.has(k));
  if (missing.length || extra.length) {
    throw new PseudonymizeError(
      [missing.length ? `columns without a declared role: ${missing.join(', ')}` : '', extra.length ? `declarations for missing columns: ${extra.join(', ')}` : '']
        .filter(Boolean).join(' / '),
    );
  }
  return ids;
}

function shuffle<T>(xs: T[]): T[] {
  for (let i = xs.length - 1; i > 0; i--) {
    const j = randomInt(i + 1);
    [xs[i], xs[j]] = [xs[j], xs[i]];
  }
  return xs;
}

export type PseudonymizeStats = { domains: Record<string, number>; tables: Record<string, number> };

export function pseudonymize(o: { srcPath: string; agentPath: string; mapPath: string; roles: Map<string, Role> }): PseudonymizeStats {
  const src = new DatabaseSync(o.srcPath, { readOnly: true });
  try {
    const ids = checkRoles(src, o.roles);

    const values = new Map<string, Set<number>>();
    for (const c of ids) {
      const set = values.get(c.domain) ?? new Set<number>();
      values.set(c.domain, set);
      const stmt = src.prepare(`SELECT DISTINCT ${q(c.column)} AS v FROM ${q(c.table)} WHERE ${q(c.column)} IS NOT NULL`);
      for (const r of stmt.iterate() as Iterable<{ v: unknown }>) {
        if (typeof r.v !== 'number' || !Number.isSafeInteger(r.v)) throw new PseudonymizeError(`${c.table}.${c.column}: non-integer ID value`);
        set.add(r.v);
      }
    }
    // Pseudonyms: large random integers, unique across all domains and never equal to a real value
    const allReal = new Set<number>();
    for (const set of values.values()) for (const v of set) allReal.add(v);
    const used = new Set<number>();
    const perm = new Map<string, Map<number, number>>();
    for (const [domain, set] of values) {
      const m = new Map<number, number>();
      for (const real of shuffle([...set])) {
        let p: number;
        do p = randomInt(PSEUDO_MIN, PSEUDO_MAX); while (used.has(p) || allReal.has(p));
        used.add(p);
        m.set(real, p);
      }
      perm.set(domain, m);
    }

    const map = new DatabaseSync(o.mapPath);
    try {
      map.exec('PRAGMA journal_mode = DELETE; CREATE TABLE pseudo_map (domain TEXT NOT NULL, real INTEGER NOT NULL, pseudo INTEGER NOT NULL, PRIMARY KEY (domain, real))');
      map.exec('BEGIN');
      const insMap = map.prepare('INSERT INTO pseudo_map VALUES (?, ?, ?)');
      for (const [domain, m] of perm) for (const [real, pseudo] of m) insMap.run(domain, real, pseudo);
      map.exec('COMMIT');
    } finally {
      map.close();
    }

    const dst = new DatabaseSync(o.agentPath);
    try {
      dst.exec('PRAGMA journal_mode = DELETE');
      // Table structure with public columns only
      const isPublic = (t: string, c: string) => ENGINE_TABLES.has(t) || o.roles.get(`${t}.${c}`) !== 'private';
      const kept = new Map<string, { name: string; type: string }[]>();
      for (const t of userTables(src)) {
        if (AGENT_EXCLUDED_TABLES.has(t)) continue;
        const cols = (src.prepare(`PRAGMA table_info(${q(t)})`).all() as { name: string; type: string }[]).filter((c) => isPublic(t, c.name));
        if (cols.length) kept.set(t, cols);
      }
      for (const [t, cols] of kept) dst.exec(`CREATE TABLE ${q(t)} (${cols.map((c) => `${q(c.name)}${c.type ? ` ${c.type}` : ''}`).join(', ')})`);
      const indexes = (src.prepare("SELECT name, tbl_name, sql FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL").all() as { name: string; tbl_name: string; sql: string }[])
        .filter((ix) => {
          const cols = kept.get(ix.tbl_name);
          if (!cols) return false;
          const used = (src.prepare(`PRAGMA index_info(${q(ix.name)})`).all() as { name: string | null }[]).map((c) => c.name);
          return used.every((c) => c !== null && cols.some((k) => k.name === c));
        });
      const tables: Record<string, number> = {};
      dst.exec('BEGIN');
      for (const [t, keptCols] of kept) {
        const cols = keptCols.map((c) => c.name);
        const mapAt = cols.map((c) => {
          const id = ids.find((x) => x.table === t && x.column === c);
          return id ? perm.get(id.domain)! : null;
        });
        const ins = dst.prepare(`INSERT INTO ${q(t)} VALUES (${cols.map(() => '?').join(', ')})`);
        const sel = src.prepare(`SELECT ${cols.map(q).join(', ')} FROM ${q(t)}`);
        sel.setReturnArrays(true);
        let n = 0;
        for (const row of sel.iterate() as Iterable<unknown[]>) {
          const out = row.map((v, i) => {
            const m = mapAt[i];
            if (m === null || v === null) return v;
            const p = m.get(v as number);
            if (p === undefined) throw new PseudonymizeError(`${t}.${cols[i]}: value not in the mapping (wrong declared domain)`);
            return p;
          });
          ins.run(...(out as never[]));
          n++;
        }
        tables[t] = n;
      }
      dst.exec('COMMIT');
      for (const ix of indexes) dst.exec(ix.sql);

      verify(src, dst, ids, tables);
      return { domains: Object.fromEntries([...perm].map(([d, m]) => [d, m.size])), tables };
    } finally {
      if (dst.isTransaction) dst.exec('ROLLBACK');
      dst.close();
    }
  } finally {
    src.close();
  }
}

function verify(src: DatabaseSync, dst: DatabaseSync, ids: IdCol[], tables: Record<string, number>): void {
  const one = (db: DatabaseSync, sql: string) => JSON.stringify(db.prepare(sql).all());
  for (const [t, n] of Object.entries(tables)) {
    const c = (dst.prepare(`SELECT count(*) n FROM ${q(t)}`).get() as { n: number }).n;
    if (c !== n) throw new PseudonymizeError(`check failed: ${t} row count ${c} ≠ ${n}`);
  }
  for (const c of ids) {
    const sql = `SELECT c, count(*) k FROM (SELECT count(*) c FROM ${q(c.table)} WHERE ${q(c.column)} IS NOT NULL GROUP BY ${q(c.column)}) GROUP BY c ORDER BY c`;
    if (one(src, sql) !== one(dst, sql)) throw new PseudonymizeError(`check failed: ${c.table}.${c.column} frequency distribution`);
  }
  for (let i = 0; i < ids.length; i++) {
    for (let j = i + 1; j < ids.length; j++) {
      const a = ids[i];
      const b = ids[j];
      if (a.domain !== b.domain) continue;
      const sql = `SELECT count(*) n FROM (SELECT DISTINCT ${q(a.column)} v FROM ${q(a.table)}) x JOIN (SELECT DISTINCT ${q(b.column)} v FROM ${q(b.table)}) y ON x.v = y.v`;
      if (one(src, sql) !== one(dst, sql)) throw new PseudonymizeError(`check failed: ${a.table}.${a.column} ↔ ${b.table}.${b.column} join`);
    }
  }
}
