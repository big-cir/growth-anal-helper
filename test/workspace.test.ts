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
  source: { type: 'sqlite', path: 'source.sqlite' },
  policy: { readablePrefixes: ['r_', 'd_', 'snapshot_'] },
});

test('findWorkspaceDir: 환경변수 우선, 없으면 ./workspace', () => {
  assert.equal(findWorkspaceDir({ GROWTH_LAB_WORKSPACE: 'examples/demo' }, '/repo'), '/repo/examples/demo');
  assert.equal(findWorkspaceDir({}, '/repo'), '/repo/workspace');
});

test('기본값 채우기', () => {
  const c = parseWorkspaceConfig(base(), '/ws', HOME);
  assert.deepEqual(c.source, { type: 'sqlite', path: '/ws/source.sqlite' });
  assert.equal(c.agent.dataMode, 'pseudonymized');
  assert.equal(c.agent.maxTurns, 8);
  assert.equal(c.run.heapLimitMb, 2048);
  assert.equal(c.server.port, 4170);
  assert.deepEqual(c.params, {});
  assert.equal(c.outDir, '/ws');
  assert.equal(parseWorkspaceConfig({ ...base(), outDir: '.out' }, '/ws', HOME).outDir, '/ws/.out');
});

test('command 소스: 홈 경로 펼치기, 기본값', () => {
  const c = parseWorkspaceConfig({
    ...base(),
    source: { type: 'command', dialect: 'mysql', localCommand: ['mysql', '--defaults-extra-file=~/.board.cnf', '~/bin/x', '--batch', 'board'] },
  }, '/ws', HOME);
  assert.equal(c.source.type, 'command');
  if (c.source.type !== 'command') return;
  assert.deepEqual(c.source.localCommand, ['mysql', `--defaults-extra-file=${join(HOME, '.board.cnf')}`, join(HOME, 'bin/x'), '--batch', 'board']);
  assert.deepEqual(c.source.preamble, []);
  assert.equal(c.source.nowQuery, null);
  assert.equal(expandHome('a~/b', HOME), 'a~/b');
});

test('비밀값 키는 어디에 있든 거부', () => {
  for (const extra of [
    { source: { type: 'command', dialect: 'mysql', localCommand: ['mysql'], password: 'x' } },
    { agent: { apiKey: 'x' } },
    { params: { secret_value: 'x' } },
    { server: { token: 'x' } },
  ]) {
    assert.throws(() => parseWorkspaceConfig({ ...base(), ...extra }, '/ws', HOME), ConfigError);
  }
});

test('형식 오류 거부', () => {
  const cases: unknown[] = [
    { ...base(), name: '' },
    { ...base(), source: { type: 'ssh' } },
    { ...base(), source: { type: 'command', dialect: 'postgres', localCommand: ['x'] } },
    { ...base(), source: { type: 'command', dialect: 'mysql', localCommand: [] } },
    { ...base(), source: { type: 'command', dialect: 'mysql', localCommand: ['x'], ignoreStderrPattern: '(' } },
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

test('params: 수·시각·"시각~시각"과 그 배열만, 자유 문자열은 거부', () => {
  const ok = { start: '2024-01-01 00:00:00.000000', n: 3, marks: ['2024-03-02 10:00:00.000000'], gap: '2022-01-01~2022-02-01' };
  assert.deepEqual(parseWorkspaceConfig({ ...base(), params: ok }, '/ws', HOME).params, ok);
  for (const bad of [{ host: 'db.example.com' }, { marks: ['a', 'b'] }, { u: 'user-at-host' }, { g: '2024-01-01~2024-02-01~2024-03-01' }]) {
    assert.throws(() => parseWorkspaceConfig({ ...base(), params: bad }, '/ws', HOME), /자유 문자열 금지|시각/, JSON.stringify(bad));
  }
});

test('loadWorkspace: 파일 없음·JSON 오류', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-ws-'));
  assert.throws(() => loadWorkspace(dir), /설정이 없음/);
  writeFileSync(join(dir, 'workspace.json'), '{');
  assert.throws(() => loadWorkspace(dir), /읽지 못함/);
  writeFileSync(join(dir, 'workspace.json'), JSON.stringify(base()));
  assert.equal(loadWorkspace(dir).config.name, 'demo');
});

test('산출물 경로 검사: 저장소 안에서 제외되지 않은 경로를 찾는다', () => {
  const repo = mkdtempSync(join(tmpdir(), 'gl-repo-'));
  execFileSync('git', ['init', '-q'], { cwd: repo });
  writeFileSync(join(repo, '.gitignore'), 'workspace/\n');
  mkdirSync(join(repo, 'workspace'));
  assert.deepEqual(findUnignoredOutputs(outputPaths(join(repo, 'workspace')), repo), []);
  const bad = findUnignoredOutputs(outputPaths(join(repo, 'elsewhere')), repo);
  assert.equal(bad.length, 7);
  writeFileSync(join(repo, '.gitignore'), 'workspace/\nexamples/demo/.out/\n');
  assert.deepEqual(findUnignoredOutputs(outputPaths(join(repo, 'examples/demo'), join(repo, 'examples/demo/.out')), repo), []);
  assert.equal(findUnignoredOutputs(outputPaths(join(repo, 'examples/demo')), repo).length, 7);
  assert.deepEqual(findUnignoredOutputs([repo], repo), ['.']);
  assert.deepEqual(findUnignoredOutputs(outputPaths(mkdtempSync(join(tmpdir(), 'gl-out-'))), repo), []);
});
