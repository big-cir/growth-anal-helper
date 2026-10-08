// 데이터 품질 검사: 수집 명세로 만드는 엔진 내장 검사와 워크스페이스 quality/*.sql.
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { TableSpec } from '../collect/spec.ts';
import { normalizeTs } from '../time.ts';

export type QualityDisplay = 'table' | 'line';
export type QualityCheck = { id: string; title: string; display: QualityDisplay; sql: string; builtin: boolean };

export class QualityError extends Error {}

const q = (s: string) => `'${s.replace(/'/g, "''")}'`;

function tsParam(v: unknown, name: string): string {
  if (typeof v !== 'string') throw new QualityError(`params.${name}: 시각 문자열이어야 함`);
  try {
    return normalizeTs(v);
  } catch (e) {
    throw new QualityError(`params.${name}: ${(e as Error).message}`);
  }
}

/** params.quality_gap_ranges: ["시작~끝", …] */
function gapRanges(v: unknown): [string, string][] {
  if (v === undefined) return [];
  if (!Array.isArray(v)) throw new QualityError('params.quality_gap_ranges: "시작~끝" 문자열 배열');
  return v.map((s, i) => {
    const parts = typeof s === 'string' ? s.split('~').map((x) => x.trim()) : [];
    if (parts.length !== 2) throw new QualityError(`params.quality_gap_ranges[${i}]: "시작~끝" 형식`);
    return [tsParam(parts[0], `quality_gap_ranges[${i}]`), tsParam(parts[1], `quality_gap_ranges[${i}]`)];
  });
}

export function builtinChecks(specs: TableSpec[], params: Record<string, unknown>): QualityCheck[] {
  const counts = specs.map((t) => {
    const keyCol = t.columns.find((c) => c.as === t.key[0]);
    const k = t.key.length === 1 && keyCol?.role !== 'private' ? `"${t.key[0]}"` : 'NULL';
    return `SELECT ${q(t.target)} AS table_name, count(*) AS rows_now, min(${k}) AS min_key, max(${k}) AS max_key FROM "${t.target}"`;
  });
  const collect = `WITH t AS (\n  ${counts.join('\n  UNION ALL ')}\n)\nSELECT t.table_name, l.rows AS collected_rows, t.rows_now, t.min_key, t.max_key, l.ms AS collect_ms, l.collected_at\nFROM t LEFT JOIN r_collect_log l ON l.table_name = t.table_name\nORDER BY t.table_name`;

  const cutoff = 'SELECT table_name, dropped_after_cutoff, nulled_after_cutoff FROM r_collect_log ORDER BY table_name';

  const minTs = params.quality_min_ts === undefined ? null : tsParam(params.quality_min_ts, 'quality_min_ts');
  const gaps = gapRanges(params.quality_gap_ranges);
  const dateRows = specs.flatMap((t) => t.columns.filter((c) => c.kind === 'ts' && c.role !== 'private').map((c) => {
    const col = `"${c.as}"`;
    const parts = [
      `${q(t.target)} AS table_name`,
      `${q(c.as)} AS column_name`,
      `count(*) - count(${col}) AS nulls`,
      `coalesce(sum(${col} > :as_of), 0) AS after_as_of`,
      `${minTs ? `coalesce(sum(${col} < ${q(minTs)}), 0)` : 'NULL'} AS before_min`,
      `${gaps.length ? `coalesce(sum(${gaps.map(([a, b]) => `(${col} >= ${q(a)} AND ${col} < ${q(b)})`).join(' OR ')}), 0)` : 'NULL'} AS in_gap`,
    ];
    return `SELECT ${parts.join(', ')} FROM "${t.target}"`;
  }));
  const dates = dateRows.length ? dateRows.join('\nUNION ALL\n') : "SELECT NULL AS table_name WHERE 0";

  return [
    { id: 'q_collect', title: '테이블별 수집 결과', display: 'table', sql: collect, builtin: true },
    { id: 'q_cutoff_drops', title: '기준 시각 정리로 버리거나 비운 값', display: 'table', sql: cutoff, builtin: true },
    { id: 'q_dates', title: '날짜 칸 점검', display: 'table', sql: dates, builtin: true },
  ];
}

const ID = /^[a-z][a-z0-9_]{0,47}$/;

/** <workspace>/quality/<id>.sql + <id>.json({ title, display }) */
export function workspaceChecks(wsDir: string): QualityCheck[] {
  const dir = join(wsDir, 'quality');
  if (!existsSync(dir)) return [];
  return readdirSync(dir).filter((f) => f.endsWith('.sql')).sort().map((f) => {
    const id = f.slice(0, -4);
    if (!ID.test(id)) throw new QualityError(`quality/${f}: 파일 이름은 ^[a-z][a-z0-9_]*$`);
    const metaFile = join(dir, `${id}.json`);
    if (!existsSync(metaFile)) throw new QualityError(`quality/${id}.json이 없음`);
    const meta = JSON.parse(readFileSync(metaFile, 'utf8')) as Record<string, unknown>;
    if (typeof meta.title !== 'string' || meta.title.trim() === '') throw new QualityError(`quality/${id}.json: title 필요`);
    if (meta.display !== 'table' && meta.display !== 'line') throw new QualityError(`quality/${id}.json: display는 table|line`);
    return { id, title: meta.title, display: meta.display, sql: readFileSync(join(dir, f), 'utf8'), builtin: false };
  });
}
