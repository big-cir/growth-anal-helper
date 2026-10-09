// Panel spec, contract, invariants, headline, hash, dual run.
import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import { parsePanelSpec, PanelSpecError, comparisonCaveat, type PanelSpec } from '../src/panels/spec.ts';
import { checkContract, checkInvariants, computeHeadline, ContractError, effectiveCaveats, type Row } from '../src/panels/contract.ts';
import { normalizeSql, semanticHash, resultCacheKey } from '../src/panels/hash.ts';
import { runPanel, sameResult, type PanelRunResult } from '../src/panels/run.ts';
import { ExecutionSlots } from '../src/query/executor.ts';
import type { ResultColumn } from '../src/query/worker.ts';
import { buildDemoSnapshot, demoSeedPanels, DEMO_DIR, type DemoSnapshot } from './helpers/demo-snapshot.ts';
import { loadMetrics, type MetricDict } from '../src/panels/metrics.ts';
import { offdictCaveat } from '../src/panels/spec.ts';
import { pseudonymize } from '../src/snapshot/pseudonymize.ts';
import { DatabaseSync } from 'node:sqlite';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const spec = (display: Record<string, unknown>, over: Record<string, unknown> = {}): PanelSpec =>
  parsePanelSpec({ title: 't', question: 'q', sql: 'SELECT 1', display, definition: [['Population', 'x']], caveats: [], answers: [], ...over });
const cols = (...names: string[]): ResultColumn[] => names.map((name) => ({ name, table: null, column: null }));


test('spec: all 4 demo seed panels pass', () => {
  const ps = demoSeedPanels();
  assert.deepEqual(ps.map((p) => p.spec.display.type).sort(), ['cohort', 'funnel', 'line', 'number']);
});

test('spec: role columns default to the role name, omitted optional ones are null', () => {
  const s = spec({ type: 'bar', x: 'week' });
  assert.deepEqual(s.display.columns, { x: 'week', numerator: 'numerator', denominator: 'denominator', series: null });
  assert.deepEqual(spec({ type: 'number' }).display.columns, { numerator: 'numerator', denominator: 'denominator', value: null, label: null });
  assert.deepEqual(spec({ type: 'number', value: 'v' }).display.columns, { numerator: null, denominator: null, value: 'v', label: null });
});

test('spec: violations are rejected', () => {
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


test('contract: missing required columns are rejected (lists the missing and result columns)', () => {
  assert.throws(() => checkContract(spec({ type: 'line' }), cols('x', 'numerator')), /missing columns the line pattern needs: denominator .*result columns: x, numerator/);
  assert.throws(() => checkContract(spec({ type: 'line', extra: ['unknown'] }), cols('x', 'numerator', 'denominator')), /unknown/);
  assert.throws(() => checkContract(spec({ type: 'line' }), cols('x', 'x', 'numerator', 'denominator')), /duplicate/);
  checkContract(spec({ type: 'table' }), cols('anything'));
});

const problems = (s: PanelSpec, c: ResultColumn[], rows: Row[]) => checkInvariants(s, c, rows).map((v) => `${v.row}: ${v.problem}`);

test('invariants (common): no rows, negative/non-integer/NULL counts, numerator > denominator, duplicate keys — row keys are reported', () => {
  const s = spec({ type: 'bar', series: 'g', extra: ['unknown'] });
  const c = cols('x', 'g', 'numerator', 'denominator', 'unknown');
  assert.deepEqual(problems(s, c, []), ['-: result has no rows']);
  const p = problems(s, c, [
    ['w1', 'a', 5, 10, 0],
    ['w1', 'b', 11, 10, 0],
    ['w2', 'a', -1, 10, 1.5],
    ['w2', 'a', null, 10, 0],
  ]);
  assert.deepEqual(p, [
    'x=w1, g=b: numerator(11) > denominator(10)',
    'x=w2, g=a: numerator must be an integer ≥ 0 (-1)',
    'x=w2, g=a: unknown must be an integer ≥ 0 (1.5)',
    'x=w2, g=a: numerator must be an integer ≥ 0 (NULL)',
    'x=w2, g=a: duplicate row key (same as row 3)',
  ]);
});

test('invariants (line/bar): NULL or mixed x, 7+ series', () => {
  const s = spec({ type: 'line', series: 's' });
  const c = cols('x', 's', 'numerator', 'denominator');
  assert.ok(problems(s, c, [[null, 'a', 1, 2]]).includes('-: x has NULL'));
  assert.ok(problems(s, c, [[1, 'a', 1, 2], ['w', 'a', 1, 2]]).includes('-: x mixes numbers and strings and cannot be sorted'));
  const seven = Array.from({ length: 7 }, (_, i) => ['w', `s${i}`, 1, 2] as Row);
  assert.ok(problems(s, c, seven).includes('-: 7 series (6 or fewer)'));
});

test('invariants (funnel): consecutive steps, non-increasing reached, eligible = previous reached − unknown (per cohort)', () => {
  const s = spec({ type: 'funnel', cohort: 'cohort' });
  const c = cols('cohort', 'step_no', 'step_name', 'reached', 'eligible', 'unknown');
  const ok: Row[] = [['A', 1, 'Sign-up', 100, 100, 0], ['A', 2, 'Join', 80, 95, 5], ['A', 3, 'Connect', 60, 80, 0], ['B', 1, 'Sign-up', 10, 10, 0]];
  assert.deepEqual(problems(s, c, ok), []);
  assert.deepEqual(problems(s, c, [['A', 1, 'a', 100, 100, 0], ['A', 3, 'c', 50, 100, 0]]), ['cohort=A, step_no=3: step numbers are not consecutive from 1 (expected 2)']);
  assert.deepEqual(problems(s, c, [['A', 1, 'a', 50, 100, 0], ['A', 2, 'b', 60, 60, 0]]).sort(), [
    "cohort=A, step_no=2: eligible (60) ≠ previous step's reached (50) − unknown (0)",
    "cohort=A, step_no=2: reached (60) is more than the previous step's reached (50)",
  ]);
});

test('invariants (cohort): period is an integer ≥ 0, denominator does not grow with period in a cohort/series', () => {
  const s = spec({ type: 'cohort', series: 'g' });
  const c = cols('cohort', 'g', 'period', 'numerator', 'denominator');
  assert.deepEqual(problems(s, c, [['A', 'x', 1, 5, 50], ['A', 'x', 2, 5, 40], ['A', 'y', 1, 1, 10], ['A', 'y', 2, 1, 12]]), [
    'cohort=A, g=y, period=2: denominator grew as period grew (10 → 12, breaks the observability condition)',
  ]);
  assert.deepEqual(problems(s, c, [['A', 'x', -1, 1, 2]]), ['cohort=A, g=x, period=-1: period must be an integer ≥ 0 (-1)']);
});

test('invariants (number/table): number has 1 row, table checks duplicates only with a key', () => {
  assert.deepEqual(problems(spec({ type: 'number' }), cols('numerator', 'denominator'), [[1, 2], [1, 2]]), ['-: the number pattern needs exactly 1 row (got 2)']);
  assert.deepEqual(problems(spec({ type: 'number', value: 'v' }), cols('v'), [['x']]), ['row 1: value must be a number (x)']);
  assert.deepEqual(problems(spec({ type: 'table' }), cols('a'), [[1], [1]]), []);
  assert.deepEqual(problems(spec({ type: 'table', key: ['a'] }), cols('a'), [[1], [1]]), ['a=1: duplicate row key (same as row 1)']);
});

test('comparison panels get the observational caveat first (2+ series, not repeated)', () => {
  const s = spec({ type: 'line', series: 's' }, { caveats: ['Other'], metric: 'm' });
  const c = cols('x', 's', 'numerator', 'denominator');
  assert.deepEqual(effectiveCaveats(s, c, [['w', 'a', 1, 2], ['w', 'b', 1, 2]]), [comparisonCaveat(), 'Other']);
  assert.deepEqual(effectiveCaveats(s, c, [['w', 'a', 1, 2]]), ['Other']);
  const already = spec({ type: 'line', series: 's' }, { caveats: [`${comparisonCaveat()}.`], metric: 'm' });
  assert.equal(effectiveCaveats(already, c, [['w', 'a', 1, 2], ['w', 'b', 1, 2]]).length, 1);
  const off = spec({ type: 'line', series: 's' }, { caveats: ['Other'] });
  assert.deepEqual(effectiveCaveats(off, c, [['w', 'a', 1, 2], ['w', 'b', 1, 2]]), [offdictCaveat(), comparisonCaveat(), 'Other']);
});


test('headline: default rule per pattern and headline override; denominator under 30 stays, with lowN', () => {
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
  const fr: Row[] = [['ALL', 1, 'Sign-up', 200, 200, 0], ['ALL', 2, 'Join', 50, 200, 0], ['A', 1, 'Sign-up', 10, 10, 0], ['A', 2, 'Join', 5, 10, 0]];
  const fh = computeHeadline(spec({ type: 'funnel', cohort: 'cohort' }), fc, fr)!;
  assert.deepEqual([fh.value, fh.label, fh.lowN], [0.25, 'Join / Sign-up', false]);
  assert.equal(computeHeadline(spec({ type: 'funnel', cohort: 'cohort', headline: { x: 'A' } }), fc, fr)!.value, 0.5);

  const nh = computeHeadline(spec({ type: 'number', label: 'l' }), cols('numerator', 'denominator', 'l'), [[3, 4, 'Recent']])!;
  assert.deepEqual([nh.value, nh.label, nh.lowN], [0.75, 'Recent', true]);
  assert.equal(computeHeadline(spec({ type: 'cohort' }), cols('cohort', 'period', 'numerator', 'denominator'), [['A', 1, 1, 2]]), null);
  assert.equal(computeHeadline(spec({ type: 'table' }), cols('a'), [[1]]), null);
});


test('semantic hash: whitespace differences give the same panel; definition, display, answers or params differ', () => {
  const a = spec({ type: 'line' }, { sql: 'SELECT x,\n  numerator   \n\n\n\nFROM d_t  ' });
  const b = spec({ type: 'line' }, { sql: 'SELECT x,\n  numerator\n\nFROM d_t' });
  assert.equal(normalizeSql(a.sql), normalizeSql(b.sql));
  assert.equal(semanticHash(a, 'p'), semanticHash(b, 'p'));
  assert.equal(semanticHash(a, 'p'), semanticHash(spec({ type: 'line' }, { sql: a.sql, title: 'Other title', caveats: ['Other caveat'] }), 'p'));
  assert.notEqual(semanticHash(a, 'p'), semanticHash(a, 'q'));
  assert.notEqual(semanticHash(a, 'p'), semanticHash(spec({ type: 'bar' }, { sql: a.sql }), 'p'));
  assert.notEqual(semanticHash(a, 'p'), semanticHash(spec({ type: 'line' }, { sql: a.sql, definition: [['Population', 'y']] }), 'p'));
  assert.notEqual(semanticHash(a, 'p'), semanticHash(spec({ type: 'line' }, { sql: a.sql, answers: [{ question: 'a', answer: 'b', defaulted: true }] }), 'p'));
  const ctx = { snapshot_id: 's', schema_version: 'a', policy_version: 'b', docs_version: 'c', prompt_version: 'd' };
  assert.notEqual(resultCacheKey('h', ctx), resultCacheKey('h', { ...ctx, snapshot_id: 't' }));
  assert.notEqual(resultCacheKey('h', ctx), resultCacheKey('h', { ...ctx, docs_version: 'x' }));
});

test('result comparison: order ignored, types differ (1 ≠ "1"), floats exact', () => {
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
/** Opens r_ too so ID dependence is checked on raw tables */
let policy = { panelReadablePrefixes: ['r_', 'd_'] };
let metrics: MetricDict;

test('dual run: all 4 demo seed panels pass with the same real and pseudonymized results', async () => {
  for (const { id, spec: s } of demoSeedPanels()) {
    const r = await run(s);
    assert.ok(r.ok, `${id}: ${!r.ok ? `${r.stage} ${r.message}` : ''}`);
    if (r.ok) assert.deepEqual(r.real, r.agent);
  }
});

test('dual run: ID columns in output are rejected (even aliased)', async () => {
  const r = await run(spec({ type: 'table' }, { sql: 'SELECT member_id AS who, count(*) AS n FROM d_activity GROUP BY member_id' }));
  assert.deepEqual([r.ok, !r.ok && r.stage], [false, 'id_column']);
  assert.match(!r.ok ? r.message : '', /who/);
});

test('dual run: panels depending on ID values (MIN(id), CAST(id AS TEXT), id + 0, ORDER BY id LIMIT 5) are id_dependent', async () => {
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

test('dual run: ID columns wrapped in UNION, subqueries or CTEs keep their origin (id_column); wrapped in expressions they are id_dependent', async () => {
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

test('dual run: even with dense real IDs 1..N, emitting IDs through an expression is id_dependent (pseudonyms are not 1..N)', async () => {
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

test('demo seed panel headline is fixed for the fixed seed', async () => {
  const p = demoSeedPanels().find((x) => x.id === 'recent_first_post')!;
  const r = await run(p.spec);
  assert.ok(r.ok);
  if (!r.ok) return;
  const h = computeHeadline(p.spec, r.columns, r.real)!;
  assert.deepEqual([h.numerator, h.denominator, h.lowN], [197, 601, false]);
  assert.ok(h.label);
});

test('dual run: failures per stage — static check, execution error, contract, invariants (pseudonymous keys)', async () => {
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

test('panel tables and metric dictionary: d_ only by default, tables recorded, metric table rule', async () => {
  const saved = { policy, metrics };
  try {
    policy = { panelReadablePrefixes: ['d_'] };
    metrics = loadMetrics(DEMO_DIR, ['d_'], demoSeedPanels().map((p) => ({ id: p.id, metric: p.spec.metric }))).dict;
    const raw = await run(spec({ type: 'table' }, { sql: 'SELECT count(*) AS n FROM r_post' }));
    assert.deepEqual([raw.ok, !raw.ok && raw.stage], [false, 'exec']);
    assert.match(!raw.ok ? raw.message : '', /r_post.*panel tables d_\*/);

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
    assert.match(!wrong.ok ? wrong.message : '', /Outside tables: d_activity/);
    const dimOnly = await run(spec({ type: 'table' }, { sql: 'SELECT count(*) AS n FROM d_calendar_week', metric: 'first_week_activation' }));
    assert.match(!dimOnly.ok ? dimOnly.message : '', /must read at least one of its tables/);
    const unknown = await run(spec({ type: 'table' }, { sql, metric: 'nope' }));
    assert.match(!unknown.ok ? unknown.message : '', /metric not in the dictionary: nope/);
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

test('columns with NULL small values (like GA4): as a numerator they break invariants; without NULL rows they pass', () => {
  const s = spec({ type: 'line', x: 'date', numerator: 'new_users', denominator: 'active_users' });
  const c = cols('date', 'new_users', 'active_users');
  assert.match(problems(s, c, [['2024-06-10', null, 50], ['2024-06-11', 12, 40]]).join('\n'), /new_users must be an integer ≥ 0/);
  assert.deepEqual(problems(s, c, [['2024-06-11', 12, 40]]), []);
});
