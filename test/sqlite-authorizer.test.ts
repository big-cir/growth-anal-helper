// How node:sqlite setAuthorizer behaves.
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

test('callback args: (action, arg1, arg2, DB name, CTE or view name)', () => {
  const calls = record('SELECT id FROM r_post');
  assert.deepEqual(calls[0], [C.SQLITE_SELECT, null, null, null, null]);
  assert.deepEqual(calls.find((c) => c[0] === C.SQLITE_READ), [C.SQLITE_READ, 'r_post', 'id', 'main', null]);
});

test('functions: (FUNCTION, null, function name). LIKE and GLOB are reported as functions', () => {
  assert.ok(has(record("SELECT title LIKE 'a%' FROM r_post"), C.SQLITE_FUNCTION, null, 'like'));
  assert.ok(has(record("SELECT title GLOB 'a*' FROM r_post"), C.SQLITE_FUNCTION, null, 'glob'));
  assert.ok(has(record('SELECT count(*) FROM r_post'), C.SQLITE_FUNCTION, null, 'count'));
  assert.ok(has(record('SELECT row_number() OVER (ORDER BY id) FROM r_post'), C.SQLITE_FUNCTION, null, 'row_number'));
});

test('CAST, CASE, IN and BETWEEN are not reported as functions', () => {
  const calls = record('SELECT CAST(id AS TEXT), CASE WHEN id IN (1,2) THEN 1 END, id BETWEEN 1 AND 2 FROM r_post');
  assert.deepEqual(calls.filter((c) => c[0] === C.SQLITE_FUNCTION), []);
});

test('CTE: the real tables read inside are reported, with the CTE name as the fifth arg', () => {
  const calls = record('WITH c AS (SELECT id FROM r_post) SELECT count(*) FROM c');
  assert.ok(calls.some((c) => c[0] === C.SQLITE_READ && c[1] === 'r_post' && c[2] === 'id' && c[4] === 'c'));
  assert.ok(!calls.some((c) => c[0] === C.SQLITE_READ && c[1] === 'c'));
});

test('pragma_* table functions: READ(pragma_…) and PRAGMA are both reported', () => {
  const calls = record("SELECT name FROM pragma_table_info('r_post')");
  assert.ok(calls.some((c) => c[0] === C.SQLITE_READ && c[1] === 'pragma_table_info'));
  assert.ok(has(calls, C.SQLITE_PRAGMA, 'table_info', 'r_post'));
});

test('table-valued functions (json_each) are reported as table reads, so the prefix check blocks them', () => {
  const calls = record("SELECT value FROM json_each('[1,2]')");
  assert.ok(calls.some((c) => c[0] === C.SQLITE_READ && c[1] === 'json_each'));
  assert.ok(!calls.some((c) => c[0] === C.SQLITE_FUNCTION && c[2] === 'json_each'));
});

test('FTS5 virtual table queries trigger an internal PRAGMA (data_version), so policy denies them (snapshots have no virtual tables)', () => {
  const db = new DatabaseSync(':memory:');
  db.exec("CREATE VIRTUAL TABLE r_doc USING fts5(body); INSERT INTO r_doc VALUES ('hello')");
  const calls: Call[] = [];
  db.setAuthorizer((...args: unknown[]) => { calls.push(args as Call); return C.SQLITE_OK; });
  db.prepare("SELECT body FROM r_doc WHERE r_doc MATCH 'hello'").all();
  db.close();
  assert.ok(has(calls, C.SQLITE_PRAGMA, 'data_version', null));
  assert.ok(has(calls, C.SQLITE_FUNCTION, null, 'match'));
});

test('returning DENY makes prepare fail', () => {
  const db = new DatabaseSync(':memory:');
  db.exec('CREATE TABLE hidden(x)');
  db.setAuthorizer((code: number, a1: string | null) =>
    code === C.SQLITE_READ && a1 === 'hidden' ? C.SQLITE_DENY : C.SQLITE_OK);
  assert.throws(() => db.prepare('SELECT x FROM hidden'), /prohibited|not authorized/i);
  db.close();
});
