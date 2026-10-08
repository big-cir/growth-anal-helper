// 긴 설명(summary) 검사: 숫자 주장을 결과에서 다시 계산하고, 문장의 숫자·라벨이 결과나 정의에 있는지 본다.
import type { ResultColumn } from '../query/worker.ts';
import type { Row, Value } from './contract.ts';
import type { PanelSpec } from './spec.ts';

export type ClaimOp = 'value' | 'rate' | 'diff' | 'ratio' | 'sum';
export type ClaimRef = { row: { column: string; value: string }[]; column: string };
export type Claim = { id: string; op: ClaimOp; refs: ClaimRef[]; display: string };
export type SummaryDraft = { prose: string; claims: Claim[] };

export class SummaryError extends Error {}

const OPS: ClaimOp[] = ['value', 'rate', 'diff', 'ratio', 'sum'];
const REF_COUNT: Record<ClaimOp, (n: number) => boolean> = {
  value: (n) => n === 1,
  rate: (n) => n === 2,
  diff: (n) => n === 4,
  ratio: (n) => n === 4,
  sum: (n) => n >= 1,
};
const UNIT: Record<ClaimOp, string> = { value: '', sum: '', rate: '%', diff: '%p', ratio: '배' };

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => !!v && typeof v === 'object' && !Array.isArray(v);

export function parseSummary(raw: unknown): SummaryDraft {
  if (!isObj(raw)) throw new SummaryError('응답이 객체가 아님');
  if (typeof raw.prose !== 'string' || raw.prose.trim() === '') throw new SummaryError('prose가 비어 있음');
  if ([...raw.prose].length > 800) throw new SummaryError('prose는 800자 이하');
  if (!Array.isArray(raw.claims) || raw.claims.length > 12) throw new SummaryError('claims는 12개 이하 배열');
  const ids = new Set<string>();
  const claims = raw.claims.map((c, i): Claim => {
    const p = `claims[${i}]`;
    if (!isObj(c)) throw new SummaryError(`${p}: 객체가 아님`);
    if (typeof c.id !== 'string' || !/^c[0-9]{1,2}$/.test(c.id)) throw new SummaryError(`${p}.id: c1 같은 형식`);
    if (ids.has(c.id)) throw new SummaryError(`${p}.id: 중복 ${c.id}`);
    ids.add(c.id);
    if (!OPS.includes(c.op as ClaimOp)) throw new SummaryError(`${p}.op: ${OPS.join('|')}`);
    const op = c.op as ClaimOp;
    if (!Array.isArray(c.refs) || !REF_COUNT[op](c.refs.length)) throw new SummaryError(`${p}.refs: ${op}에 맞는 개수가 아님`);
    const refs = c.refs.map((r, j): ClaimRef => {
      if (!isObj(r) || typeof r.column !== 'string' || !Array.isArray(r.row)) throw new SummaryError(`${p}.refs[${j}]: { row, column } 형식`);
      const row = r.row.map((k) => {
        if (!isObj(k) || typeof k.column !== 'string' || typeof k.value !== 'string') throw new SummaryError(`${p}.refs[${j}].row: { column, value } 목록`);
        return { column: k.column, value: k.value };
      });
      return { row, column: r.column };
    });
    if (typeof c.display !== 'string' || c.display.trim() === '') throw new SummaryError(`${p}.display: 비어 있음`);
    return { id: c.id, op, refs, display: c.display };
  });
  return { prose: raw.prose, claims };
}

const show = (v: Value) => (v === null ? 'NULL' : String(v));

function cell(ref: ClaimRef, columns: ResultColumn[], rows: Row[]): number {
  const idx = new Map(columns.map((c, i) => [c.name, i]));
  for (const k of ref.row) if (!idx.has(k.column)) throw new SummaryError(`행 키 칸이 결과에 없음: ${k.column}`);
  if (!idx.has(ref.column)) throw new SummaryError(`칸이 결과에 없음: ${ref.column}`);
  const hits = rows.filter((r) => ref.row.every((k) => show(r[idx.get(k.column)!]) === k.value));
  const key = ref.row.map((k) => `${k.column}=${k.value}`).join(', ') || '(키 없음)';
  if (hits.length !== 1) throw new SummaryError(`행 키 ${key}에 맞는 행이 ${hits.length}개 (정확히 1개여야 함)`);
  const v = hits[0][idx.get(ref.column)!];
  if (typeof v !== 'number') throw new SummaryError(`${key}의 ${ref.column} 값이 수가 아님`);
  return v;
}

function compute(c: Claim, columns: ResultColumn[], rows: Row[]): number {
  const v = c.refs.map((r) => cell(r, columns, rows));
  const rate = (n: number, d: number) => {
    if (d === 0) throw new SummaryError(`${c.id}: 분모가 0`);
    return n / d;
  };
  switch (c.op) {
    case 'value':
      return v[0];
    case 'sum':
      return v.reduce((a, b) => a + b, 0);
    case 'rate':
      return rate(v[0], v[1]) * 100;
    case 'diff':
      return (rate(v[0], v[1]) - rate(v[2], v[3])) * 100;
    case 'ratio': {
      const b = rate(v[2], v[3]);
      if (b === 0) throw new SummaryError(`${c.id}: 비교 대상 비율이 0`);
      return rate(v[0], v[1]) / b;
    }
  }
}

const round = (x: number, d: number) => Math.round(x * 10 ** d) / 10 ** d;

/** 엔진이 문장에 넣는 값 */
export function formatClaim(op: ClaimOp, x: number): string {
  if ((op === 'value' || op === 'sum') && Number.isInteger(x)) return x.toLocaleString('en-US');
  return `${round(x, 1).toFixed(1)}${UNIT[op]}`;
}

/** display가 계산값과 반올림 규칙으로 맞는지. 맞으면 넣을 값, 아니면 null */
function matchDisplay(c: Claim, x: number): string | null {
  const m = /^([+\-−]?)\s*([0-9][0-9,]*(?:\.[0-9]+)?)\s*(%p|%|배|x)?$/.exec(c.display.trim());
  if (!m) return null;
  const unit = m[3] ?? '';
  if (unit !== UNIT[c.op] && !(c.op === 'ratio' && unit === 'x')) return null;
  const decimals = (m[2].split('.')[1] ?? '').length;
  const shown = Number(m[2].replace(/,/g, '')) * (m[1] && m[1] !== '+' ? -1 : 1);
  const integerOp = (c.op === 'value' || c.op === 'sum') && Number.isInteger(x);
  if (integerOp) return shown === x ? formatClaim(c.op, x) : null;
  if (decimals > 1) return null;
  if (round(x, decimals) === shown) return formatClaim(c.op, x);
  // 차이를 부호 없이 쓴 경우(문장에서 "낮다"로 방향을 말함)
  if (!m[1] && c.op === 'diff' && round(Math.abs(x), decimals) === shown) return formatClaim(c.op, Math.abs(x));
  return null;
}

/** 정의 문구의 숫자 라벨: 숫자와 붙은 단위까지 한 덩어리 */
const LABEL_TOKEN = /\d[\d.,:\-/]*(?:주차|개월|시간|번째|단계|일|주|개|년|월|분|초|명|건|회|차)?/g;
/** 비율·배수 단위. 이런 숫자는 claim으로만 쓴다 */
const RATE_UNIT = /^\s*(?:%|％|퍼센트|배|x\b)/;

/** 문장에 써도 되는 숫자 포함 라벨: 결과의 행 키 값, 정의 문구의 숫자 */
export function allowedLabels(spec: PanelSpec, columns: ResultColumn[], rows: Row[]): string[] {
  const d = spec.display;
  const idx = new Map(columns.map((c, i) => [c.name, i]));
  const keyCols = [d.columns.x, d.columns.series, d.columns.cohort, d.columns.step_name, d.columns.label, ...d.key].filter((c): c is string => !!c && idx.has(c));
  const out = new Set<string>();
  for (const r of rows) {
    for (const c of keyCols) {
      const v = r[idx.get(c)!];
      if (typeof v === 'string' && /\d/.test(v)) out.add(v);
      else if (typeof v === 'number' && Math.abs(v) >= 1000) out.add(String(v));
    }
    if (d.type === 'cohort' && d.columns.period && idx.has(d.columns.period)) {
      const p = r[idx.get(d.columns.period)!];
      if (typeof p === 'number') for (const s of [`W${p}`, `${p}주차`, `${p}주 차`]) out.add(s);
    }
  }
  const texts = [spec.title, spec.question, ...spec.definition.flat(), ...spec.answers.flatMap((a) => [a.question, a.answer])];
  for (const raw of texts) {
    const t = raw.normalize('NFKC');
    for (const m of t.matchAll(LABEL_TOKEN)) if (!RATE_UNIT.test(t.slice(m.index! + m[0].length))) out.add(m[0]);
  }
  return [...out].sort((a, b) => b.length - a.length);
}

/** 라벨로 덮이지 않는 숫자 */
export function strayNumbers(text: string, labels: string[]): string[] {
  const covered = new Array<boolean>(text.length).fill(false);
  for (const l of labels) {
    let i = text.indexOf(l);
    while (i >= 0) {
      for (let k = i; k < i + l.length; k++) covered[k] = true;
      i = text.indexOf(l, i + 1);
    }
  }
  const out: string[] = [];
  for (const m of text.matchAll(/\p{Nd}[\p{Nd}.,:\-/]*/gu)) {
    const at = m.index!;
    const rate = RATE_UNIT.test(text.slice(at + m[0].length));
    if (rate || [...m[0]].some((ch, k) => /\p{Nd}/u.test(ch) && !covered[at + k])) out.push(m[0]);
  }
  return out;
}

export type SummaryCheck = { ok: true; text: string } | { ok: false; problems: string[] };

export function checkSummary(draft: SummaryDraft, spec: PanelSpec, columns: ResultColumn[], rows: Row[]): SummaryCheck {
  const problems: string[] = [];
  const filled = new Map<string, string>();
  for (const c of draft.claims) {
    try {
      const x = compute(c, columns, rows);
      const v = matchDisplay(c, x);
      if (v === null) problems.push(`${c.id}: display ${c.display}가 계산값 ${formatClaim(c.op, x)}와 맞지 않음`);
      else filled.set(c.id, v);
    } catch (e) {
      if (!(e instanceof SummaryError)) throw e;
      problems.push(`${c.id}: ${e.message}`);
    }
  }
  const used = [...draft.prose.matchAll(/\{(c[0-9]{1,2})\}/g)].map((m) => m[1]);
  for (const id of new Set(used)) if (!draft.claims.some((c) => c.id === id)) problems.push(`문장의 {${id}}에 해당하는 claim이 없음`);
  const bare = draft.prose.normalize('NFKC').replace(/\{c[0-9]{1,2}\}/g, ' ');
  const stray = strayNumbers(bare, allowedLabels(spec, columns, rows));
  if (stray.length) problems.push(`문장에 claim 밖의 숫자나 결과·정의에 없는 라벨이 있음: ${[...new Set(stray)].slice(0, 8).join(', ')}`);
  if (problems.length) return { ok: false, problems };
  return { ok: true, text: draft.prose.replace(/\{(c[0-9]{1,2})\}/g, (_, id: string) => filled.get(id)!) };
}

/** 에이전트에게 보낼 결과: 200행 이하는 전체, 넘으면 결정적 요약 */
export function summaryResult(columns: ResultColumn[], rows: Row[]): Record<string, unknown> {
  const names = columns.map((c) => c.name);
  if (rows.length <= 200) return { columns: names, rows };
  const stats = names.map((name, i) => {
    const nums = rows.map((r) => r[i]).filter((v): v is number => typeof v === 'number');
    if (nums.length === 0) return { name };
    return { name, min: Math.min(...nums), max: Math.max(...nums), sum: nums.reduce((a, b) => a + b, 0) };
  });
  return { columns: names, row_count: rows.length, stats, first_rows: rows.slice(0, 20), last_rows: rows.slice(-20) };
}
