// 패널 사양 검증.

export type PatternType = 'number' | 'table' | 'line' | 'bar' | 'funnel' | 'cohort';

/** 패턴별 역할. 생략한 필수 역할은 같은 이름의 칸을 쓴다 */
export const ROLES: Record<PatternType, { required: string[]; optional: string[] }> = {
  number: { required: [], optional: ['numerator', 'denominator', 'value', 'label'] },
  table: { required: [], optional: [] },
  line: { required: ['x', 'numerator', 'denominator'], optional: ['series'] },
  bar: { required: ['x', 'numerator', 'denominator'], optional: ['series'] },
  funnel: { required: ['step_no', 'step_name', 'reached', 'eligible', 'unknown'], optional: ['cohort'] },
  cohort: { required: ['cohort', 'period', 'numerator', 'denominator'], optional: ['series', 'deleted_n'] },
};

export type Display = {
  type: PatternType;
  columns: Record<string, string | null>;
  /** 보조 수치 칸(0 이상 정수) */
  extra: string[];
  /** table 행 키 */
  key: string[];
  headline: { x?: string; series?: string | null } | null;
};

export type Answer = { question: string; answer: string; defaulted: boolean };

export type PanelSpec = {
  /** 지표 사전 id. null이면 사전 밖 */
  metric: string | null;
  title: string;
  question: string;
  sql: string;
  display: Display;
  definition: [string, string][];
  caveats: string[];
  answers: Answer[];
};

export class PanelSpecError extends Error {}

const TYPES: PatternType[] = ['number', 'table', 'line', 'bar', 'funnel', 'cohort'];
const COL = /^[A-Za-z_][A-Za-z0-9_]*$/;

function fail(path: string, msg: string): never {
  throw new PanelSpecError(`panel${path}: ${msg}`);
}
function str(v: unknown, path: string, max: number, { allowEmpty = false } = {}): string {
  if (typeof v !== 'string' || (!allowEmpty && v.trim() === '')) fail(path, '비어 있지 않은 문자열이어야 함');
  if ([...v].length > max) fail(path, `${max}자 이하`);
  return v;
}
function arr(v: unknown, path: string, min: number, max: number): unknown[] {
  if (!Array.isArray(v)) fail(path, '배열이어야 함');
  if (v.length < min || v.length > max) fail(path, `${min}~${max}개`);
  return v;
}
function colName(v: unknown, path: string): string {
  if (typeof v !== 'string' || !COL.test(v)) fail(path, '결과 칸 이름(식별자)이어야 함');
  return v;
}

export const PANEL_LIMITS = { title: 60, question: 300, sql: 12_000, definition: 10, definitionText: 300, caveats: 5, caveatText: 200, answers: 8 };

function parseDisplay(raw: unknown): Display {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) fail('.display', '객체여야 함');
  const d = raw as Record<string, unknown>;
  if (!TYPES.includes(d.type as PatternType)) fail('.display.type', TYPES.join('|'));
  const type = d.type as PatternType;
  const roles = ROLES[type];
  const allowedKeys = new Set(['type', 'extra', 'key', 'headline', ...roles.required, ...roles.optional]);
  for (const k of Object.keys(d)) if (!allowedKeys.has(k)) fail(`.display.${k}`, `${type} 패턴에 없는 키`);

  const columns: Record<string, string | null> = {};
  for (const r of roles.required) columns[r] = d[r] === undefined ? r : colName(d[r], `.display.${r}`);
  for (const r of roles.optional) columns[r] = d[r] === undefined || d[r] === null ? null : colName(d[r], `.display.${r}`);
  if (type === 'number') {
    const hasRate = d.numerator !== undefined || d.denominator !== undefined;
    if (hasRate) {
      columns.numerator = colName(d.numerator ?? 'numerator', '.display.numerator');
      columns.denominator = colName(d.denominator ?? 'denominator', '.display.denominator');
      if (d.value !== undefined && d.value !== null) fail('.display.value', 'numerator·denominator와 value는 함께 쓸 수 없음');
    } else if (d.value === undefined || d.value === null) {
      columns.numerator = 'numerator';
      columns.denominator = 'denominator';
    }
  }

  const extra = d.extra === undefined || d.extra === null ? [] : arr(d.extra, '.display.extra', 0, 6).map((v, i) => colName(v, `.display.extra[${i}]`));
  const key = d.key === undefined || d.key === null ? [] : arr(d.key, '.display.key', 0, 6).map((v, i) => colName(v, `.display.key[${i}]`));
  if (key.length && type !== 'table') fail('.display.key', 'table 패턴에서만 (다른 패턴은 행 키가 정해져 있음)');

  let headline: Display['headline'] = null;
  if (d.headline !== undefined && d.headline !== null) {
    if (typeof d.headline !== 'object' || Array.isArray(d.headline)) fail('.display.headline', '객체여야 함');
    const h = d.headline as Record<string, unknown>;
    for (const k of Object.keys(h)) if (k !== 'x' && k !== 'series') fail(`.display.headline.${k}`, '알 수 없는 키');
    if (type === 'table' || type === 'cohort') fail('.display.headline', `${type} 패턴은 대표 숫자가 없음`);
    headline = {
      ...(h.x !== undefined ? { x: str(h.x, '.display.headline.x', 100) } : {}),
      ...(h.series !== undefined ? { series: h.series === null ? null : str(h.series, '.display.headline.series', 100) } : {}),
    };
  }
  return { type, columns, extra, key, headline };
}

export function parsePanelSpec(raw: unknown): PanelSpec {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) fail('', '객체여야 함');
  const o = raw as Record<string, unknown>;
  const L = PANEL_LIMITS;
  if (o.metric !== undefined && o.metric !== null && (typeof o.metric !== 'string' || !/^[a-z][a-z0-9_]{0,47}$/.test(o.metric))) fail('.metric', '지표 사전 id 또는 null');
  return {
    metric: typeof o.metric === 'string' ? o.metric : null,
    title: str(o.title, '.title', L.title),
    question: str(o.question, '.question', L.question),
    sql: str(o.sql, '.sql', L.sql),
    display: parseDisplay(o.display),
    definition: arr(o.definition, '.definition', 1, L.definition).map((p, i) => {
      if (!Array.isArray(p) || p.length !== 2) fail(`.definition[${i}]`, '[항목, 내용] 쌍');
      return [str(p[0], `.definition[${i}][0]`, 40), str(p[1], `.definition[${i}][1]`, L.definitionText)] as [string, string];
    }),
    caveats: arr(o.caveats, '.caveats', 0, L.caveats).map((c, i) => str(c, `.caveats[${i}]`, L.caveatText)),
    answers: arr(o.answers, '.answers', 0, L.answers).map((a, i) => {
      if (!a || typeof a !== 'object' || Array.isArray(a)) fail(`.answers[${i}]`, '객체여야 함');
      const x = a as Record<string, unknown>;
      if (typeof x.defaulted !== 'boolean') fail(`.answers[${i}].defaulted`, 'true/false');
      return { question: str(x.question, `.answers[${i}].question`, 200), answer: str(x.answer, `.answers[${i}].answer`, 200), defaulted: x.defaulted };
    }),
  };
}

export function displayToJson(d: Display): Record<string, unknown> {
  const out: Record<string, unknown> = { type: d.type };
  for (const [k, v] of Object.entries(d.columns)) if (v !== null) out[k] = v;
  if (d.extra.length) out.extra = d.extra;
  if (d.key.length) out.key = d.key;
  if (d.headline) out.headline = d.headline;
  return out;
}

export const COMPARISON_CAVEAT = '관측 비교이며 인과효과가 아닙니다';
export const OFFDICT_CAVEAT = '지표 사전에 없는 정의로 만든 패널이에요';
