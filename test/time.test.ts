import { test } from 'node:test';
import assert from 'node:assert/strict';
import { addDays, isNormalizedTs, normalizeTs, weekStart } from '../src/time.ts';

test('normalizeTs: pads fractions to 6 digits', () => {
  assert.equal(normalizeTs('2024-03-05 07:08:09'), '2024-03-05 07:08:09.000000');
  assert.equal(normalizeTs('2024-03-05 07:08:09.5'), '2024-03-05 07:08:09.500000');
  assert.equal(normalizeTs('2024-03-05T07:08:09.123456'), '2024-03-05 07:08:09.123456');
  assert.equal(normalizeTs('2024-03-05'), '2024-03-05 00:00:00.000000');
});

test('normalizeTs: rejects bad formats and non-existent dates', () => {
  for (const bad of ['', '2024-3-5 07:08:09', '2024-02-30 00:00:00', '2023-02-29 00:00:00', '2024-01-01 24:00:00', '2024-01-01 00:00:00.1234567', '2024-01-01 00:00', '0099-01-01 00:00:00']) {
    assert.throws(() => normalizeTs(bad), bad);
  }
  assert.equal(normalizeTs('2024-02-29 00:00:00'), '2024-02-29 00:00:00.000000');
});

test('normalized strings sort in time order', () => {
  const xs = ['2024-01-01 00:00:00.9', '2024-01-01 00:00:00.10', '2023-12-31 23:59:59.999999'].map(normalizeTs);
  assert.deepEqual([...xs].sort(), ['2023-12-31 23:59:59.999999', '2024-01-01 00:00:00.100000', '2024-01-01 00:00:00.900000']);
  assert.ok(isNormalizedTs(xs[0]));
  assert.ok(!isNormalizedTs('2024-01-01 00:00:00'));
});

test('weekStart: Monday 00:00', () => {
  assert.equal(weekStart('2024-03-04 12:00:00'), '2024-03-04 00:00:00.000000');
  assert.equal(weekStart('2024-03-10 23:59:59.999999'), '2024-03-04 00:00:00.000000');
  assert.equal(weekStart('2024-03-11 00:00:00'), '2024-03-11 00:00:00.000000');
  assert.equal(weekStart('2025-01-01 10:00:00'), '2024-12-30 00:00:00.000000');
});

test('addDays: month and leap-year boundaries, keeps the time', () => {
  assert.equal(addDays('2024-02-28 13:14:15.000001', 1), '2024-02-29 13:14:15.000001');
  assert.equal(addDays('2024-02-29 00:00:00', 1), '2024-03-01 00:00:00.000000');
  assert.equal(addDays('2024-01-01 00:00:00', -1), '2023-12-31 00:00:00.000000');
  assert.equal(addDays('2024-01-01 00:00:00', 56), '2024-02-26 00:00:00.000000');
  assert.throws(() => addDays('2024-01-01 00:00:00', 0.5));
  assert.throws(() => addDays('1000-01-01 00:00:00', -1));
  assert.throws(() => addDays('9999-12-31 00:00:00', 1));
});
