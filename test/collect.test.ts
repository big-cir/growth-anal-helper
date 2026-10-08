// 수집·확정·가명 사본·derive·test-derived.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { DatabaseSync } from 'node:sqlite';
import { cpSync, existsSync, mkdtempSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { parseSpec, selectSql, specHash, SpecError } from '../src/collect/spec.ts';
import { parseLine, unescapeField } from '../src/collect/sources/tsv.ts';
import { CommandSourceAdapter } from '../src/collect/sources/command.ts';
import { SqliteSource } from '../src/collect/sources/sqlite.ts';
import { collect } from '../src/collect/collector.ts';
import { finalize, loadDerivedRoles } from '../src/collect/finalize.ts';
import { pseudonymize, PseudonymizeError, PSEUDO_MAX, PSEUDO_MIN } from '../src/snapshot/pseudonymize.ts';
import { readCurrent } from '../src/snapshot/store.ts';
import { loadBuildInputs, runCollect, runDerive, runTestDerived } from '../src/snapshot/pipeline.ts';
import { loadWorkspace, type CommandSource } from '../src/workspace.ts';
import { seedDemo } from '../examples/demo/seed.ts';

const ROOT = join(import.meta.dirname, '..');
const DEMO = join(ROOT, 'examples', 'demo');
const FAKE = join(import.meta.dirname, 'fixtures', 'fake-mysql.ts');
const ANCHOR = '2024-06-03 12:00:00';

const col = (o: Record<string, unknown>) => ({ kind: 'int', role: 'ordinary', ...o });
const tableSpec = (cols: unknown[], extra: Record<string, unknown> = {}) => [{
  source: 'thing', target: 'r_thing', key: ['id'], cutoffColumn: 'created_at',
  columns: [col({ expr: 'id', as: 'id', role: { identifier: 'thing' } }), col({ expr: 'created_at', as: 'created_at', kind: 'ts' }), ...cols],
  ...extra,
}];


test('명세: 데모 tables.json은 통과', () => {
  const specs = parseSpec(JSON.parse(readFileSync(join(DEMO, 'tables.json'), 'utf8')));
  assert.equal(specs.length, 6);
  const reply = specs.find((t) => t.target === 'r_reply')!;
  assert.equal(selectSql(reply, false), 'SELECT id AS id, post_id AS post_id, member_id AS member_id, parent_reply_id IS NOT NULL AS is_nested, created_at AS created_at, deleted_at AS deleted_at FROM reply ORDER BY id');
  const member = specs.find((t) => t.target === 'r_member')!;
  assert.match(selectSql(member, true), /CONCAT\('s', country\) AS country/);
});

test('명세: 규칙 위반은 모두 거부', () => {
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

test('명세 해시는 역할을 빼고 계산한다', () => {
  const a = parseSpec(tableSpec([col({ expr: 'n', as: 'n' })]));
  const b = parseSpec(tableSpec([col({ expr: 'n', as: 'n', role: { identifier: 'other' } })]));
  const c = parseSpec(tableSpec([col({ expr: 'm', as: 'n' })]));
  assert.equal(specHash(a), specHash(b));
  assert.notEqual(specHash(a), specHash(c));
});


test('배치 형식: 이스케이프 되돌리기, NULL', () => {
  assert.equal(unescapeField('a\\tb\\nc\\\\d\\0e'), 'a\tb\nc\\d\0e');
  assert.throws(() => unescapeField('a\\q'));
  assert.deepEqual(parseLine('1\tNULL\tsNULL\t'), ['1', null, 'sNULL', '']);
});

function fakeSource(scenario: string, sqlLog?: string): CommandSourceAdapter {
  const cfg: CommandSource = {
    type: 'command', dialect: 'mysql',
    localCommand: [process.execPath, FAKE, scenario, ...(sqlLog ? [sqlLog] : [])],
    preamble: ['SET NAMES utf8mb4;'], nowQuery: null, ignoreStderrPattern: 'Using a password',
  };
  const logged: string[] = [];
  const a = new CommandSourceAdapter(cfg, (l) => logged.push(l));
  (a as unknown as { logged: string[] }).logged = logged;
  return a;
}

test('명령 소스: SQL은 stdin으로(앞줄 포함), 값 이스케이프, stderr 거르기', async () => {
  const sqlLog = join(mkdtempSync(join(tmpdir(), 'gl-cmd-')), 'sql.txt');
  const src = fakeSource('rows', sqlLog);
  const rows: (string | null)[][] = [];
  const r = await src.selectStream('SELECT 1', (v) => rows.push(v));
  assert.deepEqual(r.columns, ['id', 'name', 'note']);
  assert.deepEqual(rows, [['1', 'sAlice', 'sNULL'], ['2', 'sTab\there', null], ['3', 'sLine\nBreak\\x', 'sok']]);
  assert.equal(readFileSync(sqlLog, 'utf8'), 'SET NAMES utf8mb4;\nSELECT 1;\n');
  assert.deepEqual((src as unknown as { logged: string[] }).logged, ['real diagnostic line']);
});

test('명령 소스: 종료 코드 실패, 열 개수 불일치', async () => {
  await assert.rejects(fakeSource('fail').selectStream('x', () => {}), /종료 코드 1/);
  await assert.rejects(fakeSource('shortrow').selectStream('x', () => {}), /열 개수 불일치/);
  assert.equal(await fakeSource('now').now(), '2024-03-20 12:00:00.123456');
});

test('수집: 헤더가 명세의 as 목록과 다르면 첫 행 전에 멈추고 임시 파일을 남기지 않는다', async () => {
  const specs = parseSpec([{
    source: 't', target: 'r_t', key: ['id'], cutoffColumn: 'ts',
    columns: [col({ expr: 'id', as: 'id' }), col({ expr: 'name', as: 'name', kind: 'text' }), col({ expr: 'ts', as: 'ts', kind: 'ts' })],
  }]);
  const dir = mkdtempSync(join(tmpdir(), 'gl-col-'));
  const src = Object.assign(fakeSource('badheader'), { now: async () => '2024-03-20 12:00:00' });
  await assert.rejects(collect({ specs, source: src, snapshotsDir: dir }), /헤더 불일치 \(id,WRONG,ts ≠ id,name,ts\)/);
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

test('수집 → 확정: 세 파일과 current.json, 기준 시각 정리 기록, 같은 입력이면 같은 해시', async () => {
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

test('가명 사본: ID는 바뀌고, 집계·조인은 같고, 순열 표는 사본 안에 없다', async () => {
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

test('확정 도중 실패: current.json은 이전 세트를 가리키고, 임시 파일이 남지 않는다', async () => {
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

test('같은 snapshot_id 세트의 일부가 없으면 덮어쓰지 않고 멈춘다', async () => {
  const ws = demoWorkspace();
  const r = await collectAt(ws, '2024-04-01 00:00:00');
  const { rmSync } = await import('node:fs');
  rmSync(r.files.map);
  await assert.rejects(collectAt(ws, '2024-04-01 00:00:00'), /사람이 확인/);
});

test('derive: 역할만 바꾸면 새 snapshot_id와 새 가명 사본, 역할 외 명세 변경은 거부', async () => {
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
  assert.throws(() => runDerive(ws()), /collect를 실행/);
});

test('역할 선언: 누락·없는 칸 선언이면 가명 사본을 만들지 않는다', async () => {
  const wsDir = demoWorkspace();
  const rolesFile = join(wsDir, 'derived-columns.json');
  const roles = JSON.parse(readFileSync(rolesFile, 'utf8'));
  delete roles['d_member.country'];
  writeFileSync(rolesFile, JSON.stringify(roles));
  await assert.rejects(collectAt(wsDir, '2024-04-01 00:00:00'), /역할 선언이 없는 칸: d_member\.country/);
  roles['d_member.country'] = 'ordinary';
  roles['d_member.nope'] = 'ordinary';
  writeFileSync(rolesFile, JSON.stringify(roles));
  await assert.rejects(collectAt(wsDir, '2024-04-01 00:00:00'), /없는 칸을 가리키는 선언: d_member\.nope/);
});

test('가명화 단위: 복합 PK·NULL·도메인이 다른 같은 숫자·값 교환에서도 충돌 없음', () => {
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

test('test-derived: 데모 케이스 통과, 틀린 기대값은 차이를 보여 준다', () => {
  const wsDir = demoWorkspace();
  const ok = runTestDerived(loadWorkspace(wsDir));
  assert.deepEqual(ok.map((r) => [r.name, r.ok]), [['connected_window', true]]);
  const exp = join(wsDir, 'tests', 'connected_window', 'expected.json');
  writeFileSync(exp, readFileSync(exp, 'utf8').replace('"connected_state": "reached"', '"connected_state": "not"'));
  const bad = runTestDerived(loadWorkspace(wsDir));
  assert.equal(bad[0].ok, false);
  assert.match(bad[0].message!, /질의 1 불일치/);
});

test('가명화 단계 방어: r_*·d_*·엔진 표가 아닌 테이블이 있으면 거부', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-ps-'));
  const src = join(dir, 's.sqlite');
  const db = new DatabaseSync(src);
  db.exec('CREATE TABLE leak (id INTEGER); INSERT INTO leak VALUES (42);');
  db.close();
  assert.throws(() => pseudonymize({ srcPath: src, agentPath: join(dir, 'a.sqlite'), mapPath: join(dir, 'm.sqlite'), roles: new Map() }), /엔진 표가 아닌 테이블.*leak/);
});

test('파생 SQL은 d_* 만 만들고 쓸 수 있다: 원본·엔진 표 변경, 임시 표, PRAGMA, ATTACH는 거부', async () => {
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
    await assert.rejects(collectAt(wsDir, '2024-04-01 00:00:00'), /derived\.sql 실행 실패.*(not authorized|prohibited)/i, bad);
  }
  writeFileSync(f, `${orig}\nCREATE TABLE d_scratch AS SELECT 1 AS n; UPDATE d_scratch SET n = 2; DROP TABLE d_scratch;\n`);
  await collectAt(wsDir, '2024-04-01 00:00:00');
});

test('가명화 단계 방어: 엔진 표 칸 구성이 바뀌었으면 거부', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-ps-'));
  const src = join(dir, 's.sqlite');
  const db = new DatabaseSync(src);
  db.exec("CREATE TABLE snapshot_params (key TEXT, value TEXT, member_id INTEGER); INSERT INTO snapshot_params VALUES ('k', 'v', 42);");
  db.close();
  assert.throws(() => pseudonymize({ srcPath: src, agentPath: join(dir, 'a.sqlite'), mapPath: join(dir, 'm.sqlite'), roles: new Map() }), /엔진 표 snapshot_params의 칸 구성이 바뀜/);
});

test('스냅샷 잠금: 잠금이 있으면(살아 있든 낡았든) 거부하고 임시 파일을 정리, 잠금을 지우면 진행', async () => {
  const wsDir = demoWorkspace();
  await collectAt(wsDir, '2024-04-01 00:00:00');
  const snaps = join(wsDir, '.out', 'snapshots');
  const before = readdirSync(snaps).sort();
  writeFileSync(join(snaps, '.lock'), String(process.pid));
  await assert.rejects(collectAt(wsDir, '2024-05-01 00:00:00'), /다른 수집·파생이 진행 중/);
  assert.deepEqual(readdirSync(snaps).sort(), [...before, '.lock'].sort());
  writeFileSync(join(snaps, '.lock'), '2147483646');
  await assert.rejects(collectAt(wsDir, '2024-05-01 00:00:00'), /낡은 잠금이 남아 있음.*지우고 다시 실행/);
  assert.deepEqual(readdirSync(snaps).sort(), [...before, '.lock'].sort());
  const { rmSync } = await import('node:fs');
  rmSync(join(snaps, '.lock'));
  const r = await collectAt(wsDir, '2024-05-01 00:00:00');
  assert.ok(!r.reused);
  assert.ok(!existsSync(join(snaps, '.lock')));
});

test('runCollect: 저장소 안에서 git 제외되지 않은 산출물 위치면 거부', async () => {
  const { execFileSync } = await import('node:child_process');
  const repo = mkdtempSync(join(tmpdir(), 'gl-repo-'));
  execFileSync('git', ['init', '-q'], { cwd: repo });
  const wsDir = join(repo, 'ws');
  cpSync(demoWorkspace(), wsDir, { recursive: true });
  await assert.rejects(runCollect(loadWorkspace(wsDir)), /git에서 제외되지 않은 산출물 경로/);
  writeFileSync(join(repo, '.gitignore'), 'ws/.out/\n');
  await assert.rejects(runCollect(loadWorkspace(wsDir)), /git에서 제외되지 않은 산출물 경로: ws(,|$| )/);
  writeFileSync(join(repo, '.gitignore'), 'ws/\n');
  const r = await runCollect(loadWorkspace(wsDir));
  assert.ok(existsSync(r.files.real));
});

test('derived-columns.json 형식 오류', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-dc-'));
  for (const bad of [{ 'r_x.a': 'ordinary' }, { 'd_x.a': 'secret' }, { 'd_x.a': { identifier: 1 } }]) {
    writeFileSync(join(dir, 'r.json'), JSON.stringify(bad));
    assert.throws(() => loadDerivedRoles(join(dir, 'r.json')));
  }
});
