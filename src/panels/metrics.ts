// Metric dictionary (metrics.json): for each common metric, the tables and definition to use.
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

export type Metric = { id: string; name: string; asks: string[]; tables: string[]; definition: [string, string][]; seed_panel: string | null };
export type MetricDict = { dimension_tables: string[]; metrics: Metric[] };

export class MetricsError extends Error {}

export const METRICS_LIMITS = { metrics: 30, fileChars: 12_000, name: 40, asks: 6, askText: 100, tables: 8, definition: 10, definitionText: 300 };
const ID = /^[a-z][a-z0-9_]{0,47}$/;
const TABLE = /^[A-Za-z_][A-Za-z0-9_]*$/;

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => !!v && typeof v === 'object' && !Array.isArray(v);

function fail(path: string, msg: string): never {
  throw new MetricsError(`metrics.json${path}: ${msg}`);
}
function str(v: unknown, path: string, max: number): string {
  if (typeof v !== 'string' || v.trim() === '') fail(path, 'must be a non-empty string');
  if ([...v].length > max) fail(path, `at most ${max} characters`);
  return v;
}
function list(v: unknown, path: string, min: number, max: number): unknown[] {
  if (!Array.isArray(v) || v.length < min || v.length > max) fail(path, `must be an array of ${min}–${max} items`);
  return v;
}
function tableName(v: unknown, path: string, prefixes: string[]): string {
  if (typeof v !== 'string' || !TABLE.test(v)) fail(path, 'must be a table name');
  if (!prefixes.some((p) => v.startsWith(p))) fail(path, `must be a panel table (${prefixes.join(', ')}): ${v}`);
  return v;
}

export function parseMetrics(text: string, panelPrefixes: string[]): MetricDict {
  if (text.length > METRICS_LIMITS.fileChars) fail('', `at most ${METRICS_LIMITS.fileChars} characters`);
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (e) {
    fail('', `JSON error: ${(e as Error).message}`);
  }
  if (!isObj(raw)) fail('', 'must be an object');
  for (const k of Object.keys(raw)) if (k !== 'dimension_tables' && k !== 'metrics') fail(`.${k}`, 'unknown key');
  const dimension_tables = raw.dimension_tables === undefined ? [] : list(raw.dimension_tables, '.dimension_tables', 0, 8).map((t, i) => tableName(t, `.dimension_tables[${i}]`, panelPrefixes));
  const ids = new Set<string>();
  const metrics = list(raw.metrics, '.metrics', 0, METRICS_LIMITS.metrics).map((m, i): Metric => {
    const p = `.metrics[${i}]`;
    if (!isObj(m)) fail(p, 'must be an object');
    for (const k of Object.keys(m)) if (!['id', 'name', 'asks', 'tables', 'definition', 'seed_panel'].includes(k)) fail(`${p}.${k}`, 'unknown key');
    if (typeof m.id !== 'string' || !ID.test(m.id)) fail(`${p}.id`, '^[a-z][a-z0-9_]{0,47}$');
    if (ids.has(m.id)) fail(`${p}.id`, `duplicate: ${m.id}`);
    ids.add(m.id);
    const L = METRICS_LIMITS;
    const tables = list(m.tables, `${p}.tables`, 1, L.tables).map((t, j) => tableName(t, `${p}.tables[${j}]`, panelPrefixes));
    for (const t of tables) if (dimension_tables.includes(t)) fail(`${p}.tables`, `also in dimension_tables: ${t}`);
    return {
      id: m.id,
      name: str(m.name, `${p}.name`, L.name),
      asks: list(m.asks, `${p}.asks`, 1, L.asks).map((a, j) => str(a, `${p}.asks[${j}]`, L.askText)),
      tables: [...new Set(tables)],
      definition: list(m.definition, `${p}.definition`, 1, L.definition).map((d, j) => {
        if (!Array.isArray(d) || d.length !== 2) fail(`${p}.definition[${j}]`, 'must be an [item, text] pair');
        return [str(d[0], `${p}.definition[${j}][0]`, 40), str(d[1], `${p}.definition[${j}][1]`, L.definitionText)] as [string, string];
      }),
      seed_panel: m.seed_panel === undefined || m.seed_panel === null ? null : str(m.seed_panel, `${p}.seed_panel`, 64),
    };
  });
  return { dimension_tables, metrics };
}

/** Empty dictionary if the file is missing. Checks that seed panel metrics and dictionary seed_panel entries agree */
export function loadMetrics(wsDir: string, panelPrefixes: string[], seeds: { id: string; metric: string | null }[]): { dict: MetricDict; text: string } {
  const f = join(wsDir, 'metrics.json');
  if (!existsSync(f)) return { dict: { dimension_tables: [], metrics: [] }, text: '' };
  const text = readFileSync(f, 'utf8');
  const dict = parseMetrics(text, panelPrefixes);
  for (const m of dict.metrics) {
    if (m.seed_panel === null) continue;
    const s = seeds.find((x) => x.id === m.seed_panel);
    if (!s) fail(`(${m.id}).seed_panel`, `no such seed panel: ${m.seed_panel}`);
    if (s.metric !== m.id) fail(`(${m.id}).seed_panel`, `seed panel ${s.id} has a metric other than ${m.id}`);
  }
  for (const s of seeds) {
    if (s.metric !== null && !dict.metrics.some((m) => m.id === s.metric)) fail('', `seed panel ${s.id} has a metric not in the dictionary: ${s.metric}`);
  }
  return { dict, text };
}

/** Table rule for dictionary-metric panels. The reason if broken */
export function metricTablesProblem(dict: MetricDict, metric: string, tables: string[]): string | null {
  const m = dict.metrics.find((x) => x.id === metric);
  if (!m) return `metric not in the dictionary: ${metric} (use a dictionary id, or null for a definition outside the dictionary)`;
  const allowed = new Set([...m.tables, ...dict.dimension_tables]);
  const outside = tables.filter((t) => !allowed.has(t));
  if (outside.length) return `metric ${m.id} (${m.name}) may only use these tables: ${[...allowed].join(', ')}. Outside tables: ${outside.join(', ')}`;
  if (!m.tables.some((t) => tables.includes(t))) return `metric ${m.id} (${m.name}) must read at least one of its tables (${m.tables.join(', ')})`;
  return null;
}

export function metricsContext(dict: MetricDict): string {
  if (!dict.metrics.length) return '(no metrics in the dictionary; every panel uses metric: null)';
  const dims = dict.dimension_tables.length ? `Dimension tables (usable with every metric): ${dict.dimension_tables.join(', ')}\n\n` : '';
  return dims + dict.metrics.map((m) => [
    `### ${m.id}: ${m.name}`,
    `- Questions like: ${m.asks.join(' / ')}`,
    `- Tables: ${m.tables.join(', ')}`,
    ...m.definition.map(([k, v]) => `- ${k}: ${v}`),
    ...(m.seed_panel ? [`- Example panel: ${m.seed_panel}`] : []),
  ].join('\n')).join('\n\n');
}
