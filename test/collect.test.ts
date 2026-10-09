// Collect, finalize, pseudonymized copy, derive, test-derived.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { DatabaseSync } from 'node:sqlite';
import { cpSync, existsSync, mkdtempSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { parseSpec, selectSql, specHash, SpecError } from '../src/collect/spec.ts';
import { MysqlSource } from '../src/collect/sources/mysql.ts';
import { PostgresSource } from '../src/collect/sources/postgres.ts';
import { SqliteSource } from '../src/collect/sources/sqlite.ts';
import type { SourceAdapter } from '../src/collect/sources/source.ts';
import { collect } from '../src/collect/collector.ts';
import { finalize, loadDerivedRoles } from '../src/collect/finalize.ts';
import { pseudonymize, PseudonymizeError, PSEUDO_MAX, PSEUDO_MIN } from '../src/snapshot/pseudonymize.ts';
import { readCurrent } from '../src/snapshot/store.ts';
import { loadBuildInputs, runCollect, runDerive, runTestDerived } from '../src/snapshot/pipeline.ts';
import { loadWorkspace, type ServerDatasource } from '../src/workspace.ts';
import { fakeMysql, fakePostgres, type Table } from './helpers/fake-db.ts';
import { seedDemo } from '../examples/demo/seed.ts';

const ROOT = join(import.meta.dirname, '..');
const DEMO = join(ROOT, 'examples', 'demo');
const ANCHOR = '2024-06-03 12:00:00';

const col = (o: Record<string, unknown>) => ({ kind: 'int', role: 'ordinary', ...o });
const tableSpec = (cols: unknown[], extra: Record<string, unknown> = {}) => [{
  source: 'thing', target: 'r_thing', key: ['id'], cutoffColumn: 'created_at',
  columns: [col({ expr: 'id', as: 'id', role: { identifier: 'thing' } }), col({ expr: 'created_at', as: 'created_at', kind: 'ts' }), ...cols],
  ...extra,
}];


test('spec: demo tables.json passes', () => {
  const specs = parseSpec(JSON.parse(readFileSync(join(DEMO, 'tables.json'), 'utf8')));
  assert.equal(specs.length, 6);
  const reply = specs.find((t) => t.target === 'r_reply')!;
  assert.equal(selectSql(reply, 'mysql'), 'SELECT id AS id, post_id AS post_id, member_id AS member_id, parent_reply_id IS NOT NULL AS is_nested, created_at AS created_at, deleted_at AS deleted_at FROM reply ORDER BY id');
  assert.equal(selectSql(reply, 'postgres'), "SELECT id AS id, post_id AS post_id, member_id AS member_id, CAST(parent_reply_id IS NOT NULL AS int) AS is_nested, to_char(created_at, 'YYYY-MM-DD HH24:MI:SS.US') AS created_at, to_char(deleted_at, 'YYYY-MM-DD HH24:MI:SS.US') AS deleted_at FROM reply ORDER BY id");
});

test('spec: every rule violation is rejected', () => {
  const bad: unknown[] = [
    tableSpec([col({ expr: 'name; DROP TABLE x', as: 'name', kind: 'text' })]),
    tableSpec([col({ expr: 'lower(name)', as: 'name', kind: 'text' })]),
    tableSpec([col({ expr: 'a IS NOT NULL', as: 'flag', kind: 'int' })]),
    tableSpec([col({ expr: 'name', as: 'name', kind: 'text', role: { identifier: 'x' } })]),
    tableSpec([col({ expr: 'name', as: 'name', kind: 'text' })]).map((t) => ({ ...t, key: ['created_at'] })),
    tableSpec([col({ expr: 'n', as: 'n' })]).map((t) => ({ ...t, cutoffColumn: 'n' })),
    tableSpec([col({ expr: 'n', as: 'n', nullAfterCutoff: true })]),
    tableSpec([col({ expr: 'n', as: 'id' })]),
    tableSpec([col({ expr: 'n', as: 'n', role: undefined })]),
    tableSpec([col({ expr: 'n', as: 'n', extra: 1 })]),
    tableSpec([]).map((t) => ({ ...t, target: 'thing' })),
    [...tableSpec([]), ...tableSpec([])],
    tableSpec([col({ expr: 'name', as: 'name', kind: 'int', maxLength: 10 })]),
  ];
  for (const b of bad) assert.throws(() => parseSpec(b), SpecError, JSON.stringify(b));
});

test('spec hash ignores roles', () => {
  const a = parseSpec(tableSpec([col({ expr: 'n', as: 'n' })]));
  const b = parseSpec(tableSpec([col({ expr: 'n', as: 'n', role: { identifier: 'other' } })]));
  const c = parseSpec(tableSpec([col({ expr: 'm', as: 'n' })]));
  assert.equal(specHash(a), specHash(b));
  assert.notEqual(specHash(a), specHash(c));
});


const TABLE: Table = { columns: ['id', 'name', 'note'], rows: [['1', 'Alice', 'NULL'], ['2', 'Tab\there', null], ['3', '한글\n줄바꿈\\x', '']] };
const ds = (kind: 'mysql' | 'postgres', port: number, password: string): ServerDatasource => ({ kind, host: '127.0.0.1', port, user: 'ro', password, database: 'board' });

async function rowsOf(src: SourceAdapter, sql = 'SELECT id, name, note FROM t') {
  const rows: (string | null)[][] = [];
  const r = await src.selectStream(sql, (v) => rows.push(v));
  return { columns: r.columns, rows };
}

for (const plugin of ['mysql_native_password', 'caching_sha2_password'] as const) {
  for (const fullAuth of plugin === 'caching_sha2_password' ? [false, true] : [false]) {
    test(`MySQL source (${plugin}${fullAuth ? ', full auth' : ''}): auth, read-only session, NULL vs string "NULL" vs special characters`, async () => {
      const db = await fakeMysql({ user: 'ro', password: 'pw:한글', plugin, fullAuth, table: TABLE });
      try {
        const src = new MysqlSource(ds('mysql', db.port, 'pw:한글'));
        assert.deepEqual(await rowsOf(src), TABLE);
        assert.equal(await src.now(), '2024-03-20 12:00:00.123456');
        assert.deepEqual(db.queries.slice(0, 2), ['SET SESSION TRANSACTION READ ONLY', 'SELECT id, name, note FROM t']);
        assert.deepEqual(db.startup[0], { user: 'ro', database: 'board' });
      } finally {
        await db.close();
      }
    });
  }
}

test('MySQL source: wrong password and SQL errors fail with the server message', async () => {
  const db = await fakeMysql({ user: 'ro', password: 'right', table: TABLE });
  try {
    await assert.rejects(rowsOf(new MysqlSource(ds('mysql', db.port, 'wrong'))), /MySQL error 1064|Access denied/);
    await assert.rejects(rowsOf(new MysqlSource(ds('mysql', db.port, 'right')), 'SELECT fail'), /Unknown column/);
  } finally {
    await db.close();
  }
});

for (const auth of ['scram', 'md5'] as const) {
  test(`PostgreSQL source (${auth}): auth, read-only session option, NULL vs special characters`, async () => {
    const db = await fakePostgres({ user: 'ro', password: 'pw:한글', auth, table: TABLE });
    try {
      const src = new PostgresSource(ds('postgres', db.port, 'pw:한글'));
      assert.deepEqual(await rowsOf(src), TABLE);
      assert.equal(await src.now(), '2024-03-20 12:00:00.123456');
      assert.equal(db.startup[0].user, 'ro');
      assert.equal(db.startup[0].database, 'board');
      assert.equal(db.startup[0].options, '-c default_transaction_read_only=on');
      await assert.rejects(rowsOf(src, 'SELECT fail'), /PostgreSQL error 28P01: column "x" does not exist/);
      await assert.rejects(rowsOf(new PostgresSource(ds('postgres', db.port, 'wrong'))), /password authentication failed/);
    } finally {
      await db.close();
    }
  });
}

test('connection failure and cancel', async () => {
  const db = await fakeMysql({ user: 'ro', password: 'pw', table: TABLE });
  const port = db.port;
  await db.close();
  await assert.rejects(rowsOf(new MysqlSource(ds('mysql', port, 'pw'))), /cannot connect to the database \(127\.0\.0\.1:\d+\)/);
  const live = await fakePostgres({ user: 'ro', password: 'pw', table: TABLE });
  try {
    const src = new PostgresSource(ds('postgres', live.port, 'pw'));
    await assert.rejects(src.selectStream('SELECT 1', () => src.abort()), /cancelled/);
    await assert.rejects(rowsOf(src), /cancelled/);
  } finally {
    await live.close();
  }
});

test('collect: a header that differs from the spec stops before the first row and leaves no temporary file', async () => {
  const specs = parseSpec([{
    source: 't', target: 'r_t', key: ['id'], cutoffColumn: 'ts',
    columns: [col({ expr: 'id', as: 'id' }), col({ expr: 'name', as: 'name', kind: 'text' }), col({ expr: 'ts', as: 'ts', kind: 'ts' })],
  }]);
  const dir = mkdtempSync(join(tmpdir(), 'gl-col-'));
  const src: SourceAdapter = {
    dialect: 'mysql', now: async () => '2024-03-20 12:00:00', abort: () => {},
    selectStream: async (_sql, onRow, onColumns) => {
      onColumns?.(['id', 'WRONG', 'ts']);
      onRow(['1', 'x', '2024-01-01 00:00:00']);
      return { columns: [], rows: 1, ms: 0 };
    },
  };
  await assert.rejects(collect({ specs, source: src, snapshotsDir: dir }), /header mismatch \(id,WRONG,ts ≠ id,name,ts\)/);
  assert.deepEqual(readdirSync(dir), []);
});


function demoWorkspace(): string {
  const dir = mkdtempSync(join(tmpdir(), 'gl-ws-'));
  for (const f of ['workspace.json', 'tables.json', 'derived.sql', 'derived-columns.json']) cpSync(join(DEMO, f), join(dir, f));
  cpSync(join(DEMO, 'tests'), join(dir, 'tests'), { recursive: true });
  seedDemo(join(dir, '.out', 'source.sqlite'), { anchor: ANCHOR });
  return dir;
}

class FixedNowSqlite extends SqliteSource {
  private readonly fixed: string;
  constructor(path: string, fixed: string) {
    super(path);
    this.fixed = fixed;
  }
  override async now() { return this.fixed; }
}

async function collectAt(wsDir: string, cutoff: string, failAt?: 'cutoff' | 'build' | 'pseudonymize' | 'rename' | 'rename-1' | 'rename-2') {
  const ws = loadWorkspace(wsDir);
  const inputs = loadBuildInputs(ws);
  const snaps = join(ws.config.outDir, 'snapshots');
  const { mkdirSync } = await import('node:fs');
  mkdirSync(snaps, { recursive: true });
  const c = await collect({ specs: inputs.specs, source: new FixedNowSqlite(join(wsDir, '.out', 'source.sqlite'), cutoff), snapshotsDir: snaps });
  return finalize({ tmpPath: c.tmpPath, snapshotsDir: snaps, inputs, meta: { cutoff: c.cutoff, startedAt: c.startedAt, finishedAt: c.finishedAt }, applyCutoffFirst: true, failAt });
}

test('collect → finalize: three files and current.json, cutoff log, same input gives the same hash', async () => {
  const ws = demoWorkspace();
  const r = await collectAt(ws, '2024-04-15 00:00:00');
  for (const p of Object.values(r.files)) assert.ok(existsSync(p), p);
  const cur = readCurrent(join(ws, '.out', 'snapshots'))!;
  assert.equal(cur.snapshot_id, r.snapshotId);
  assert.match(r.snapshotId, /^20240415-000000-[0-9a-f]{8}-[0-9a-f]{8}-[0-9a-f]{8}-[0-9a-f]{8}$/);

  const db = new DatabaseSync(r.files.real, { readOnly: true });
  const n = (sql: string) => (db.prepare(sql).get() as { n: number }).n;
  assert.equal(n("SELECT count(*) n FROM r_post WHERE created_at > '2024-04-15 00:00:00.000000'"), 0);
  assert.equal(n("SELECT count(*) n FROM r_member WHERE deleted_at > '2024-04-15 00:00:00.000000'"), 0);
  assert.ok(n("SELECT sum(dropped_after_cutoff) n FROM r_collect_log") > 0);
  assert.ok(n("SELECT sum(nulled_after_cutoff) n FROM r_collect_log") > 0);
  assert.equal(n("SELECT count(*) n FROM snapshot_params WHERE key = 'calendar_start'"), 1);
  assert.ok(n('SELECT count(*) n FROM d_calendar_week') > 0);
  assert.equal(n("SELECT count(*) n FROM d_calendar_week WHERE week_start >= '2024-04-15 00:00:00.000000'"), 0);
  db.close();

  const again = await collectAt(ws, '2024-04-15 00:00:00');
  assert.equal(again.snapshotId, r.snapshotId);
  assert.ok(again.reused);
});

test('pseudonymized copy: IDs change, aggregates and joins match, no mapping table inside', async () => {
  const r = await collectAt(demoWorkspace(), '2024-06-01 00:00:00');
  const real = new DatabaseSync(r.files.real, { readOnly: true });
  const agent = new DatabaseSync(r.files.agent, { readOnly: true });
  const same = (sql: string) => assert.deepEqual(agent.prepare(sql).all(), real.prepare(sql).all(), sql);
  same("SELECT count(*), sum(board_state = 'reached'), sum(connected_state = 'reached'), sum(received_state = 'reached') FROM d_member_first_week");
  same('SELECT count(DISTINCT member_id) FROM r_post');
  same('SELECT count(*) FROM r_post p JOIN r_member m ON m.id = p.member_id JOIN r_board b ON b.id = p.board_id');
  same('SELECT life_week, count(*) FROM d_member_activity_week GROUP BY 1 ORDER BY 1');
  const ids = (db: DatabaseSync) => JSON.stringify(db.prepare('SELECT id FROM r_member ORDER BY id LIMIT 50').all());
  assert.notEqual(ids(agent), ids(real));
  assert.equal((agent.prepare("SELECT count(*) n FROM sqlite_master WHERE name LIKE '%map%'").get() as { n: number }).n, 0);
  const map = new DatabaseSync(r.files.map, { readOnly: true });
  const m = map.prepare("SELECT count(*) n, count(DISTINCT pseudo) d, min(pseudo) lo, max(pseudo) hi, sum(pseudo IN (SELECT real FROM pseudo_map)) overlap FROM pseudo_map WHERE domain = 'member'").get() as Record<string, number>;
  assert.equal(m.n, m.d);
  assert.ok(m.lo >= PSEUDO_MIN && m.hi < PSEUDO_MAX, JSON.stringify(m));
  assert.equal(m.overlap, 0);
  const g = map.prepare('SELECT count(*) n, count(DISTINCT pseudo) d, sum(pseudo IN (SELECT real FROM pseudo_map)) overlap FROM pseudo_map').get() as Record<string, number>;
  assert.deepEqual([g.n, g.overlap], [g.d, 0]);
  for (const db of [real, agent, map]) db.close();
});

test('failure during finalize: current.json keeps the previous set and no temporary files remain', async () => {
  const ws = demoWorkspace();
  const first = await collectAt(ws, '2024-04-01 00:00:00');
  const snaps = join(ws, '.out', 'snapshots');
  const before = readdirSync(snaps).sort();
  for (const stage of ['cutoff', 'build', 'pseudonymize', 'rename', 'rename-1', 'rename-2'] as const) {
    await assert.rejects(collectAt(ws, '2024-05-01 00:00:00', stage), new RegExp(`\\(${stage}\\)`));
    assert.equal(readCurrent(snaps)!.snapshot_id, first.snapshotId, stage);
    assert.deepEqual(readdirSync(snaps).sort(), before, stage);
  }
});

test('a partial set with the same snapshot_id stops instead of overwriting', async () => {
  const ws = demoWorkspace();
  const r = await collectAt(ws, '2024-04-01 00:00:00');
  const { rmSync } = await import('node:fs');
  rmSync(r.files.map);
  await assert.rejects(collectAt(ws, '2024-04-01 00:00:00'), /manual check/);
});

test('derive: role-only changes give a new snapshot_id and copy; other spec changes are rejected', async () => {
  const wsDir = demoWorkspace();
  const first = await collectAt(wsDir, '2024-04-01 00:00:00');
  const ws = () => loadWorkspace(wsDir);
  assert.equal(runDerive(ws()).snapshotId, first.snapshotId);

  const rolesFile = join(wsDir, 'derived-columns.json');
  const roles = JSON.parse(readFileSync(rolesFile, 'utf8'));
  roles['d_activity.post_id'] = 'ordinary';
  writeFileSync(rolesFile, JSON.stringify(roles));
  const second = runDerive(ws());
  assert.notEqual(second.snapshotId, first.snapshotId);
  assert.equal(second.snapshotId.split('-').slice(0, 4).join('-'), first.snapshotId.split('-').slice(0, 4).join('-'));
  const agent = new DatabaseSync(second.files.agent, { readOnly: true });
  const real = new DatabaseSync(second.files.real, { readOnly: true });
  const q = 'SELECT max(post_id) m FROM d_activity';
  assert.deepEqual(agent.prepare(q).get(), real.prepare(q).get());
  agent.close();
  real.close();

  const specFile = join(wsDir, 'tables.json');
  const spec = JSON.parse(readFileSync(specFile, 'utf8'));
  spec[0].columns.pop();
  writeFileSync(specFile, JSON.stringify(spec));
  assert.throws(() => runDerive(ws()), /run collect/);
});

test('role declarations: missing or extra declarations block the pseudonymized copy', async () => {
  const wsDir = demoWorkspace();
  const rolesFile = join(wsDir, 'derived-columns.json');
  const roles = JSON.parse(readFileSync(rolesFile, 'utf8'));
  delete roles['d_member.country'];
  writeFileSync(rolesFile, JSON.stringify(roles));
  await assert.rejects(collectAt(wsDir, '2024-04-01 00:00:00'), /columns without a declared role: d_member\.country/);
  roles['d_member.country'] = 'ordinary';
  roles['d_member.nope'] = 'ordinary';
  writeFileSync(rolesFile, JSON.stringify(roles));
  await assert.rejects(collectAt(wsDir, '2024-04-01 00:00:00'), /declarations for missing columns: d_member\.nope/);
});

test('pseudonymize unit: no collisions with composite PKs, NULLs, same numbers across domains or swapped values', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-ps-'));
  const src = join(dir, 's.sqlite');
  const db = new DatabaseSync(src);
  db.exec(`CREATE TABLE r_a (id INTEGER PRIMARY KEY, other INTEGER);
           CREATE TABLE r_link (a_id INTEGER, b_id INTEGER, PRIMARY KEY (a_id, b_id));
           INSERT INTO r_a VALUES (1, 2), (2, 1), (3, NULL);
           INSERT INTO r_link VALUES (1, 1), (1, 2), (2, 1), (3, 3);`);
  db.close();
  const roles = new Map<string, import('../src/collect/spec.ts').Role>([
    ['r_a.id', { identifier: 'a' }], ['r_a.other', { identifier: 'a' }],
    ['r_link.a_id', { identifier: 'a' }], ['r_link.b_id', { identifier: 'b' }],
  ]);
  const stats = pseudonymize({ srcPath: src, agentPath: join(dir, 'a.sqlite'), mapPath: join(dir, 'm.sqlite'), roles });
  assert.deepEqual(stats.domains, { a: 3, b: 3 });
  const a = new DatabaseSync(join(dir, 'a.sqlite'), { readOnly: true });
  const pairs = a.prepare('SELECT x.id, x.other, y.other AS back FROM r_a x LEFT JOIN r_a y ON y.id = x.other ORDER BY x.id').all() as { id: number; other: number | null; back: number | null }[];
  assert.equal(pairs.filter((p) => p.other !== null && p.back === p.id).length, 2);
  assert.equal(pairs.filter((p) => p.other === null).length, 1);
  a.close();
  assert.throws(() => pseudonymize({ srcPath: src, agentPath: join(dir, 'a2.sqlite'), mapPath: join(dir, 'm2.sqlite'), roles: new Map([...roles].slice(1)) }), PseudonymizeError);
});

test('test-derived: demo cases pass; wrong expectations show the difference', () => {
  const wsDir = demoWorkspace();
  const ok = runTestDerived(loadWorkspace(wsDir));
  assert.deepEqual(ok.map((r) => [r.name, r.ok]), [['connected_window', true]]);
  const exp = join(wsDir, 'tests', 'connected_window', 'expected.json');
  writeFileSync(exp, readFileSync(exp, 'utf8').replace('"connected_state": "reached"', '"connected_state": "not"'));
  const bad = runTestDerived(loadWorkspace(wsDir));
  assert.equal(bad[0].ok, false);
  assert.match(bad[0].message!, /query 1 mismatch/);
});

test('pseudonymize guard: rejects tables other than r_*, d_* and engine tables', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-ps-'));
  const src = join(dir, 's.sqlite');
  const db = new DatabaseSync(src);
  db.exec('CREATE TABLE leak (id INTEGER); INSERT INTO leak VALUES (42);');
  db.close();
  assert.throws(() => pseudonymize({ srcPath: src, agentPath: join(dir, 'a.sqlite'), mapPath: join(dir, 'm.sqlite'), roles: new Map() }), /engine tables are allowed.*leak/);
});

test('derived SQL may only create and write d_*: rejects changes to raw or engine tables, temp tables, PRAGMA, ATTACH', async () => {
  const wsDir = demoWorkspace();
  const f = join(wsDir, 'derived.sql');
  const orig = readFileSync(f, 'utf8');
  for (const bad of [
    'ALTER TABLE snapshot_params ADD COLUMN member_id INTEGER;',
    "INSERT INTO snapshot_params SELECT 'leak', id FROM r_member;",
    'UPDATE r_collect_log SET rows = 0;',
    'DELETE FROM r_post;',
    'CREATE TABLE leak AS SELECT id FROM r_member;',
    'CREATE INDEX ix_leak ON r_member(country);',
    'CREATE TEMP TABLE d_tmp AS SELECT 1;',
    'PRAGMA writable_schema = 1;',
    "ATTACH DATABASE ':memory:' AS other;",
    'CREATE TRIGGER d_t AFTER INSERT ON d_member BEGIN SELECT 1; END;',
    'CREATE VIEW d_v AS SELECT 1 AS n;',
  ]) {
    writeFileSync(f, `${orig}\n${bad}\n`);
    await assert.rejects(collectAt(wsDir, '2024-04-01 00:00:00'), /derived\.sql failed.*(not authorized|prohibited)/i, bad);
  }
  writeFileSync(f, `${orig}\nCREATE TABLE d_scratch AS SELECT 1 AS n; UPDATE d_scratch SET n = 2; DROP TABLE d_scratch;\n`);
  await collectAt(wsDir, '2024-04-01 00:00:00');
});

test('pseudonymize guard: rejects changed engine table columns', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-ps-'));
  const src = join(dir, 's.sqlite');
  const db = new DatabaseSync(src);
  db.exec("CREATE TABLE snapshot_params (key TEXT, value TEXT, member_id INTEGER); INSERT INTO snapshot_params VALUES ('k', 'v', 42);");
  db.close();
  assert.throws(() => pseudonymize({ srcPath: src, agentPath: join(dir, 'a.sqlite'), mapPath: join(dir, 'm.sqlite'), roles: new Map() }), /columns of engine table snapshot_params changed/);
});

test('snapshot lock: a live or stale lock rejects and cleans up temporary files; removing it lets collect proceed', async () => {
  const wsDir = demoWorkspace();
  await collectAt(wsDir, '2024-04-01 00:00:00');
  const snaps = join(wsDir, '.out', 'snapshots');
  const before = readdirSync(snaps).sort();
  writeFileSync(join(snaps, '.lock'), String(process.pid));
  await assert.rejects(collectAt(wsDir, '2024-05-01 00:00:00'), /another collect or derive is running/);
  assert.deepEqual(readdirSync(snaps).sort(), [...before, '.lock'].sort());
  writeFileSync(join(snaps, '.lock'), '2147483646');
  await assert.rejects(collectAt(wsDir, '2024-05-01 00:00:00'), /stale lock left behind.*delete .* and run again/);
  assert.deepEqual(readdirSync(snaps).sort(), [...before, '.lock'].sort());
  const { rmSync } = await import('node:fs');
  rmSync(join(snaps, '.lock'));
  const r = await collectAt(wsDir, '2024-05-01 00:00:00');
  assert.ok(!r.reused);
  assert.ok(!existsSync(join(snaps, '.lock')));
});

test('runCollect: rejects output locations inside the repo that are not git-ignored', async () => {
  const { execFileSync } = await import('node:child_process');
  const repo = mkdtempSync(join(tmpdir(), 'gl-repo-'));
  execFileSync('git', ['init', '-q'], { cwd: repo });
  const wsDir = join(repo, 'ws');
  cpSync(demoWorkspace(), wsDir, { recursive: true });
  await assert.rejects(runCollect(loadWorkspace(wsDir)), /output paths are not git-ignored/);
  writeFileSync(join(repo, '.gitignore'), 'ws/.out/\n');
  await assert.rejects(runCollect(loadWorkspace(wsDir)), /output paths are not git-ignored: ws(,|$| )/);
  writeFileSync(join(repo, '.gitignore'), 'ws/\n');
  const r = await runCollect(loadWorkspace(wsDir));
  assert.ok(existsSync(r.files.real));
});

test('derived-columns.json format errors', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-dc-'));
  for (const bad of [{ 'r_x.a': 'ordinary' }, { 'd_x.a': 'secret' }, { 'd_x.a': { identifier: 1 } }]) {
    writeFileSync(join(dir, 'r.json'), JSON.stringify(bad));
    assert.throws(() => loadDerivedRoles(join(dir, 'r.json')));
  }
});
