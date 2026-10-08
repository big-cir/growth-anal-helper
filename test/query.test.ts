// 쿼리 실행: 정적 검사, 권한, worker, 실행 슬롯.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { DatabaseSync } from 'node:sqlite';
import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { lintSql } from '../src/query/sql-lint.ts';
import { runInWorker, type WorkerInput } from '../src/query/worker.ts';
import { ExecutionSlots, LIMITS, runQuery, SlotCancelled, type QueryRequest, type SlotLease } from '../src/query/executor.ts';


test('lint: 문장 하나, SELECT/WITH만, 끝의 ; 허용, 문자열·주석 안의 ;·키워드는 무시', () => {
  assert.deepEqual(lintSql('SELECT 1;', []), { ok: true, params: [] });
  assert.deepEqual(lintSql("SELECT ';' AS a, 'DROP TABLE x' -- ; comment\n /* ; */ FROM r_t", []), { ok: true, params: [] });
  assert.deepEqual(lintSql('WITH c AS (SELECT 1) SELECT * FROM c', []), { ok: true, params: [] });
  for (const bad of ['SELECT 1; SELECT 2', 'SELECT 1;;', 'DELETE FROM r_t', 'PRAGMA table_info(r_t)', "ATTACH 'x' AS y", 'VALUES (1)', '', '  ', "SELECT 'open", 'SELECT /* open', 'SELECT [open']) {
    assert.equal(lintSql(bad, []).ok, false, bad);
  }
});

test('lint: WITH RECURSIVE 거부', () => {
  const r = lintSql('WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM n) SELECT x FROM n', []);
  assert.equal(r.ok, false);
  assert.match((r as { message: string }).message, /d_calendar_week/);
});

test('lint: 매개변수는 :as_of와 워크스페이스 스칼라 키만', () => {
  assert.deepEqual(lintSql('SELECT :as_of, :start, :as_of', ['start']), { ok: true, params: ['as_of', 'start'] });
  assert.deepEqual(lintSql("SELECT '12:30' AS t, \"col:x\" FROM r_t", []), { ok: true, params: [] });
  for (const bad of ['SELECT ?', 'SELECT ?1', 'SELECT @x', 'SELECT $x', 'SELECT :1', 'SELECT :other']) {
    assert.equal(lintSql(bad, ['start']).ok, false, bad);
  }
});


function testDb(): string {
  const p = join(mkdtempSync(join(tmpdir(), 'gl-q-')), 'snap.sqlite');
  const db = new DatabaseSync(p);
  db.exec(`
    CREATE TABLE r_t (id INTEGER PRIMARY KEY, name TEXT, v);
    CREATE TABLE d_big (id INTEGER PRIMARY KEY, s TEXT);
    CREATE TABLE d_ko (id INTEGER PRIMARY KEY, s TEXT);
    CREATE TABLE hidden (secret TEXT);
    INSERT INTO hidden VALUES ('x');
    INSERT INTO r_t VALUES (1, 'a', 9007199254740993), (2, 'b', x'00ff'), (3, 'c', 1.5), (4, 'd', 7);
  `);
  const ins = db.prepare('INSERT INTO d_big VALUES (?, ?)');
  db.exec('BEGIN');
  for (let i = 1; i <= 6000; i++) ins.run(i, i === 1 ? 'x'.repeat(5000) : 'y'.repeat(900));
  const ko = db.prepare('INSERT INTO d_ko VALUES (?, ?)');
  for (let i = 1; i <= 1200; i++) ko.run(i, '가'.repeat(1300));
  db.exec('COMMIT');
  db.close();
  return p;
}
const DB = testDb();

const workerIn = (sql: string, over: Partial<WorkerInput> = {}): WorkerInput => ({
  path: DB, sql, params: {}, readablePrefixes: ['r_', 'd_'], heapLimitMb: 256,
  maxRows: 50, overflow: 'more', cellLimit: 4096, truncateCells: true, outputLimit: 4 * 1024 * 1024, ...over,
});

test('authorizer: 허용 접두사 밖 테이블·sqlite_master·pragma_*·json_each·허용 밖 함수·쓰기는 실행 단계에서 거부', () => {
  for (const sql of [
    'SELECT secret FROM hidden',
    'SELECT name FROM sqlite_master',
    "SELECT name FROM pragma_table_info('r_t')",
    "SELECT value FROM json_each('[1]')",
    "SELECT printf('%d', id) FROM r_t",
    'SELECT randomblob(4)',
    'SELECT zeroblob(4)',
    "SELECT load_extension('x')",
    "SELECT json_extract('{}', '$.a')",
    'INSERT INTO r_t (id) VALUES (99)',
    'DELETE FROM r_t',
    'PRAGMA table_info(r_t)',
    "ATTACH DATABASE ':memory:' AS x",
    'CREATE TABLE r_new (a)',
  ]) {
    const r = runInWorker(workerIn(sql));
    assert.equal(r.ok, false, sql);
    assert.equal((r as { kind: string }).kind, 'sqlite', sql);
  }
});

test('authorizer: 허용 함수·LIKE·GLOB·CTE·창 함수·CAST·CASE는 통과, 결과 칸의 원본 표시', () => {
  const r = runInWorker(workerIn(`
    WITH c AS (SELECT id, name FROM r_t WHERE name LIKE '%' AND name GLOB '*')
    SELECT c.id AS k, upper(c.name) AS u, row_number() OVER (ORDER BY c.id) AS rn,
           CAST(c.id AS TEXT) AS t, CASE WHEN c.id IN (1, 2) THEN 'x' END AS cs, date('2024-01-01') AS d
    FROM c ORDER BY c.id`));
  assert.ok(r.ok, JSON.stringify(r));
  if (!r.ok) return;
  assert.deepEqual(r.columns[0], { name: 'k', table: 'r_t', column: 'id' });
  assert.deepEqual(r.columns[1], { name: 'u', table: null, column: null });
  assert.deepEqual(r.rows[0], [['i', 1], ['s', 'A'], ['i', 1], ['s', '1'], ['s', 'x'], ['s', '2024-01-01']]);
});

test('결과 타입: BLOB·안전 범위 밖 정수·무한대는 오류, 실수는 f 태그', () => {
  assert.deepEqual((runInWorker(workerIn('SELECT v FROM r_t WHERE id = 1')) as { kind: string }).kind, 'type');
  assert.match((runInWorker(workerIn('SELECT v FROM r_t WHERE id = 2')) as { message: string }).message, /BLOB/);
  assert.match((runInWorker(workerIn('SELECT 1e999 AS inf')) as { message: string }).message, /유한하지 않은/);
  const r = runInWorker(workerIn('SELECT v FROM r_t WHERE id = 3'));
  assert.ok(r.ok && JSON.stringify(r.rows) === '[[["f",1.5]]]');
});


const SLOTS = new ExecutionSlots(8, 8);
async function q(sql: string, over: Partial<Omit<QueryRequest, 'lease'>> = {}) {
  return SLOTS.run('interactive', undefined, (lease) => runQuery(req(sql, lease, over)));
}

const req = (sql: string, lease: SlotLease, over: Partial<QueryRequest> = {}): QueryRequest => ({
  lease, sql, path: DB, mode: 'probe', asOf: '2024-06-01 00:00:00.000000', params: { start: '2024-01-01', marks: ['a', 'b'] },
  readablePrefixes: ['r_', 'd_'], heapLimitMb: 256, ...over,
});

test('탐색: 51행까지 읽고 50행 + 더 있음, 큰 셀은 잘라서 표시', async () => {
  const r = await q('SELECT id, s FROM d_big ORDER BY id');
  assert.ok(r.ok);
  if (!r.ok) return;
  assert.equal(r.rows.length, 50);
  assert.equal(r.more, true);
  assert.equal(r.truncatedCells, 1);
  assert.equal((r.rows[0][1][1] as string).length, LIMITS.cellBytes);
});

test('패널: 4KB 넘는 셀 거부, 5,000행 넘으면 거부, 출력 4MB 넘으면 거부', async () => {
  const cell = await q('SELECT s FROM d_big WHERE id = 1', { mode: 'panel' });
  assert.deepEqual([cell.ok, !cell.ok && cell.kind], [false, 'limit']);
  const many = await q('SELECT id FROM d_big', { mode: 'panel' });
  assert.match(!many.ok ? many.message : '', /5000행/);
  const big = await q('SELECT id, s, s AS s2, s AS s3, s AS s4, s AS s5 FROM d_big WHERE id > 1 AND id <= 5000', { mode: 'panel' });
  assert.match(!big.ok ? big.message : '', /바이트를 넘음/);
});

test('출력 상한은 UTF-8 바이트로 센다(다바이트 글자), 상한 오류 뒤 worker가 남지 않는다', async () => {
  const before = new Set(workerPids());
  const r = await q('SELECT id, s FROM d_ko', { mode: 'panel' });
  assert.deepEqual([r.ok, !r.ok && r.kind], [false, 'limit']);
  assert.deepEqual(workerPids().filter((p) => !before.has(p)), []);
});

test('실행 슬롯: 직접 만든 lease·반납한 lease·같은 lease 동시 사용은 거부, 차례로 쓰는 건 허용', async () => {
  const msg = '실행 슬롯 없이(또는 같은 슬롯으로 동시에) 쿼리를 실행할 수 없음';
  const forged = { kind: 'interactive', active: true, release() {}, begin: () => true, end() {} } as unknown as SlotLease;
  assert.deepEqual(await runQuery(req('SELECT 1', forged)), { ok: false, kind: 'input', message: msg });
  assert.throws(() => new (SlotLease as unknown as new (...a: unknown[]) => SlotLease)(Symbol('x'), 'interactive', () => {}));

  const lease = await SLOTS.acquire('interactive');
  const slow = runQuery(req('SELECT count(*) FROM d_big a, d_big b', lease));
  assert.deepEqual(await runQuery(req('SELECT 1', lease)), { ok: false, kind: 'input', message: msg });
  assert.ok((await slow).ok);
  assert.ok((await runQuery(req('SELECT 1', lease))).ok);
  lease.release();
  assert.deepEqual(await runQuery(req('SELECT 1', lease)), { ok: false, kind: 'input', message: msg });
  assert.equal(typeof (lease as unknown as { begin?: unknown }).begin, 'undefined');
});

test('실행 슬롯: 쿼리 실행 중 release하면 그 쿼리가 끝날 때 반납된다', async () => {
  const s = new ExecutionSlots(1, 1);
  const lease = await s.acquire('interactive');
  const running = runQuery(req('SELECT count(*) FROM d_big a, d_big b', lease));
  lease.release();
  assert.equal(s.stats.inUse, 1);
  assert.equal(lease.active, true);
  assert.deepEqual(await runQuery(req('SELECT 1', lease)), { ok: false, kind: 'input', message: '실행 슬롯 없이(또는 같은 슬롯으로 동시에) 쿼리를 실행할 수 없음' });
  assert.ok((await running).ok);
  assert.equal(s.stats.inUse, 0);
  assert.equal(lease.active, false);
});

test('매개변수: :as_of와 스칼라 params 바인딩, 배열 params·모르는 이름은 거부', async () => {
  const r = await q('SELECT :as_of AS a, :start AS s');
  assert.ok(r.ok && JSON.stringify(r.rows) === '[[["s","2024-06-01 00:00:00.000000"],["s","2024-01-01"]]]');
  assert.equal((await q('SELECT :marks')).ok, false);
  assert.equal((await q('SELECT :nope')).ok, false);
});

function workerPids(): number[] {
  try {
    // 이 테스트 프로세스가 띄운 worker만(다른 테스트 파일이 동시에 띄운 것은 제외)
    return execFileSync('pgrep', ['-P', String(process.pid), '-f', 'src/query/worker.ts'], { encoding: 'utf8' }).split('\n').filter(Boolean).map(Number);
  } catch {
    return [];
  }
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

test('오래 걸리는 쿼리: 시간 제한 없이 돌고, 취소하면 프로세스 그룹이 죽는다', async () => {
  const ac = new AbortController();
  const slow = 'SELECT count(*) FROM d_big a, d_big b, d_big c';
  const before = new Set(workerPids());
  const p = q(slow, { signal: ac.signal });
  await new Promise((r) => setTimeout(r, 800));
  const workers = workerPids().filter((x) => !before.has(x));
  assert.equal(workers.length, 1, '실행 중인 worker 1개');
  const t0 = Date.now();
  ac.abort();
  const r = await p;
  assert.deepEqual([r.ok, !r.ok && r.kind], [false, 'cancelled']);
  assert.ok(Date.now() - t0 < 2000);
  assert.equal(alive(workers[0]), false);
});

test('이미 취소된 요청은 worker를 띄우지 않는다', async () => {
  const ac = new AbortController();
  ac.abort();
  const r = await q('SELECT 1', { signal: ac.signal });
  assert.deepEqual([r.ok, !r.ok && r.kind], [false, 'cancelled']);
});


test('슬롯: 전체 2개, 백그라운드는 1개까지, 대화 요청이 먼저', async () => {
  const s = new ExecutionSlots(2, 1);
  const order: string[] = [];
  const bg1 = await s.acquire('background');
  const bg2p = s.acquire('background').then((l) => { order.push('bg2'); return l; });
  const i1 = await s.acquire('interactive');
  assert.deepEqual(s.stats, { inUse: 2, backgroundInUse: 1, waiting: 1 });
  const i2p = s.acquire('interactive').then((l) => { order.push('i2'); return l; });
  bg1.release();
  const i2 = await i2p;
  assert.deepEqual(order, ['i2']);
  assert.equal(bg1.active, false);
  i1.release();
  const bg2 = await bg2p;
  assert.deepEqual(order, ['i2', 'bg2']);
  assert.deepEqual(s.stats, { inUse: 2, backgroundInUse: 1, waiting: 0 });
  i2.release();
  bg2.release();
  bg2.release();
  assert.deepEqual(s.stats, { inUse: 0, backgroundInUse: 0, waiting: 0 });
});

test('슬롯: run()은 실패해도 반납한다', async () => {
  const s = new ExecutionSlots(1, 1);
  await assert.rejects(s.run('interactive', undefined, async () => { throw new Error('x'); }), /x/);
  assert.equal(s.stats.inUse, 0);
  assert.equal(await s.run('background', undefined, async (lease) => lease.active), true);
  assert.equal(s.stats.inUse, 0);
});

test('슬롯: 기다리는 중 취소하면 대기열에서 빠진다', async () => {
  const s = new ExecutionSlots(1, 1);
  const held = await s.acquire('interactive');
  const ac = new AbortController();
  const waiting = s.acquire('interactive', ac.signal);
  ac.abort();
  await assert.rejects(waiting, SlotCancelled);
  assert.equal(s.stats.waiting, 0);
  held.release();
  assert.equal(s.stats.inUse, 0);
});

function runWorkerRaw(input: string): Promise<string> {
  return new Promise((resolve) => {
    const worker = join(import.meta.dirname, '..', 'src', 'query', 'worker.ts');
    const child = spawn(process.execPath, [worker], { stdio: ['pipe', 'pipe', 'inherit'] });
    let out = '';
    child.stdout.on('data', (b) => { out += b; });
    child.stdin.on('error', () => {});
    child.on('close', () => resolve(out));
    child.stdin.end(input);
  });
}

test('worker 단독 실행: 64KB 넘는 입력은 읽다가 바로 거부', async () => {
  assert.deepEqual(JSON.parse(await runWorkerRaw('x'.repeat(200 * 1024))), { ok: false, kind: 'input', message: '입력이 64KB를 넘음' });
  const ok = JSON.parse(await runWorkerRaw(JSON.stringify(workerIn('SELECT 1 AS one'))));
  assert.equal(ok.rows[0][0][1], 1);
});
