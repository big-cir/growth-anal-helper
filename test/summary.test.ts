// Summary check and recompute decisions.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parsePanelSpec } from '../src/panels/spec.ts';
import { checkSummary, parseSummary, strayNumbers, summaryResult, SummaryError, type SummaryDraft } from '../src/panels/summary-check.ts';
import { decide } from '../src/panels/recompute.ts';
import type { PanelVersions, SavedPanel } from '../src/panels/store.ts';
import type { ResultColumn } from '../src/query/worker.ts';
import { setLanguage } from '../src/i18n.ts';

const col = (name: string): ResultColumn => ({ name, table: null, column: null });
const display = { type: 'line', x: 'x', numerator: 'numerator', denominator: 'denominator', series: 'series' };
const spec = parsePanelSpec({
  title: '7-day join rate by signup week', question: 'What is the 7-day join rate of signups in the last 8 weeks?', sql: 'SELECT 1',
  display, definition: [['Period', '7 days after signup']], caveats: [], answers: [{ question: 'Window', answer: '7 days', defaulted: true }],
});
const koSpec = parsePanelSpec({
  title: '가입 주별 7일 참여율', question: '최근 8주 가입자의 7일 내 참여율은?', sql: 'SELECT 1',
  display, definition: [['기간', '가입 후 7일']], caveats: [], answers: [{ question: '창', answer: '7일', defaulted: true }],
});
const columns = ['x', 'series', 'numerator', 'denominator'].map(col);
const rows = [
  ['2024-05-06', 'A', 30, 100],
  ['2024-05-13', 'A', 45, 90],
  ['2024-05-13', 'B', 20, 80],
];
const ref = (x: string, s: string, c: string) => ({ row: [{ column: 'x', value: x }, { column: 'series', value: s }], column: c });
const draft = (prose: string, claims: SummaryDraft['claims']): SummaryDraft => ({ prose, claims });
const inKorean = (fn: () => void) => {
  setLanguage('ko');
  try {
    fn();
  } finally {
    setLanguage('en');
  }
};
const claims = (diff: string, ratio: string): SummaryDraft['claims'] => [
  { id: 'c1', op: 'rate', refs: [ref('2024-05-13', 'A', 'numerator'), ref('2024-05-13', 'A', 'denominator')], display: '50.0%' },
  { id: 'c2', op: 'diff', refs: [ref('2024-05-13', 'A', 'numerator'), ref('2024-05-13', 'A', 'denominator'), ref('2024-05-13', 'B', 'numerator'), ref('2024-05-13', 'B', 'denominator')], display: diff },
  { id: 'c3', op: 'ratio', refs: [ref('2024-05-13', 'A', 'numerator'), ref('2024-05-13', 'A', 'denominator'), ref('2024-05-13', 'B', 'numerator'), ref('2024-05-13', 'B', 'denominator')], display: ratio },
  { id: 'c4', op: 'sum', refs: [ref('2024-05-06', 'A', 'denominator'), ref('2024-05-13', 'A', 'denominator')], display: '190' },
];

test('claims are recomputed from the result and inserted when they match', () => {
  const d = draft('In the week of 2024-05-13, A joined at {c1}, {c2} above B and {c3} as high. Signups over both weeks total {c4}.', claims('25pp', '2.0x'));
  assert.deepEqual(checkSummary(d, spec, columns, rows), { ok: true, text: 'In the week of 2024-05-13, A joined at 50.0%, 25.0pp above B and 2.0x as high. Signups over both weeks total 190.' });
});

test('Korean: units are %p and 배, and either unit style is accepted in display', () => {
  inKorean(() => {
    const d = draft('2024-05-13 주 A의 참여율은 {c1}로, B보다 {c2} 높고 {c3}입니다. 두 주 합계 가입자는 {c4}명입니다.', claims('25%p', '2.0x'));
    assert.deepEqual(checkSummary(d, koSpec, columns, rows), { ok: true, text: '2024-05-13 주 A의 참여율은 50.0%로, B보다 25.0%p 높고 2.0배입니다. 두 주 합계 가입자는 190명입니다.' });
  });
});

test('wrong values, missing rows or columns, and numbers outside claims are reported', () => {
  const bad = checkSummary(draft('The rate is {c1}, and {c2}. 37% of everyone joined and the week of 5/13 is highest.', [
    { id: 'c1', op: 'rate', refs: [ref('2024-05-13', 'A', 'numerator'), ref('2024-05-13', 'A', 'denominator')], display: '51.0%' },
    { id: 'c2', op: 'value', refs: [ref('2024-05-20', 'A', 'numerator')], display: '3' },
  ]), spec, columns, rows);
  assert.equal(bad.ok, false);
  const msg = (bad as { problems: string[] }).problems.join('\n');
  assert.match(msg, /c1: display 51.0% does not match the computed value 50.0%/);
  assert.match(msg, /c2: row key x=2024-05-20, series=A matches 0 rows/);
  assert.match(msg, /numbers outside claims.*37.*5\/13/);
  const missing = checkSummary(draft('It is {c9}.', []), spec, columns, rows);
  assert.match((missing as { problems: string[] }).problems[0], /no claim for \{c9\}/);
});

test('result labels and definition numbers can be written as they are', () => {
  const ok = checkSummary(draft('Counted 7 days after signup, from the week of 2024-05-06 to the week of 2024-05-13.', []), spec, columns, rows);
  assert.equal(ok.ok, true);
  assert.deepEqual(strayNumbers('W4 retention', ['W4']), []);
  assert.deepEqual(strayNumbers('W14 retention', ['W4']), ['14']);
});

test('numbers with rate or ratio units and full-width digits are rejected outside claims', () => {
  const bad = (prose: string, s = spec) => {
    const r = checkSummary(draft(prose, []), s, columns, rows);
    return r.ok ? null : r.problems.join('\n');
  };
  assert.equal(bad('They joined within 7 days.'), null);
  assert.match(bad('The rate is 7%.')!, /numbers outside claims/);
  assert.match(bad('The rate is 7 %.')!, /numbers outside claims/);
  assert.match(bad('The rate is ９９％.')!, /99/);
  assert.match(bad('It is 2x higher than B.')!, /numbers outside claims/);
  assert.match(bad('A gap of 3pp.')!, /numbers outside claims/);
  // Korean units attach to the number, so the unit must match the definition too
  inKorean(() => {
    assert.equal(bad('가입 후 7일 안에 참여했어요.', koSpec), null);
    assert.match(bad('B보다 2배 높습니다.', koSpec)!, /numbers outside claims/);
    assert.match(bad('7주 동안 보였어요.', koSpec)!, /numbers outside claims/);
  });
});

test('a difference may be written without a sign, and integers are compared as they are', () => {
  const d = draft('B is {c1} lower than A.', [
    { id: 'c1', op: 'diff', refs: [ref('2024-05-13', 'B', 'numerator'), ref('2024-05-13', 'B', 'denominator'), ref('2024-05-13', 'A', 'numerator'), ref('2024-05-13', 'A', 'denominator')], display: '25.0pp' },
  ]);
  assert.deepEqual(checkSummary(d, spec, columns, rows), { ok: true, text: 'B is 25.0pp lower than A.' });
  const n = draft('{c1}', [{ id: 'c1', op: 'value', refs: [ref('2024-05-06', 'A', 'denominator')], display: '1,000' }]);
  assert.equal(checkSummary(n, spec, columns, rows).ok, false);
});

test('reply format checks', () => {
  assert.throws(() => parseSummary({ prose: '', claims: [] }), SummaryError);
  assert.throws(() => parseSummary({ prose: 'x', claims: [{ id: 'c1', op: 'rate', refs: [ref('a', 'b', 'c')], display: '1%' }] }), /wrong count for rate/);
  assert.throws(() => parseSummary({ prose: 'x', claims: [{ id: 'c1', op: 'value', refs: [ref('a', 'b', 'c')], display: '1' }, { id: 'c1', op: 'value', refs: [ref('a', 'b', 'c')], display: '1' }] }), /duplicate/);
});

test('over 200 rows only a deterministic summary is sent', () => {
  const many = Array.from({ length: 250 }, (_, i) => [`k${i}`, i]);
  const s = summaryResult([col('k'), col('n')], many);
  assert.equal(s.row_count, 250);
  assert.equal((s.first_rows as unknown[]).length, 20);
  assert.deepEqual((s.stats as unknown[])[1], { name: 'n', min: 0, max: 249, sum: 31125 });
  assert.equal(summaryResult([col('k')], [['a']]).rows !== undefined, true);
});

test('recompute decision: automatic when only the snapshot changes, review when rules change', () => {
  const v: PanelVersions = { snapshot_id: 's1', schema_version: 'a', policy_version: 'b', docs_version: 'c', prompt_version: 'd', pattern_contract_version: 1, renderer_version: 1 };
  const p = { versions: v, status: 'ok', last_result: { snapshot_id: 's1' } } as unknown as SavedPanel;
  assert.equal(decide(p, v), 'none');
  assert.equal(decide(p, { ...v, snapshot_id: 's2' }), 'auto');
  assert.equal(decide(p, { ...v, snapshot_id: 's2', renderer_version: 2 }), 'auto');
  assert.equal(decide(p, { ...v, renderer_version: 2 }), 'none');
  for (const k of ['schema_version', 'policy_version', 'docs_version', 'prompt_version'] as const) assert.equal(decide(p, { ...v, snapshot_id: 's2', [k]: 'z' }), 'review', k);
  assert.equal(decide(p, { ...v, pattern_contract_version: 2 }), 'review');
  assert.equal(decide({ ...p, status: 'review' } as SavedPanel, { ...v, docs_version: 'z' }), 'none');
  assert.equal(decide({ ...p, status: 'recompute_failed', last_result: { snapshot_id: 's0' } } as unknown as SavedPanel, v), 'auto');
});
