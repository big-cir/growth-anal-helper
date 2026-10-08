// 지표 사전(metrics.json): 자주 묻는 지표마다 써야 할 표와 정의.
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
  if (typeof v !== 'string' || v.trim() === '') fail(path, '비어 있지 않은 문자열');
  if ([...v].length > max) fail(path, `${max}자 이하`);
  return v;
}
function list(v: unknown, path: string, min: number, max: number): unknown[] {
  if (!Array.isArray(v) || v.length < min || v.length > max) fail(path, `${min}~${max}개 배열`);
  return v;
}
function tableName(v: unknown, path: string, prefixes: string[]): string {
  if (typeof v !== 'string' || !TABLE.test(v)) fail(path, '표 이름');
  if (!prefixes.some((p) => v.startsWith(p))) fail(path, `패널용 표(${prefixes.join(', ')})여야 함: ${v}`);
  return v;
}

export function parseMetrics(text: string, panelPrefixes: string[]): MetricDict {
  if (text.length > METRICS_LIMITS.fileChars) fail('', `${METRICS_LIMITS.fileChars}자 이하`);
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (e) {
    fail('', `JSON 오류: ${(e as Error).message}`);
  }
  if (!isObj(raw)) fail('', '객체여야 함');
  for (const k of Object.keys(raw)) if (k !== 'dimension_tables' && k !== 'metrics') fail(`.${k}`, '알 수 없는 키');
  const dimension_tables = raw.dimension_tables === undefined ? [] : list(raw.dimension_tables, '.dimension_tables', 0, 8).map((t, i) => tableName(t, `.dimension_tables[${i}]`, panelPrefixes));
  const ids = new Set<string>();
  const metrics = list(raw.metrics, '.metrics', 0, METRICS_LIMITS.metrics).map((m, i): Metric => {
    const p = `.metrics[${i}]`;
    if (!isObj(m)) fail(p, '객체여야 함');
    for (const k of Object.keys(m)) if (!['id', 'name', 'asks', 'tables', 'definition', 'seed_panel'].includes(k)) fail(`${p}.${k}`, '알 수 없는 키');
    if (typeof m.id !== 'string' || !ID.test(m.id)) fail(`${p}.id`, '^[a-z][a-z0-9_]{0,47}$');
    if (ids.has(m.id)) fail(`${p}.id`, `중복: ${m.id}`);
    ids.add(m.id);
    const L = METRICS_LIMITS;
    const tables = list(m.tables, `${p}.tables`, 1, L.tables).map((t, j) => tableName(t, `${p}.tables[${j}]`, panelPrefixes));
    for (const t of tables) if (dimension_tables.includes(t)) fail(`${p}.tables`, `dimension_tables와 겹침: ${t}`);
    return {
      id: m.id,
      name: str(m.name, `${p}.name`, L.name),
      asks: list(m.asks, `${p}.asks`, 1, L.asks).map((a, j) => str(a, `${p}.asks[${j}]`, L.askText)),
      tables: [...new Set(tables)],
      definition: list(m.definition, `${p}.definition`, 1, L.definition).map((d, j) => {
        if (!Array.isArray(d) || d.length !== 2) fail(`${p}.definition[${j}]`, '[항목, 내용] 쌍');
        return [str(d[0], `${p}.definition[${j}][0]`, 40), str(d[1], `${p}.definition[${j}][1]`, L.definitionText)] as [string, string];
      }),
      seed_panel: m.seed_panel === undefined || m.seed_panel === null ? null : str(m.seed_panel, `${p}.seed_panel`, 64),
    };
  });
  return { dimension_tables, metrics };
}

/** 파일이 없으면 빈 사전. 시드 패널의 metric과 사전의 seed_panel이 서로 맞는지 본다 */
export function loadMetrics(wsDir: string, panelPrefixes: string[], seeds: { id: string; metric: string | null }[]): { dict: MetricDict; text: string } {
  const f = join(wsDir, 'metrics.json');
  if (!existsSync(f)) return { dict: { dimension_tables: [], metrics: [] }, text: '' };
  const text = readFileSync(f, 'utf8');
  const dict = parseMetrics(text, panelPrefixes);
  for (const m of dict.metrics) {
    if (m.seed_panel === null) continue;
    const s = seeds.find((x) => x.id === m.seed_panel);
    if (!s) fail(`(${m.id}).seed_panel`, `시드 패널이 없음: ${m.seed_panel}`);
    if (s.metric !== m.id) fail(`(${m.id}).seed_panel`, `시드 패널 ${s.id}의 metric이 ${m.id}가 아님`);
  }
  for (const s of seeds) {
    if (s.metric !== null && !dict.metrics.some((m) => m.id === s.metric)) fail('', `시드 패널 ${s.id}의 metric이 사전에 없음: ${s.metric}`);
  }
  return { dict, text };
}

/** 사전 지표 패널이 참조한 표 규칙. 어기면 이유 */
export function metricTablesProblem(dict: MetricDict, metric: string, tables: string[]): string | null {
  const m = dict.metrics.find((x) => x.id === metric);
  if (!m) return `지표 사전에 없는 metric: ${metric} (사전 id 중 하나를 쓰거나, 사전에 없는 정의면 null)`;
  const allowed = new Set([...m.tables, ...dict.dimension_tables]);
  const outside = tables.filter((t) => !allowed.has(t));
  if (outside.length) return `지표 ${m.id}(${m.name})는 다음 표로만 만든다: ${[...allowed].join(', ')}. 벗어난 표: ${outside.join(', ')}`;
  if (!m.tables.some((t) => tables.includes(t))) return `지표 ${m.id}(${m.name})의 표(${m.tables.join(', ')}) 중 하나 이상을 읽어야 함`;
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
