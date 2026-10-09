// Source cross-check: running and comparing query pairs, source SQL limits, target week.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { cpSync, readdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { buildDemoSnapshot, DEMO_DIR } from './helpers/demo-snapshot.ts';
import { runVerify, sameValue, verifyWeek, VerifyCancelled, VerifyError } from '../src/verify.ts';
import type { SourceAdapter } from '../src/collect/sources/source.ts';
import { bindSourceSql } from '../src/query/sql-lint.ts';

const W = { week_start: '2024-04-29 00:00:00.000000', week_end: '2024-05-06 00:00:00.000000', as_of: '2024-06-03 12:00:00.000000' };

test('source SQL: parameters become string literals; string contents are untouched', () => {
  assert.deepEqual(bindSourceSql("SELECT count(*) AS n, ':week_start' AS s FROM t WHERE c >= :week_start AND c < :week_end;", W),
    { ok: true, sql: "SELECT count(*) AS n, ':week_start' AS s FROM t WHERE c >= '2024-04-29 00:00:00.000000' AND c < '2024-05-06 00:00:00.000000'" });
});

test('source SQL: rejects multiple statements, writes, file writes, locks, delays and unknown parameters', () => {
  for (const sql of [
    'SELECT 1; SELECT 2',
    'DELETE FROM t',
    "SELECT 1 INTO OUTFILE '/x'",
    'SELECT a FROM t FOR UPDATE',
    'SELECT a FROM t FOR SHARE',
    'SELECT sleep(10)',
    "SELECT load_file('/x')",
    'SELECT :other',
    'WITH RECURSIVE n(x) AS (SELECT 1) SELECT x FROM n',
    // Syntax where MySQL may see statement boundaries differently: executable comments, -- without a space, #, backslash escapes
    "SELECT 1 /*! INTO OUTFILE '/tmp/x' */",
    'SELECT [x; DELETE FROM t]',
    'SELECT [x; SELECT sleep(99)]',
    'SELECT 1--x;\nDELETE FROM t',
    "SELECT 1 # ';\nDELETE FROM t",
    "SELECT 'x\\'' ; DELETE FROM t; SELECT '",
    'SELECT 1 -- note',
    // MySQL accepts UPDATE and DELETE after WITH
    'WITH c AS (SELECT 1) DELETE FROM t',
    'WITH c AS (SELECT 1) UPDATE t SET a = 1',
  ]) assert.equal(bindSourceSql(sql, W).ok, false, sql);
  // Quoted identifiers are not treated as words
  assert.equal(bindSourceSql('SELECT count(*) AS "into" FROM t', W).ok, true);
  assert.equal(bindSourceSql("SELECT '[x]' AS s FROM t", W).ok, true);
});

test('target week: defaults to the Monday five weeks before the cutoff week; Mondays and weeks before the cutoff only', () => {
  assert.deepEqual(verifyWeek('2024-06-03 12:00:00.000000'), { start: '2024-04-29 00:00:00.000000', end: '2024-05-06 00:00:00.000000' });
  assert.deepEqual(verifyWeek('2024-06-03 12:00:00.000000', '2024-05-27'), { start: '2024-05-27 00:00:00.000000', end: '2024-06-03 00:00:00.000000' });
  assert.throws(() => verifyWeek('2024-06-03 12:00:00.000000', '2024-05-28'), /Monday/);
  assert.throws(() => verifyWeek('2024-06-03 12:00:00.000000', '2024-06-03'), /does not end before/);
  assert.throws(() => verifyWeek('2024-06-03 12:00:00.000000', '2024-02-30'), /real date/);
});

test('value comparison: NULLs, numbers as numbers, everything else as text', () => {
  assert.ok(sameValue('12.50', '12.5'));
  assert.ok(sameValue(null, null));
  assert.ok(!sameValue(null, '0'));
  assert.ok(!sameValue('a', 'A'));
  // Large integers and long decimals compare exactly
  assert.ok(!sameValue('9007199254740992', '9007199254740993'));
  assert.ok(sameValue('9007199254740993', '9007199254740993.000'));
  assert.ok(!sameValue('0.1000000000000000000001', '0.1'));
  assert.ok(sameValue('1.5e3', '1500'));
  assert.ok(sameValue('-0', '0'));
  assert.ok(sameValue('007', '7'));
});

test('demo: all query pairs match and a log is written; different values, two rows and bad SQL do not match', async () => {
  const snap = await buildDemoSnapshot({ mutate: (d) => cpSync(join(DEMO_DIR, 'verify.json'), join(d, 'verify.json')) });
  const r = await runVerify(snap.ws);
  assert.equal(r.week_start, W.week_start);
  assert.ok(r.ok, JSON.stringify(r.items));
  assert.ok(Number(r.items[0].source!.n) > 0);
  assert.ok(readdirSync(join(snap.ws.config.outDir, 'logs')).some((f) => f.startsWith('verify-')));

  writeFileSync(join(snap.ws.dir, 'verify.json'), JSON.stringify({ checks: [
    { id: 'off_by_one', title: 'off', source_sql: 'SELECT count(*) + 1 AS n FROM member', snapshot_sql: 'SELECT count(*) AS n FROM r_member' },
    { id: 'two_rows', title: 'two rows', source_sql: 'SELECT id AS n FROM member', snapshot_sql: 'SELECT 1 AS n' },
    { id: 'col_names', title: 'column names', source_sql: 'SELECT 1 AS a', snapshot_sql: 'SELECT 1 AS b' },
    { id: 'bad_source', title: 'write', source_sql: 'DELETE FROM member', snapshot_sql: 'SELECT 1 AS n' },
    { id: 'bad_snapshot', title: 'unreadable table', source_sql: 'SELECT 1 AS n', snapshot_sql: 'SELECT count(*) AS n FROM sqlite_master' },
    { id: 'wide_row', title: 'wide row', source_sql: "SELECT substr(hex(zeroblob(1100)), 1, 2200) AS a, substr(hex(zeroblob(1100)), 1, 2200) AS b", snapshot_sql: 'SELECT 1 AS a, 1 AS b' },
  ] }));
  const bad = await runVerify(snap.ws);
  assert.equal(bad.ok, false);
  const by = Object.fromEntries(bad.items.map((i) => [i.id, i]));
  assert.match(by.off_by_one.error!, /different values: n/);
  assert.match(by.two_rows.error!, /more than one row/);
  assert.match(by.col_names.error!, /column names differ/);
  assert.match(by.bad_source.error!, /source SQL/);
  assert.match(by.bad_snapshot.error!, /snapshot/);
  assert.match(by.wide_row.error!, /4096 bytes/);
  assert.ok(bad.items.every((i) => !i.ok));
});

test('invalid verify.json is rejected before running', async () => {
  const snap = await buildDemoSnapshot();
  await assert.rejects(runVerify(snap.ws), (e) => e instanceof VerifyError && /verify.json not found/.test(e.message));
  writeFileSync(join(snap.ws.dir, 'verify.json'), JSON.stringify({ checks: [{ id: 'A', title: 'x', source_sql: 'SELECT 1', snapshot_sql: 'SELECT 1' }] }));
  await assert.rejects(runVerify(snap.ws), /identifier/);
});

test('cancel: before start or during the snapshot query gives VerifyCancelled', async () => {
  const snap = await buildDemoSnapshot({ mutate: (d) => cpSync(join(DEMO_DIR, 'verify.json'), join(d, 'verify.json')) });
  const before = new AbortController();
  before.abort();
  await assert.rejects(runVerify(snap.ws, { signal: before.signal }), (e) => e instanceof VerifyCancelled);
  // Abort right after the source value is returned → stops at the snapshot query
  const ac = new AbortController();
  const source: SourceAdapter = {
    dialect: 'sqlite',
    now: async () => '',
    abort: () => {},
    selectStream: async (_sql, onRow, onColumns) => {
      onColumns?.(['n']);
      onRow(['1']);
      ac.abort();
      return { columns: ['n'], rows: 1, ms: 0 };
    },
  };
  await assert.rejects(runVerify(snap.ws, { source, signal: ac.signal }), (e) => e instanceof VerifyCancelled);
});
