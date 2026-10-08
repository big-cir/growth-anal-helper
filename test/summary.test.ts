// 긴 설명 검사와 재계산 판정.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parsePanelSpec } from '../src/panels/spec.ts';
import { checkSummary, parseSummary, strayNumbers, summaryResult, SummaryError, type SummaryDraft } from '../src/panels/summary-check.ts';
import { decide } from '../src/panels/recompute.ts';
import type { PanelVersions, SavedPanel } from '../src/panels/store.ts';
import type { ResultColumn } from '../src/query/worker.ts';

const col = (name: string): ResultColumn => ({ name, table: null, column: null });
const spec = parsePanelSpec({
  title: '가입 주별 7일 참여율', question: '최근 8주 가입자의 7일 내 참여율은?', sql: 'SELECT 1',
  display: { type: 'line', x: 'x', numerator: 'numerator', denominator: 'denominator', series: 'series' },
  definition: [['기간', '가입 후 7일']], caveats: [], answers: [{ question: '창', answer: '7일', defaulted: true }],
});
const columns = ['x', 'series', 'numerator', 'denominator'].map(col);
const rows = [
  ['2024-05-06', 'A', 30, 100],
  ['2024-05-13', 'A', 45, 90],
  ['2024-05-13', 'B', 20, 80],
];
const ref = (x: string, s: string, c: string) => ({ row: [{ column: 'x', value: x }, { column: 'series', value: s }], column: c });
const draft = (prose: string, claims: SummaryDraft['claims']): SummaryDraft => ({ prose, claims });

test('claim을 결과에서 다시 계산해 맞으면 문장에 넣는다', () => {
  const d = draft('2024-05-13 주 A의 참여율은 {c1}로, B보다 {c2} 높고 {c3}입니다. 두 주 합계 가입자는 {c4}명입니다.', [
    { id: 'c1', op: 'rate', refs: [ref('2024-05-13', 'A', 'numerator'), ref('2024-05-13', 'A', 'denominator')], display: '50.0%' },
    { id: 'c2', op: 'diff', refs: [ref('2024-05-13', 'A', 'numerator'), ref('2024-05-13', 'A', 'denominator'), ref('2024-05-13', 'B', 'numerator'), ref('2024-05-13', 'B', 'denominator')], display: '25%p' },
    { id: 'c3', op: 'ratio', refs: [ref('2024-05-13', 'A', 'numerator'), ref('2024-05-13', 'A', 'denominator'), ref('2024-05-13', 'B', 'numerator'), ref('2024-05-13', 'B', 'denominator')], display: '2.0배' },
    { id: 'c4', op: 'sum', refs: [ref('2024-05-06', 'A', 'denominator'), ref('2024-05-13', 'A', 'denominator')], display: '190' },
  ]);
  const r = checkSummary(d, spec, columns, rows);
  assert.deepEqual(r, { ok: true, text: '2024-05-13 주 A의 참여율은 50.0%로, B보다 25.0%p 높고 2.0배입니다. 두 주 합계 가입자는 190명입니다.' });
});

test('계산이 안 맞거나 행·칸이 없거나 claim 밖 숫자가 있으면 지적한다', () => {
  const bad = checkSummary(draft('참여율은 {c1}, 그리고 {c2}. 전체의 37%가 참여했고 5월 13일 주가 가장 높습니다.', [
    { id: 'c1', op: 'rate', refs: [ref('2024-05-13', 'A', 'numerator'), ref('2024-05-13', 'A', 'denominator')], display: '51.0%' },
    { id: 'c2', op: 'value', refs: [ref('2024-05-20', 'A', 'numerator')], display: '3' },
  ]), spec, columns, rows);
  assert.equal(bad.ok, false);
  const msg = (bad as { problems: string[] }).problems.join('\n');
  assert.match(msg, /c1: display 51.0%가 계산값 50.0%와 맞지 않음/);
  assert.match(msg, /c2: 행 키 x=2024-05-20, series=A에 맞는 행이 0개/);
  assert.match(msg, /claim 밖의 숫자.*37.*5.*13/);
  const missing = checkSummary(draft('{c9}입니다.', []), spec, columns, rows);
  assert.match((missing as { problems: string[] }).problems[0], /\{c9\}에 해당하는 claim이 없음/);
});

test('결과 라벨·정의 숫자는 문장에 그대로 쓸 수 있다', () => {
  const ok = checkSummary(draft('가입 후 7일 기준으로 2024-05-06 주부터 2024-05-13 주까지 셌습니다.', []), spec, columns, rows);
  assert.equal(ok.ok, true);
  assert.deepEqual(strayNumbers('W4 리텐션', ['W4']), []);
  assert.deepEqual(strayNumbers('W14 리텐션', ['W4']), ['14']);
});

test('정의 숫자는 단위까지 맞아야 하고, 비율·배수 단위가 붙은 숫자와 전각 숫자는 claim 밖이면 거부', () => {
  const bad = (prose: string) => {
    const r = checkSummary(draft(prose, []), spec, columns, rows);
    return r.ok ? null : r.problems.join('\n');
  };
  assert.equal(bad('가입 후 7일 안에 참여했어요.'), null);
  assert.match(bad('참여율은 7%입니다.')!, /claim 밖의 숫자/);
  assert.match(bad('참여율은 7 %입니다.')!, /claim 밖의 숫자/);
  assert.match(bad('참여율은 ９９％입니다.')!, /99/);
  assert.match(bad('B보다 2배 높습니다.')!, /claim 밖의 숫자/);
  assert.match(bad('7주 동안 보였어요.')!, /claim 밖의 숫자/);
});

test('차이는 부호 없이 써도 되고, 정수는 그대로 비교한다', () => {
  const d = draft('B는 A보다 {c1} 낮습니다.', [
    { id: 'c1', op: 'diff', refs: [ref('2024-05-13', 'B', 'numerator'), ref('2024-05-13', 'B', 'denominator'), ref('2024-05-13', 'A', 'numerator'), ref('2024-05-13', 'A', 'denominator')], display: '25.0%p' },
  ]);
  assert.deepEqual(checkSummary(d, spec, columns, rows), { ok: true, text: 'B는 A보다 25.0%p 낮습니다.' });
  const n = draft('{c1}', [{ id: 'c1', op: 'value', refs: [ref('2024-05-06', 'A', 'denominator')], display: '1,000' }]);
  assert.equal(checkSummary(n, spec, columns, rows).ok, false);
});

test('응답 형식 검사', () => {
  assert.throws(() => parseSummary({ prose: '', claims: [] }), SummaryError);
  assert.throws(() => parseSummary({ prose: 'x', claims: [{ id: 'c1', op: 'rate', refs: [ref('a', 'b', 'c')], display: '1%' }] }), /rate에 맞는 개수/);
  assert.throws(() => parseSummary({ prose: 'x', claims: [{ id: 'c1', op: 'value', refs: [ref('a', 'b', 'c')], display: '1' }, { id: 'c1', op: 'value', refs: [ref('a', 'b', 'c')], display: '1' }] }), /중복/);
});

test('200행이 넘으면 결정적 요약만 보낸다', () => {
  const many = Array.from({ length: 250 }, (_, i) => [`k${i}`, i]);
  const s = summaryResult([col('k'), col('n')], many);
  assert.equal(s.row_count, 250);
  assert.equal((s.first_rows as unknown[]).length, 20);
  assert.deepEqual((s.stats as unknown[])[1], { name: 'n', min: 0, max: 249, sum: 31125 });
  assert.equal(summaryResult([col('k')], [['a']]).rows !== undefined, true);
});

test('재계산 판정: 스냅샷만 바뀌면 자동, 규칙이 바뀌면 재검토', () => {
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
