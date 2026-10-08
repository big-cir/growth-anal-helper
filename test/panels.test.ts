// 패널 사양·계약·불변식·대표 숫자·해시·이중 실행.
import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import { parsePanelSpec, PanelSpecError, COMPARISON_CAVEAT, type PanelSpec } from '../src/panels/spec.ts';
import { checkContract, checkInvariants, computeHeadline, ContractError, effectiveCaveats, type Row } from '../src/panels/contract.ts';
import { normalizeSql, semanticHash, resultCacheKey } from '../src/panels/hash.ts';
import { runPanel, sameResult, type PanelRunResult } from '../src/panels/run.ts';
import { ExecutionSlots } from '../src/query/executor.ts';
import type { ResultColumn } from '../src/query/worker.ts';
import { buildDemoSnapshot, demoSeedPanels, DEMO_DIR, type DemoSnapshot } from './helpers/demo-snapshot.ts';
import { loadMetrics, type MetricDict } from '../src/panels/metrics.ts';
import { OFFDICT_CAVEAT } from '../src/panels/spec.ts';
import { pseudonymize } from '../src/snapshot/pseudonymize.ts';
import { DatabaseSync } from 'node:sqlite';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const spec = (display: Record<string, unknown>, over: Record<string, unknown> = {}): PanelSpec =>
  parsePanelSpec({ title: 't', question: 'q', sql: 'SELECT 1', display, definition: [['모집단', 'x']], caveats: [], answers: [], ...over });
const cols = (...names: string[]): ResultColumn[] => names.map((name) => ({ name, table: null, column: null }));


test('사양: 데모 시드 패널 4개는 모두 통과', () => {
  const ps = demoSeedPanels();
  assert.deepEqual(ps.map((p) => p.spec.display.type).sort(), ['cohort', 'funnel', 'line', 'number']);
});

test('사양: 역할 칸 기본값은 역할 이름, 생략한 선택 칸은 null', () => {
  const s = spec({ type: 'bar', x: 'week' });
  assert.deepEqual(s.display.columns, { x: 'week', numerator: 'numerator', denominator: 'denominator', series: null });
  assert.deepEqual(spec({ type: 'number' }).display.columns, { numerator: 'numerator', denominator: 'denominator', value: null, label: null });
  assert.deepEqual(spec({ type: 'number', value: 'v' }).display.columns, { numerator: null, denominator: null, value: 'v', label: null });
});

test('사양: 위반은 거부', () => {
  const bad: [Record<string, unknown>, Record<string, unknown>?][] = [
    [{ type: 'pie' }],
    [{ type: 'line', step_no: 'x' }],
    [{ type: 'line', x: 'bad name' }],
    [{ type: 'table', headline: { x: 'a' } }],
    [{ type: 'cohort', headline: { series: 'a' } }],
    [{ type: 'line', key: ['x'] }],
    [{ type: 'number', value: 'v', numerator: 'n' }],
    [{ type: 'line', extra: ['a', 'b', 'c', 'd', 'e', 'f', 'g'] }],
    [{ type: 'line' }, { title: 'x'.repeat(61) }],
    [{ type: 'line' }, { definition: [] }],
    [{ type: 'line' }, { definition: [['a']] }],
    [{ type: 'line' }, { answers: [{ question: 'a', answer: 'b' }] }],
    [{ type: 'line' }, { caveats: ['a', 'b', 'c', 'd', 'e', 'f'] }],
    [{ type: 'line' }, { sql: '' }],
  ];
  for (const [d, o] of bad) assert.throws(() => spec(d, o), PanelSpecError, JSON.stringify([d, o]));
});


test('계약: 필요한 칸이 결과에 없으면 거부(어떤 칸이 없는지와 결과 칸 목록)', () => {
  assert.throws(() => checkContract(spec({ type: 'line' }), cols('x', 'numerator')), /필요한 칸이 결과에 없음: denominator .*결과 칸: x, numerator/);
  assert.throws(() => checkContract(spec({ type: 'line', extra: ['unknown'] }), cols('x', 'numerator', 'denominator')), /unknown/);
  assert.throws(() => checkContract(spec({ type: 'line' }), cols('x', 'x', 'numerator', 'denominator')), /중복/);
  checkContract(spec({ type: 'table' }), cols('anything'));
});

const problems = (s: PanelSpec, c: ResultColumn[], rows: Row[]) => checkInvariants(s, c, rows).map((v) => `${v.row}: ${v.problem}`);

test('불변식(공통): 행 0개, 음수·비정수·NULL 수, 분자 > 분모, 행 키 중복 — 위반 행 키를 담는다', () => {
  const s = spec({ type: 'bar', series: 'g', extra: ['unknown'] });
  const c = cols('x', 'g', 'numerator', 'denominator', 'unknown');
  assert.deepEqual(problems(s, c, []), ['-: 결과 행이 없음']);
  const p = problems(s, c, [
    ['w1', 'a', 5, 10, 0],
    ['w1', 'b', 11, 10, 0],
    ['w2', 'a', -1, 10, 1.5],
    ['w2', 'a', null, 10, 0],
  ]);
  assert.deepEqual(p, [
    'x=w1, g=b: numerator(11) > denominator(10)',
    'x=w2, g=a: numerator는 0 이상 정수여야 함 (-1)',
    'x=w2, g=a: unknown는 0 이상 정수여야 함 (1.5)',
    'x=w2, g=a: numerator는 0 이상 정수여야 함 (NULL)',
    'x=w2, g=a: 행 키가 중복됨 (3번째 행과 같음)',
  ]);
});

test('불변식(line/bar): x NULL·형식 섞임, series 7개 이상', () => {
  const s = spec({ type: 'line', series: 's' });
  const c = cols('x', 's', 'numerator', 'denominator');
  assert.ok(problems(s, c, [[null, 'a', 1, 2]]).includes('-: x에 NULL이 있음'));
  assert.ok(problems(s, c, [[1, 'a', 1, 2], ['w', 'a', 1, 2]]).includes('-: x 값의 형식이 섞여 정렬할 수 없음 (수와 문자열)'));
  const seven = Array.from({ length: 7 }, (_, i) => ['w', `s${i}`, 1, 2] as Row);
  assert.ok(problems(s, c, seven).includes('-: series가 7개 (6개 이하)'));
});

test('불변식(funnel): 단계 연속, 도달 감소, 분모 = 앞 단계 도달 − 판정 불가 (코호트별)', () => {
  const s = spec({ type: 'funnel', cohort: 'cohort' });
  const c = cols('cohort', 'step_no', 'step_name', 'reached', 'eligible', 'unknown');
  const ok: Row[] = [['A', 1, '가입', 100, 100, 0], ['A', 2, '참여', 80, 95, 5], ['A', 3, '연결', 60, 80, 0], ['B', 1, '가입', 10, 10, 0]];
  assert.deepEqual(problems(s, c, ok), []);
  assert.deepEqual(problems(s, c, [['A', 1, 'a', 100, 100, 0], ['A', 3, 'c', 50, 100, 0]]), ['cohort=A, step_no=3: 단계 번호가 1부터 연속이 아님 (2이어야 함)']);
  assert.deepEqual(problems(s, c, [['A', 1, 'a', 50, 100, 0], ['A', 2, 'b', 60, 60, 0]]).sort(), [
    'cohort=A, step_no=2: 도달(60)이 앞 단계 도달(50)보다 많음',
    'cohort=A, step_no=2: 분모(60) ≠ 앞 단계 도달(50) − 판정 불가(0)',
  ]);
});

test('불변식(cohort): period는 0 이상 정수, 같은 코호트·series에서 period가 늘면 분모가 늘지 않음', () => {
  const s = spec({ type: 'cohort', series: 'g' });
  const c = cols('cohort', 'g', 'period', 'numerator', 'denominator');
  assert.deepEqual(problems(s, c, [['A', 'x', 1, 5, 50], ['A', 'x', 2, 5, 40], ['A', 'y', 1, 1, 10], ['A', 'y', 2, 1, 12]]), [
    'cohort=A, g=y, period=2: period가 늘었는데 분모가 늘었음 (10 → 12, 관측 가능 조건 위반)',
  ]);
  assert.deepEqual(problems(s, c, [['A', 'x', -1, 1, 2]]), ['cohort=A, g=x, period=-1: period는 0 이상 정수여야 함 (-1)']);
});

test('불변식(number·table): number는 1행, table은 key 지정 시에만 중복 검사', () => {
  assert.deepEqual(problems(spec({ type: 'number' }), cols('numerator', 'denominator'), [[1, 2], [1, 2]]), ['-: number 패턴은 행이 1개여야 함 (2개)']);
  assert.deepEqual(problems(spec({ type: 'number', value: 'v' }), cols('v'), [['x']]), ['1번째 행: value는 수여야 함 (x)']);
  assert.deepEqual(problems(spec({ type: 'table' }), cols('a'), [[1], [1]]), []);
  assert.deepEqual(problems(spec({ type: 'table', key: ['a'] }), cols('a'), [[1], [1]]), ['a=1: 행 키가 중복됨 (1번째 행과 같음)']);
});

test('비교 패널이면 관측 비교 문구를 앞에 붙인다(series 2개 이상, 중복이면 생략)', () => {
  const s = spec({ type: 'line', series: 's' }, { caveats: ['기타'], metric: 'm' });
  const c = cols('x', 's', 'numerator', 'denominator');
  assert.deepEqual(effectiveCaveats(s, c, [['w', 'a', 1, 2], ['w', 'b', 1, 2]]), [COMPARISON_CAVEAT, '기타']);
  assert.deepEqual(effectiveCaveats(s, c, [['w', 'a', 1, 2]]), ['기타']);
  const already = spec({ type: 'line', series: 's' }, { caveats: [`${COMPARISON_CAVEAT}.`], metric: 'm' });
  assert.equal(effectiveCaveats(already, c, [['w', 'a', 1, 2], ['w', 'b', 1, 2]]).length, 1);
  const off = spec({ type: 'line', series: 's' }, { caveats: ['기타'] });
  assert.deepEqual(effectiveCaveats(off, c, [['w', 'a', 1, 2], ['w', 'b', 1, 2]]), [OFFDICT_CAVEAT, COMPARISON_CAVEAT, '기타']);
});


test('대표 숫자: 패턴별 기본 규칙과 headline 지정, 분모 30 미만은 바꾸지 않고 lowN', () => {
  const line = spec({ type: 'line', series: 's' });
  const lc = cols('x', 's', 'numerator', 'denominator');
  const lr: Row[] = [['2024-01-08', 'b', 9, 100], ['2024-01-01', 'a', 1, 10], ['2024-01-08', 'a', 3, 12]];
  const h = computeHeadline(line, lc, lr)!;
  assert.deepEqual([h.value, h.lowN, h.label], [0.09, false, '2024-01-08 · b']);
  const ha = computeHeadline(spec({ type: 'line', series: 's', headline: { series: 'a' } }), lc, lr)!;
  assert.deepEqual([ha.value, ha.lowN, ha.label], [0.25, true, '2024-01-08 · a']);
  const h2 = computeHeadline(spec({ type: 'line', series: 's', headline: { x: '2024-01-01', series: 'a' } }), lc, lr)!;
  assert.deepEqual([h2.value, h2.denominator], [0.1, 10]);
  assert.throws(() => computeHeadline(spec({ type: 'line', series: 's', headline: { series: 'zzz' } }), lc, lr), ContractError);
  assert.throws(() => computeHeadline(spec({ type: 'line', headline: { x: 'nope' } }), cols('x', 'numerator', 'denominator'), [['a', 1, 2]]), ContractError);

  const fc = cols('cohort', 'step_no', 'step_name', 'reached', 'eligible', 'unknown');
  const fr: Row[] = [['ALL', 1, '가입', 200, 200, 0], ['ALL', 2, '참여', 50, 200, 0], ['A', 1, '가입', 10, 10, 0], ['A', 2, '참여', 5, 10, 0]];
  const fh = computeHeadline(spec({ type: 'funnel', cohort: 'cohort' }), fc, fr)!;
  assert.deepEqual([fh.value, fh.label, fh.lowN], [0.25, '참여 / 가입', false]);
  assert.equal(computeHeadline(spec({ type: 'funnel', cohort: 'cohort', headline: { x: 'A' } }), fc, fr)!.value, 0.5);

  const nh = computeHeadline(spec({ type: 'number', label: 'l' }), cols('numerator', 'denominator', 'l'), [[3, 4, '최근']])!;
  assert.deepEqual([nh.value, nh.label, nh.lowN], [0.75, '최근', true]);
  assert.equal(computeHeadline(spec({ type: 'cohort' }), cols('cohort', 'period', 'numerator', 'denominator'), [['A', 1, 1, 2]]), null);
  assert.equal(computeHeadline(spec({ type: 'table' }), cols('a'), [[1]]), null);
});


test('의미 해시: 공백·빈 줄 차이는 같은 패널, 정의·표시·답·params가 다르면 다른 패널', () => {
  const a = spec({ type: 'line' }, { sql: 'SELECT x,\n  numerator   \n\n\n\nFROM d_t  ' });
  const b = spec({ type: 'line' }, { sql: 'SELECT x,\n  numerator\n\nFROM d_t' });
  assert.equal(normalizeSql(a.sql), normalizeSql(b.sql));
  assert.equal(semanticHash(a, 'p'), semanticHash(b, 'p'));
  assert.equal(semanticHash(a, 'p'), semanticHash(spec({ type: 'line' }, { sql: a.sql, title: '다른 제목', caveats: ['다른 주의'] }), 'p'));
  assert.notEqual(semanticHash(a, 'p'), semanticHash(a, 'q'));
  assert.notEqual(semanticHash(a, 'p'), semanticHash(spec({ type: 'bar' }, { sql: a.sql }), 'p'));
  assert.notEqual(semanticHash(a, 'p'), semanticHash(spec({ type: 'line' }, { sql: a.sql, definition: [['모집단', 'y']] }), 'p'));
  assert.notEqual(semanticHash(a, 'p'), semanticHash(spec({ type: 'line' }, { sql: a.sql, answers: [{ question: 'a', answer: 'b', defaulted: true }] }), 'p'));
  const ctx = { snapshot_id: 's', schema_version: 'a', policy_version: 'b', docs_version: 'c', prompt_version: 'd' };
  assert.notEqual(resultCacheKey('h', ctx), resultCacheKey('h', { ...ctx, snapshot_id: 't' }));
  assert.notEqual(resultCacheKey('h', ctx), resultCacheKey('h', { ...ctx, docs_version: 'x' }));
});

test('결과 비교: 정렬 순서는 무시, 타입이 다르면 다름(1 ≠ "1"), 실수는 정확히', () => {
  assert.ok(sameResult([[['i', 1], ['s', 'a']], [['i', 2], ['n', null]]], [[['i', 2], ['n', null]], [['i', 1], ['s', 'a']]]));
  assert.ok(!sameResult([[['i', 1]]], [[['s', '1']]]));
  assert.ok(!sameResult([[['f', 0.1]]], [[['f', 0.1000000001]]]));
  assert.ok(!sameResult([[['i', 1]]], [[['i', 1]], [['i', 1]]]));
});


let demo: DemoSnapshot;
const slots = new ExecutionSlots(4, 4);
before(async () => {
  demo = await buildDemoSnapshot();
  metrics = loadMetrics(DEMO_DIR, ['d_'], demoSeedPanels().map((p) => ({ id: p.id, metric: p.spec.metric }))).dict;
});

function run(s: PanelSpec): Promise<PanelRunResult> {
  return slots.run('interactive', undefined, (lease) => runPanel({
    spec: s, paths: { real: demo.real, agent: demo.agent }, asOf: demo.asOf, params: demo.ws.config.params,
    policy, metrics, blocked: { columns: [], tables: [] }, heapLimitMb: 512, roles: demo.roles, lease,
  }));
}
/** ID 의존 검사를 원본 표로도 보려고 r_까지 연다 */
let policy = { panelReadablePrefixes: ['r_', 'd_'] };
let metrics: MetricDict;

test('이중 실행: 데모 시드 패널 4개 모두 통과, 원본과 가명 결과가 같다', async () => {
  for (const { id, spec: s } of demoSeedPanels()) {
    const r = await run(s);
    assert.ok(r.ok, `${id}: ${!r.ok ? `${r.stage} ${r.message}` : ''}`);
    if (r.ok) assert.deepEqual(r.real, r.agent);
  }
});

test('이중 실행: ID 칸을 그대로 내면 거부(별칭이어도)', async () => {
  const r = await run(spec({ type: 'table' }, { sql: 'SELECT member_id AS who, count(*) AS n FROM d_activity GROUP BY member_id' }));
  assert.deepEqual([r.ok, !r.ok && r.stage], [false, 'id_column']);
  assert.match(!r.ok ? r.message : '', /who/);
});

test('이중 실행: ID 값에 의존하는 패널(MIN(id), CAST(id AS TEXT), id + 0, ORDER BY id LIMIT 5)은 id_dependent', async () => {
  const cases = [
    'SELECT MIN(id) AS v FROM r_member',
    'SELECT CAST(id AS TEXT) AS v FROM r_member ORDER BY created_at LIMIT 3',
    'SELECT id + 0 AS v FROM r_member ORDER BY created_at LIMIT 3',
    'SELECT created_at AS v FROM r_member ORDER BY id LIMIT 5',
    "SELECT count(*) AS v FROM r_post WHERE member_id < 1500",
  ];
  for (const sql of cases) {
    const r = await run(spec({ type: 'table' }, { sql }));
    assert.deepEqual([r.ok, !r.ok && r.stage], [false, 'id_dependent'], sql);
  }
});

test('이중 실행: UNION·서브쿼리·CTE로 감싼 ID 칸도 원본 표시가 남아 id_column, 계산식으로 감싸면 id_dependent', async () => {
  for (const sql of [
    'SELECT id AS v FROM r_member UNION SELECT 0',
    'SELECT x.id AS v FROM (SELECT id FROM r_member) x',
    'WITH c AS (SELECT member_id FROM d_activity) SELECT member_id AS v FROM c GROUP BY 1',
  ]) {
    const r = await run(spec({ type: 'table' }, { sql }));
    assert.deepEqual([r.ok, !r.ok && r.stage], [false, 'id_column'], sql);
  }
  for (const sql of ['SELECT coalesce(id, 0) AS v FROM r_member', 'SELECT (SELECT max(id) FROM r_member) AS v']) {
    const r = await run(spec({ type: 'table' }, { sql }));
    assert.deepEqual([r.ok, !r.ok && r.stage], [false, 'id_dependent'], sql);
  }
});

test('이중 실행: 실제 ID가 1..N처럼 조밀해도 ID 전체를 계산식으로 내면 id_dependent (가명 값이 1..N이 아니므로)', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-dense-'));
  const real = join(dir, 'r.sqlite');
  const db = new DatabaseSync(real);
  db.exec('CREATE TABLE r_x (id INTEGER PRIMARY KEY, g TEXT)');
  const ins = db.prepare('INSERT INTO r_x VALUES (?, ?)');
  for (let i = 1; i <= 200; i++) ins.run(i, i % 2 ? 'a' : 'b');
  db.close();
  const agent = join(dir, 'a.sqlite');
  const roles = new Map<string, import('../src/collect/spec.ts').Role>([['r_x.id', { identifier: 'x' }], ['r_x.g', 'ordinary']]);
  pseudonymize({ srcPath: real, agentPath: agent, mapPath: join(dir, 'm.sqlite'), roles });
  const r = await slots.run('interactive', undefined, (lease) => runPanel({
    spec: spec({ type: 'table' }, { sql: 'SELECT coalesce(id, 0) AS v FROM r_x' }), paths: { real, agent }, asOf: '2024-01-01 00:00:00.000000',
    params: {}, policy: { panelReadablePrefixes: ['r_'] }, metrics: { dimension_tables: [], metrics: [] }, blocked: { columns: [], tables: [] }, heapLimitMb: 256, roles, lease,
  }));
  assert.deepEqual([r.ok, !r.ok && r.stage], [false, 'id_dependent']);
});

test('데모 시드 패널의 대표 숫자는 고정 시드에서 정해진 값', async () => {
  const p = demoSeedPanels().find((x) => x.id === 'recent_first_post')!;
  const r = await run(p.spec);
  assert.ok(r.ok);
  if (!r.ok) return;
  const h = computeHeadline(p.spec, r.columns, r.real)!;
  assert.deepEqual([h.numerator, h.denominator, h.lowN, h.label], [197, 601, false, '최근 4주 가입자']);
});

test('이중 실행: 단계별 실패 — 정적 검사, 실행 오류, 계약, 불변식(가명 키)', async () => {
  const lint = await run(spec({ type: 'table' }, { sql: 'SELECT 1; SELECT 2' }));
  assert.deepEqual([lint.ok, !lint.ok && lint.stage], [false, 'lint']);
  const exec = await run(spec({ type: 'table' }, { sql: 'SELECT nope FROM d_member' }));
  assert.deepEqual([exec.ok, !exec.ok && exec.stage], [false, 'exec']);
  const contract = await run(spec({ type: 'line' }, { sql: 'SELECT signup_week AS x, count(*) AS n FROM d_member GROUP BY 1' }));
  assert.deepEqual([contract.ok, !contract.ok && contract.stage], [false, 'contract']);
  const inv = await run(spec({ type: 'line' }, { sql: 'SELECT signup_week AS x, count(*) AS numerator, 1 AS denominator FROM d_member GROUP BY 1' }));
  assert.deepEqual([inv.ok, !inv.ok && inv.stage], [false, 'invariant']);
  assert.match(!inv.ok ? inv.message : '', /x=\d{4}-\d{2}-\d{2}: numerator\(\d+\) > denominator\(1\)/);
});

test('패널용 표·지표 사전: 기본 정책은 d_만, 참조한 표 기록, 지표 표 규칙', async () => {
  const saved = { policy, metrics };
  try {
    policy = { panelReadablePrefixes: ['d_'] };
    metrics = loadMetrics(DEMO_DIR, ['d_'], demoSeedPanels().map((p) => ({ id: p.id, metric: p.spec.metric }))).dict;
    const raw = await run(spec({ type: 'table' }, { sql: 'SELECT count(*) AS n FROM r_post' }));
    assert.deepEqual([raw.ok, !raw.ok && raw.stage], [false, 'exec']);
    assert.match(!raw.ok ? raw.message : '', /읽을 수 없는 표: r_post.*패널용 표 d_\*/);

    for (const { id, spec: s } of demoSeedPanels()) {
      const r = await run(s);
      assert.ok(r.ok, `${id}: ${!r.ok ? `${r.stage} ${r.message}` : ''}`);
    }
    const seed = demoSeedPanels().find((p) => p.id === 'connected_retention')!;
    const ok = await run(seed.spec);
    assert.deepEqual(ok.ok && ok.tables, ['d_member_activity_week', 'd_member_first_week']);

    const sql = 'SELECT count(*) AS n FROM d_activity';
    const wrong = await run(spec({ type: 'table' }, { sql, metric: 'first_week_activation' }));
    assert.deepEqual([wrong.ok, !wrong.ok && wrong.stage], [false, 'metric_tables']);
    assert.match(!wrong.ok ? wrong.message : '', /벗어난 표: d_activity/);
    const dimOnly = await run(spec({ type: 'table' }, { sql: 'SELECT count(*) AS n FROM d_calendar_week', metric: 'first_week_activation' }));
    assert.match(!dimOnly.ok ? dimOnly.message : '', /하나 이상을 읽어야 함/);
    const unknown = await run(spec({ type: 'table' }, { sql, metric: 'nope' }));
    assert.match(!unknown.ok ? unknown.message : '', /지표 사전에 없는 metric: nope/);
    const off = await run(spec({ type: 'table' }, { sql }));
    assert.ok(off.ok);
    assert.deepEqual(off.ok && off.tables, ['d_activity']);
    const cte = await run(spec({ type: 'table' }, { sql: 'WITH unused AS (SELECT * FROM d_member), c AS (SELECT member_id FROM d_activity) SELECT count(*) AS n FROM c WHERE EXISTS (SELECT 1 FROM d_membership m WHERE m.member_id = c.member_id)' }));
    assert.deepEqual(cte.ok && cte.tables, ['d_activity', 'd_membership']);
  } finally {
    policy = saved.policy;
    metrics = saved.metrics;
  }
});
