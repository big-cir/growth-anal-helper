import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  ConfigError, expandHome, findUnignoredOutputs, findWorkspaceDir, loadWorkspace, outputPaths, parseWorkspaceConfig,
} from '../src/workspace.ts';

const HOME = join(tmpdir(), 'fake-home');

const base = () => ({
  name: 'demo',
  datasource: { host: 'sqlite://source.sqlite' },
  policy: { readablePrefixes: ['r_', 'd_', 'snapshot_'] },
});

test('findWorkspaceDir: env var first, then ./workspace', () => {
  assert.equal(findWorkspaceDir({ GROWTH_LAB_WORKSPACE: 'examples/demo' }, '/repo'), '/repo/examples/demo');
  assert.equal(findWorkspaceDir({}, '/repo'), '/repo/workspace');
});

test('fills defaults', () => {
  const c = parseWorkspaceConfig(base(), '/ws', HOME);
  assert.deepEqual(c.datasource, { kind: 'sqlite', path: '/ws/source.sqlite' });
  assert.equal(c.agent.dataMode, 'pseudonymized');
  assert.equal(c.agent.maxTurns, 8);
  assert.equal(c.run.heapLimitMb, 2048);
  assert.equal(c.server.port, 4170);
  assert.deepEqual(c.params, {});
  assert.equal(c.outDir, '/ws');
  assert.equal(parseWorkspaceConfig({ ...base(), outDir: '.out' }, '/ws', HOME).outDir, '/ws/.out');
});

test('datasource: database type and port in host, default ports, password kept as is', () => {
  const ds = (o: Record<string, unknown>) => parseWorkspaceConfig({ ...base(), datasource: o }, '/ws', HOME).datasource;
  assert.deepEqual(ds({ host: 'mysql://127.0.0.1:3310', user: 'ro', password: 'p@ss:w/rd', database: 'board' }),
    { kind: 'mysql', host: '127.0.0.1', port: 3310, user: 'ro', password: 'p@ss:w/rd', database: 'board' });
  assert.deepEqual(ds({ host: 'postgres://db.internal', user: 'ro', database: 'app' }),
    { kind: 'postgres', host: 'db.internal', port: 5432, user: 'ro', password: '', database: 'app' });
  assert.equal((ds({ host: 'postgresql://[::1]', user: 'ro', database: 'app' }) as { host: string }).host, '::1');
  assert.deepEqual(ds({ host: 'sqlite://~/data/x.sqlite' }), { kind: 'sqlite', path: join(HOME, 'data/x.sqlite') });
  assert.deepEqual(ds({ host: 'sqlite:///abs/x.sqlite' }), { kind: 'sqlite', path: '/abs/x.sqlite' });
  assert.equal(expandHome('a~/b', HOME), 'a~/b');
});

test('ga4: connection lives in workspace.json; key_file is expanded from home or the workspace', () => {
  const ga4 = (key_file: string) => parseWorkspaceConfig({ ...base(), ga4: { property_id: '123', time_zone: 'Asia/Seoul', key_file } }, '/ws', HOME).ga4;
  assert.deepEqual(ga4('~/k.json'), { propertyId: '123', timeZone: 'Asia/Seoul', keyFile: join(HOME, 'k.json') });
  assert.equal(ga4('keys/k.json')!.keyFile, '/ws/keys/k.json');
  for (const bad of [{ property_id: 'x', time_zone: 'Asia/Seoul', key_file: 'k' }, { property_id: '1', time_zone: 'Mars/Base', key_file: 'k' }, { property_id: '1', time_zone: 'UTC' }, { property_id: '1', time_zone: 'UTC', key_file: 'k', extra: 1 }]) {
    assert.throws(() => parseWorkspaceConfig({ ...base(), ga4: bad }, '/ws', HOME), /workspace.json \.ga4/, JSON.stringify(bad));
  }
});

test('rejects invalid config', () => {
  const cases: unknown[] = [
    { ...base(), name: '' },
    { ...base(), datasource: undefined },
    { ...base(), datasource: { host: 'oracle://h', user: 'u', database: 'd' } },
    { ...base(), datasource: { host: '127.0.0.1:3306', user: 'u', database: 'd' } },
    { ...base(), datasource: { host: 'mysql://h?ssl=1', user: 'u', database: 'd' } },
    { ...base(), datasource: { host: 'mysql://h/db', user: 'u', database: 'd' } },
    { ...base(), datasource: { host: 'mysql://h:99999', user: 'u', database: 'd' } },
    { ...base(), datasource: { host: 'mysql://h', database: 'd' } },
    { ...base(), datasource: { host: 'mysql://h', user: 'u' } },
    { ...base(), datasource: { host: 'mysql://h', user: 'u', database: 'd', password: 1 } },
    { ...base(), datasource: { host: 'mysql://h', user: 'u', database: 'd', port: 1 } },
    { ...base(), datasource: { host: 'sqlite://x.sqlite', user: 'u' } },
    { ...base(), policy: { readablePrefixes: [] } },
    { ...base(), policy: { readablePrefixes: ['r_; DROP'] } },
    { ...base(), policy: { readablePrefixes: ['sqlite_'] } },
    { ...base(), params: { as_of: 'x' } },
    { ...base(), params: { 'bad-key': 'x' } },
    { ...base(), params: { list: [1, 2] } },
    { ...base(), agent: { dataMode: 'raw' } },
    { ...base(), agent: { maxTurns: 0 } },
    { ...base(), agent: { unknown: 1 } },
    { ...base(), run: { heapLimitMb: 1.5 } },
    { ...base(), extra: true },
    { ...base(), outDir: '/abs/out' },
    { ...base(), outDir: '~/out' },
    { ...base(), outDir: '../out' },
    { ...base(), outDir: '.' },
  ];
  for (const c of cases) assert.throws(() => parseWorkspaceConfig(c, '/ws', HOME), ConfigError, JSON.stringify(c));
});

test('params: numbers, timestamps, "timestamp~timestamp" and arrays of those only; free text is rejected', () => {
  const ok = { start: '2024-01-01 00:00:00.000000', n: 3, marks: ['2024-03-02 10:00:00.000000'], gap: '2022-01-01~2022-02-01' };
  assert.deepEqual(parseWorkspaceConfig({ ...base(), params: ok }, '/ws', HOME).params, ok);
  for (const bad of [{ host: 'db.example.com' }, { marks: ['a', 'b'] }, { u: 'user-at-host' }, { g: '2024-01-01~2024-02-01~2024-03-01' }]) {
    assert.throws(() => parseWorkspaceConfig({ ...base(), params: bad }, '/ws', HOME), /no free text|timestamp/, JSON.stringify(bad));
  }
});

test('loadWorkspace: missing file, invalid JSON', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-ws-'));
  assert.throws(() => loadWorkspace(dir), /config not found/);
  writeFileSync(join(dir, 'workspace.json'), '{');
  assert.throws(() => loadWorkspace(dir), /cannot read/);
  writeFileSync(join(dir, 'workspace.json'), JSON.stringify(base()));
  assert.equal(loadWorkspace(dir).config.name, 'demo');
});

test('output path check: finds paths inside the repo that are not git-ignored', () => {
  const repo = mkdtempSync(join(tmpdir(), 'gl-repo-'));
  execFileSync('git', ['init', '-q'], { cwd: repo });
  writeFileSync(join(repo, '.gitignore'), 'workspace/\n');
  mkdirSync(join(repo, 'workspace'));
  assert.deepEqual(findUnignoredOutputs(outputPaths(join(repo, 'workspace')), repo), []);
  const bad = findUnignoredOutputs(outputPaths(join(repo, 'elsewhere')), repo);
  assert.equal(bad.length, 8);
  writeFileSync(join(repo, '.gitignore'), 'workspace/\nexamples/demo/.out/\n');
  assert.deepEqual(findUnignoredOutputs(outputPaths(join(repo, 'examples/demo'), join(repo, 'examples/demo/.out')), repo), []);
  assert.equal(findUnignoredOutputs(outputPaths(join(repo, 'examples/demo')), repo).length, 8);
  assert.deepEqual(findUnignoredOutputs([repo], repo), ['.']);
  assert.deepEqual(findUnignoredOutputs(outputPaths(mkdtempSync(join(tmpdir(), 'gl-out-'))), repo), []);
});
