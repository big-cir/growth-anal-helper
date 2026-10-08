// node:sqlite setAuthorizer 동작 확인.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { DatabaseSync, constants as C } from 'node:sqlite';

type Call = [number, string | null, string | null, string | null, string | null];

function record(sql: string): Call[] {
  const db = new DatabaseSync(':memory:');
  db.exec("CREATE TABLE r_post(id INTEGER PRIMARY KEY, title TEXT); INSERT INTO r_post VALUES (1,'ab'),(2,'cd'); CREATE TABLE hidden(x)");
  const calls: Call[] = [];
  db.setAuthorizer((...args: unknown[]) => {
    calls.push(args as Call);
    return C.SQLITE_OK;
  });
  db.prepare(sql).all();
  db.close();
  return calls;
}

const has = (calls: Call[], code: number, a1: string | null, a2: string | null) =>
  calls.some((c) => c[0] === code && c[1] === a1 && c[2] === a2);

test('콜백 인자: (동작, 인자1, 인자2, DB 이름, CTE·뷰 이름)', () => {
  const calls = record('SELECT id FROM r_post');
  assert.deepEqual(calls[0], [C.SQLITE_SELECT, null, null, null, null]);
  assert.deepEqual(calls.find((c) => c[0] === C.SQLITE_READ), [C.SQLITE_READ, 'r_post', 'id', 'main', null]);
});

test('함수: (FUNCTION, null, 함수 이름). LIKE·GLOB는 함수로 보고된다', () => {
  assert.ok(has(record("SELECT title LIKE 'a%' FROM r_post"), C.SQLITE_FUNCTION, null, 'like'));
  assert.ok(has(record("SELECT title GLOB 'a*' FROM r_post"), C.SQLITE_FUNCTION, null, 'glob'));
  assert.ok(has(record('SELECT count(*) FROM r_post'), C.SQLITE_FUNCTION, null, 'count'));
  assert.ok(has(record('SELECT row_number() OVER (ORDER BY id) FROM r_post'), C.SQLITE_FUNCTION, null, 'row_number'));
});

test('CAST·CASE·IN·BETWEEN은 함수로 보고되지 않는다', () => {
  const calls = record('SELECT CAST(id AS TEXT), CASE WHEN id IN (1,2) THEN 1 END, id BETWEEN 1 AND 2 FROM r_post');
  assert.deepEqual(calls.filter((c) => c[0] === C.SQLITE_FUNCTION), []);
});

test('CTE: 이름이 아니라 안에서 읽는 실제 테이블이 보고되고, 다섯째 인자에 CTE 이름', () => {
  const calls = record('WITH c AS (SELECT id FROM r_post) SELECT count(*) FROM c');
  assert.ok(calls.some((c) => c[0] === C.SQLITE_READ && c[1] === 'r_post' && c[2] === 'id' && c[4] === 'c'));
  assert.ok(!calls.some((c) => c[0] === C.SQLITE_READ && c[1] === 'c'));
});

test('pragma_* 테이블 함수: READ(pragma_…)와 PRAGMA 동작이 함께 보고된다', () => {
  const calls = record("SELECT name FROM pragma_table_info('r_post')");
  assert.ok(calls.some((c) => c[0] === C.SQLITE_READ && c[1] === 'pragma_table_info'));
  assert.ok(has(calls, C.SQLITE_PRAGMA, 'table_info', 'r_post'));
});

test('테이블 값 함수(json_each)는 함수가 아니라 테이블 읽기로 보고된다 → 접두사 검사로 막힘', () => {
  const calls = record("SELECT value FROM json_each('[1,2]')");
  assert.ok(calls.some((c) => c[0] === C.SQLITE_READ && c[1] === 'json_each'));
  assert.ok(!calls.some((c) => c[0] === C.SQLITE_FUNCTION && c[2] === 'json_each'));
});

test('FTS5 가상 테이블 조회는 내부 PRAGMA(data_version)를 일으킨다 → 정책상 거부됨 (스냅샷에는 가상 테이블을 두지 않음)', () => {
  const db = new DatabaseSync(':memory:');
  db.exec("CREATE VIRTUAL TABLE r_doc USING fts5(body); INSERT INTO r_doc VALUES ('hello')");
  const calls: Call[] = [];
  db.setAuthorizer((...args: unknown[]) => { calls.push(args as Call); return C.SQLITE_OK; });
  db.prepare("SELECT body FROM r_doc WHERE r_doc MATCH 'hello'").all();
  db.close();
  assert.ok(has(calls, C.SQLITE_PRAGMA, 'data_version', null));
  assert.ok(has(calls, C.SQLITE_FUNCTION, null, 'match'));
});

test('DENY를 돌려주면 prepare가 실패한다', () => {
  const db = new DatabaseSync(':memory:');
  db.exec('CREATE TABLE hidden(x)');
  db.setAuthorizer((code: number, a1: string | null) =>
    code === C.SQLITE_READ && a1 === 'hidden' ? C.SQLITE_DENY : C.SQLITE_OK);
  assert.throws(() => db.prepare('SELECT x FROM hidden'), /prohibited|not authorized/i);
  db.close();
});
