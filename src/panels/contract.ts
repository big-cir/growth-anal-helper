// 패턴별 결과 계약·불변식·대표 숫자.
import type { ResultColumn, Tagged } from '../query/worker.ts';
import { COMPARISON_CAVEAT, OFFDICT_CAVEAT, type PanelSpec } from './spec.ts';

/** 계약·불변식·대표 숫자 규칙이 바뀌면 올린다 */
export const PATTERN_CONTRACT_VERSION = 1;

export type Value = string | number | null;
export type Row = Value[];
/** 불변식 위반. index는 결과 안 순번(행 전체 위반이면 null) */
export type Violation = { row: string; problem: string; index: number | null; rule: string; column: string | null };

export class ContractError extends Error {}

export function columnIndex(columns: ResultColumn[]): Map<string, number> {
  const m = new Map<string, number>();
  columns.forEach((c, i) => {
    if (m.has(c.name)) throw new ContractError(`결과 칸 이름이 중복됨: ${c.name}`);
    m.set(c.name, i);
  });
  return m;
}

function usedColumns(spec: PanelSpec): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [role, col] of Object.entries(spec.display.columns)) if (col) out[role] = col;
  return out;
}

/** 필요한 칸이 결과에 있는지 */
export function checkContract(spec: PanelSpec, columns: ResultColumn[]): void {
  const idx = columnIndex(columns);
  const need = [...Object.values(usedColumns(spec)), ...spec.display.extra, ...spec.display.key];
  const missing = need.filter((c) => !idx.has(c));
  if (missing.length) {
    throw new ContractError(`${spec.display.type} 패턴에 필요한 칸이 결과에 없음: ${missing.join(', ')} (결과 칸: ${columns.map((c) => c.name).join(', ')})`);
  }
}

const KEY_ROLES: Record<string, string[]> = {
  line: ['x', 'series'],
  bar: ['x', 'series'],
  funnel: ['cohort', 'step_no'],
  cohort: ['cohort', 'series', 'period'],
  number: [],
};

const COUNT_ROLES: Record<string, string[]> = {
  number: ['numerator', 'denominator'],
  line: ['numerator', 'denominator'],
  bar: ['numerator', 'denominator'],
  funnel: ['reached', 'eligible', 'unknown'],
  cohort: ['numerator', 'denominator', 'deleted_n'],
  table: [],
};

const isCount = (v: Value) => typeof v === 'number' && Number.isInteger(v) && v >= 0;
const show = (v: Value) => (v === null ? 'NULL' : String(v));

export function untagRows(rows: Tagged[][]): Row[] {
  return rows.map((r) => r.map((t) => t[1]));
}

export function checkInvariants(spec: PanelSpec, columns: ResultColumn[], rows: Row[]): Violation[] {
  const idx = columnIndex(columns);
  const type = spec.display.type;
  const used = usedColumns(spec);
  const get = (row: Row, role: string): Value => row[idx.get(used[role])!];
  const keyRoles = type === 'table' ? [] : KEY_ROLES[type].filter((r) => used[r]);
  const keyCols = type === 'table' ? spec.display.key : keyRoles.map((r) => used[r]);
  const rowKey = (row: Row, i: number) =>
    keyCols.length ? keyCols.map((c) => `${c}=${show(row[idx.get(c)!])}`).join(', ') : `${i + 1}번째 행`;
  const out: Violation[] = [];
  const add = (row: Row, i: number, rule: string, column: string | null, problem: string) => out.push({ row: rowKey(row, i), problem, index: i, rule, column });
  const whole = (rule: string, column: string | null, problem: string) => out.push({ row: '-', problem, index: null, rule, column });

  if (rows.length === 0) return [{ row: '-', problem: '결과 행이 없음', index: null, rule: 'rows_nonempty', column: null }];
  if (type === 'number' && rows.length !== 1) return [{ row: '-', problem: `number 패턴은 행이 1개여야 함 (${rows.length}개)`, index: null, rule: 'number_one_row', column: null }];

  const countCols = [...COUNT_ROLES[type].filter((r) => used[r]).map((r) => used[r]), ...spec.display.extra];
  rows.forEach((row, i) => {
    for (const c of countCols) {
      const v = row[idx.get(c)!];
      if (!isCount(v)) add(row, i, 'count_nonneg_int', c, `${c}는 0 이상 정수여야 함 (${show(v)})`);
    }
    const [num, den] = type === 'funnel' ? ['reached', 'eligible'] : ['numerator', 'denominator'];
    if (used[num] && used[den]) {
      const a = get(row, num);
      const b = get(row, den);
      if (isCount(a) && isCount(b) && (a as number) > (b as number)) add(row, i, 'numerator_le_denominator', used[num], `${used[num]}(${a}) > ${used[den]}(${b})`);
    }
    if (type === 'number' && used.value) {
      const v = get(row, 'value');
      if (typeof v !== 'number') add(row, i, 'value_number', used.value, `value는 수여야 함 (${show(v)})`);
    }
  });

  if (keyCols.length) {
    const seen = new Map<string, number>();
    rows.forEach((row, i) => {
      const k = JSON.stringify(keyCols.map((c) => row[idx.get(c)!]));
      if (seen.has(k)) add(row, i, 'unique_row_key', null, `행 키가 중복됨 (${seen.get(k)! + 1}번째 행과 같음)`);
      else seen.set(k, i);
    });
  }

  if (type === 'line' || type === 'bar') {
    const xs = rows.map((r) => get(r, 'x'));
    if (xs.some((x) => x === null)) whole('x_not_null', used.x, 'x에 NULL이 있음');
    else if (new Set(xs.map((x) => typeof x)).size > 1) whole('x_sortable', used.x, 'x 값의 형식이 섞여 정렬할 수 없음 (수와 문자열)');
    if (used.series) {
      const series = new Set(rows.map((r) => show(get(r, 'series'))));
      if (series.size > 6) whole('series_max_6', used.series, `series가 ${series.size}개 (6개 이하)`);
    }
  }

  if (type === 'funnel') {
    const groups = new Map<string, { row: Row; i: number }[]>();
    rows.forEach((row, i) => {
      const g = used.cohort ? show(get(row, 'cohort')) : '';
      groups.set(g, [...(groups.get(g) ?? []), { row, i }]);
    });
    for (const list of groups.values()) {
      const bad = list.find(({ row }) => !Number.isInteger(get(row, 'step_no')));
      if (bad) {
        add(bad.row, bad.i, 'step_no_int', used.step_no, 'step_no는 정수여야 함');
        continue;
      }
      list.sort((a, b) => (get(a.row, 'step_no') as number) - (get(b.row, 'step_no') as number));
      list.forEach(({ row, i }, n) => {
        if (get(row, 'step_no') !== n + 1) add(row, i, 'steps_consecutive', used.step_no, `단계 번호가 1부터 연속이 아님 (${n + 1}이어야 함)`);
        if (n === 0) return;
        const prev = list[n - 1].row;
        const [r, pr, e, u] = [get(row, 'reached'), get(prev, 'reached'), get(row, 'eligible'), get(row, 'unknown')];
        if (![r, pr, e, u].every(isCount)) return;
        if ((r as number) > (pr as number)) add(row, i, 'reached_nonincreasing', used.reached, `도달(${r})이 앞 단계 도달(${pr})보다 많음`);
        if (e !== (pr as number) - (u as number)) add(row, i, 'eligible_is_prev_minus_unknown', used.eligible, `분모(${e}) ≠ 앞 단계 도달(${pr}) − 판정 불가(${u})`);
      });
    }
  }

  if (type === 'cohort') {
    const groups = new Map<string, { row: Row; i: number }[]>();
    rows.forEach((row, i) => {
      const p = get(row, 'period');
      if (!Number.isInteger(p) || (p as number) < 0) add(row, i, 'period_nonneg_int', used.period, `period는 0 이상 정수여야 함 (${show(p)})`);
      const g = JSON.stringify([get(row, 'cohort'), used.series ? get(row, 'series') : null]);
      groups.set(g, [...(groups.get(g) ?? []), { row, i }]);
    });
    for (const list of groups.values()) {
      const ok = list.filter(({ row }) => Number.isInteger(get(row, 'period')) && isCount(get(row, 'denominator')));
      ok.sort((a, b) => (get(a.row, 'period') as number) - (get(b.row, 'period') as number));
      for (let n = 1; n < ok.length; n++) {
        const [d, pd] = [get(ok[n].row, 'denominator') as number, get(ok[n - 1].row, 'denominator') as number];
        if (d > pd) add(ok[n].row, ok[n].i, 'denominator_nonincreasing', used.denominator, `period가 늘었는데 분모가 늘었음 (${pd} → ${d}, 관측 가능 조건 위반)`);
      }
    }
  }
  return out;
}

export function formatViolations(vs: Violation[], max = 10): string {
  const head = vs.slice(0, max).map((v) => `- ${v.row}: ${v.problem}`).join('\n');
  return vs.length > max ? `${head}\n- … 외 ${vs.length - max}건` : head;
}

/** 엔진 자동 문구를 앞에 붙인다: 사전 밖 → 관측 비교(series 2개 이상) */
export function effectiveCaveats(spec: PanelSpec, columns: ResultColumn[], rows: Row[]): string[] {
  const used = usedColumns(spec);
  const t = spec.display.type;
  let comparison = false;
  if ((t === 'line' || t === 'bar' || t === 'cohort') && used.series) {
    const i = columnIndex(columns).get(used.series)!;
    comparison = new Set(rows.map((r) => show(r[i]))).size >= 2;
  }
  const auto: string[] = [];
  if (spec.metric === null && !spec.caveats.some((c) => c.includes(OFFDICT_CAVEAT))) auto.push(OFFDICT_CAVEAT);
  if (comparison && !spec.caveats.some((c) => c.includes(COMPARISON_CAVEAT))) auto.push(COMPARISON_CAVEAT);
  return [...auto, ...spec.caveats];
}

export type Headline = { value: number; numerator: number | null; denominator: number | null; lowN: boolean; label: string } | null;

/** 대표 숫자: display.headline 지정이 없으면 패턴별 기본 규칙. 분모 30 미만은 lowN */
export function computeHeadline(spec: PanelSpec, columns: ResultColumn[], rows: Row[]): Headline {
  const t = spec.display.type;
  if (t === 'table' || t === 'cohort' || rows.length === 0) return null;
  const idx = columnIndex(columns);
  const used = usedColumns(spec);
  const get = (row: Row, role: string): Value => row[idx.get(used[role])!];
  const rate = (num: number, den: number, label: string): Headline => ({ value: den === 0 ? NaN : num / den, numerator: num, denominator: den, lowN: den < 30, label });
  const h = spec.display.headline;

  if (t === 'number') {
    const row = rows[0];
    if (used.value) return { value: get(row, 'value') as number, numerator: null, denominator: null, lowN: false, label: used.label ? show(get(row, 'label')) : '' };
    return rate(get(row, 'numerator') as number, get(row, 'denominator') as number, used.label ? show(get(row, 'label')) : '');
  }

  if (t === 'line' || t === 'bar') {
    const seriesOrder = used.series ? [...new Set(rows.map((r) => show(get(r, 'series'))))] : [''];
    const wantSeries = h && 'series' in h ? (h.series === null ? 'NULL' : h.series!) : seriesOrder[0];
    if (used.series && !seriesOrder.includes(wantSeries)) throw new ContractError(`대표 숫자로 지정한 series가 결과에 없음: ${wantSeries}`);
    const inSeries = rows.filter((r) => !used.series || show(get(r, 'series')) === wantSeries);
    let row: Row | undefined;
    if (h?.x !== undefined) {
      row = inSeries.find((r) => show(get(r, 'x')) === h.x);
      if (!row) throw new ContractError(`대표 숫자로 지정한 x가 결과에 없음: ${h.x}`);
    } else {
      row = [...inSeries].sort((a, b) => compareValues(get(a, 'x'), get(b, 'x'))).at(-1)!;
    }
    return rate(get(row, 'numerator') as number, get(row, 'denominator') as number, `${show(get(row, 'x'))}${used.series ? ` · ${wantSeries}` : ''}`);
  }

  // funnel: 마지막 단계의 처음 대비 비율(코호트가 있으면 'ALL' 행)
  let group = rows;
  if (used.cohort) {
    const want = h?.x ?? 'ALL';
    group = rows.filter((r) => show(get(r, 'cohort')) === want);
    if (group.length === 0) {
      if (h?.x !== undefined) throw new ContractError(`대표 숫자로 지정한 코호트가 결과에 없음: ${h.x}`);
      return null;
    }
  }
  const sorted = [...group].sort((a, b) => (get(a, 'step_no') as number) - (get(b, 'step_no') as number));
  const first = sorted[0];
  const last = sorted.at(-1)!;
  return rate(get(last, 'reached') as number, get(first, 'reached') as number, `${show(get(last, 'step_name'))} / ${show(get(first, 'step_name'))}`);
}

/** NULL < 수 < 문자열 */
export function compareValues(a: Value, b: Value): number {
  const rank = (v: Value) => (v === null ? 0 : typeof v === 'number' ? 1 : 2);
  if (rank(a) !== rank(b)) return rank(a) - rank(b);
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  if (typeof a === 'string' && typeof b === 'string') return a < b ? -1 : a > b ? 1 : 0;
  return 0;
}
