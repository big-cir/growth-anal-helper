// Screen renderer output escaping.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { renderPanel, headlineHtml } from '../../web/charts.js';

const EVIL = '<img src=x onerror=alert(1)>';
const cols = (...names) => names.map((name) => ({ name, table: null, column: null }));

test('all patterns escape result strings, column names and labels', () => {
  const views = [
    { display: { type: 'line', x: 'x', numerator: 'numerator', denominator: 'denominator', series: 's', extra: ['note'] }, columns: cols('x', 's', 'numerator', 'denominator', 'note'), rows: [[EVIL, EVIL, 1, 2, EVIL], ['b', EVIL, 1, 2, EVIL]] },
    { display: { type: 'bar', x: 'x', numerator: 'numerator', denominator: 'denominator', extra: ['note'] }, columns: cols('x', 'numerator', 'denominator', 'note'), rows: [[EVIL, 1, 2, EVIL]] },
    { display: { type: 'funnel', step_no: 'step_no', step_name: 'step_name', reached: 'reached', eligible: 'eligible', unknown: 'unknown', cohort: 'cohort' }, columns: cols('cohort', 'step_no', 'step_name', 'reached', 'eligible', 'unknown'), rows: [[EVIL, 1, EVIL, 5, 5, 0]] },
    { display: { type: 'cohort', cohort: 'cohort', period: 'period', numerator: 'numerator', denominator: 'denominator', series: 's' }, columns: cols('cohort', 's', 'period', 'numerator', 'denominator'), rows: [[EVIL, EVIL, 1, 1, 40]] },
    { display: { type: 'number', numerator: 'numerator', denominator: 'denominator', label: 'l' }, columns: cols('numerator', 'denominator', 'l'), rows: [[1, 2, EVIL]] },
    { display: { type: 'number', value: 'v', label: 'l' }, columns: cols('v', 'l'), rows: [[EVIL, EVIL]] },
    { display: { type: 'table' }, columns: cols(EVIL), rows: [[EVIL]] },
  ];
  for (const v of views) {
    const out = renderPanel(v, EVIL, {});
    assert.ok(!out.html.includes('<img'), `${v.display.type}: ${out.html.slice(0, 200)}`);
    if (out.controls) for (const o of out.controls.options) assert.equal(typeof o, 'string');
  }
  assert.ok(!headlineHtml({ value: 0.5, numerator: 1, denominator: 2, lowN: true, label: EVIL }).includes('<img'));
  assert.ok(!headlineHtml({ value: EVIL, numerator: null, denominator: null, lowN: false, label: '' }).includes('<img'));
});

test('n<30 cells are dimmed and get no heatmap color', () => {
  const v = { display: { type: 'cohort', cohort: 'cohort', period: 'period', numerator: 'numerator', denominator: 'denominator' }, columns: cols('cohort', 'period', 'numerator', 'denominator'), rows: [['A', 1, 10, 100], ['A', 2, 3, 12]] };
  const html = renderPanel(v, 't', {}).html;
  assert.match(html, /class="cell h\d"/);
  assert.match(html, /class="cell small"/);
});

test('table pattern: lines for a time first column, bars for categories, values escaped', async () => {
  const { tableChart } = await import('../../web/charts.js');
  const time = { display: { type: 'table' }, columns: cols('week', 'avg_members', EVIL), rows: [['2024-05-13', 3.5, 1], ['2024-05-06', 3.1, 2]] };
  const line = tableChart(time, 't');
  assert.match(line, /polyline class="ln s1"/);
  assert.ok(!line.includes(EVIL));
  const cat = { display: { type: 'table' }, columns: cols('board', 'n'), rows: [[EVIL, 3], ['b', 5]] };
  const bar = tableChart(cat, 't');
  assert.match(bar, /rect class="f1"/);
  assert.ok(!bar.includes(EVIL));
  assert.equal(tableChart({ display: { type: 'table' }, columns: cols('a', 'b'), rows: [['x', 'y'], ['z', 'w']] }, 't'), null, 'table only without numeric columns');
  const mixed = tableChart({ display: { type: 'table' }, columns: cols('week', 'avg', 'boards'), rows: [['2024-05-06', 3.1, 120], ['2024-05-13', 3.5, 130]] }, 't');
  assert.equal((mixed.match(/polyline/g) ?? []).length, 1, 'columns more than 10x off are not drawn');
  assert.match(mixed, /in the table only/);
  assert.equal(tableChart({ display: { type: 'table' }, columns: cols('a', 'n'), rows: [['x', 1]] }, 't'), null, 'table only for a single row');
});

test('chart shape switch: redraws line, bar and table panels without a rerun', async () => {
  const { chartModes, thumbChart } = await import('../../web/charts.js');
  const rate = { display: { type: 'line', x: 'x', numerator: 'numerator', denominator: 'denominator' }, columns: cols('x', 'numerator', 'denominator'), rows: [['W1', 1, 40], ['W2', 2, 40]] };
  assert.deepEqual(chartModes(rate), { auto: 'line', line: true, bar: true, table: true });
  assert.match(renderPanel(rate, 't', {}, 'bar').html, /<path d="M/);
  assert.ok(!renderPanel(rate, 't', {}, 'table').html.includes('<svg'));
  assert.equal(thumbChart(rate, 't', 'table'), null);

  const time = { display: { type: 'table' }, columns: cols('week', 'n'), rows: [['2024-05-13', 3], ['2024-05-06', 5]] };
  assert.equal(chartModes(time).auto, 'line');
  assert.match(renderPanel(time, 't', {}, 'bar').html, /rect class="f1"/);
  assert.ok(!renderPanel(time, 't', {}, 'table').html.includes('<svg'));

  const many = { display: { type: 'table' }, columns: cols('k', 'n'), rows: Array.from({ length: 31 }, (_, i) => [`c${i}`, i]) };
  assert.deepEqual(chartModes(many), { auto: 'table', line: true, bar: false, table: true });
  assert.match(renderPanel(many, 't', {}, 'line').html, /polyline/);

  assert.equal(chartModes({ display: { type: 'table' }, columns: cols('a', 'b'), rows: [['x', 'y'], ['z', 'w']] }), null);
  assert.equal(chartModes({ display: { type: 'number', value: 'v' }, columns: cols('v'), rows: [[1]] }), null);
});

test('cohort curves: latest 6 cohorts of the selected series, table kept below', async () => {
  const { chartModes, thumbChart } = await import('../../web/charts.js');
  const rows = [];
  for (let c = 1; c <= 8; c++) for (let p = 0; p <= 8 - c; p++) rows.push([`2024-W0${c}`, 'A', p, 10 + c, p === 0 && c === 8 ? 20 : 100]);
  rows.push(['2024-W08', 'B', 0, 5, 50]);
  const v = { display: { type: 'cohort', cohort: 'cohort', period: 'period', numerator: 'numerator', denominator: 'denominator', series: 's' }, columns: cols('cohort', 's', 'period', 'numerator', 'denominator'), rows };
  assert.deepEqual(chartModes(v), { auto: 'table', line: true, table: true });
  const html = renderPanel(v, 't', { series: 'A' }, 'line').html;
  assert.equal((html.match(/<polyline/g) ?? []).length, 6, 'one line per cohort, latest 6');
  assert.ok(!html.includes('2024-W02</span>') && html.includes('2024-W08</span>'));
  assert.match(html, /opacity="0.4"/, 'n<30 points dimmed');
  assert.match(html, /table class="heat"/);
  assert.ok(!renderPanel(v, 't', { series: 'A' }).html.includes('<svg'), 'table by default');
  assert.match(thumbChart(v, 't', 'line'), /<polyline/);
  assert.equal(thumbChart(v, 't'), null);
});
