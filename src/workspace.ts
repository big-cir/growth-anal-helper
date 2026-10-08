// 워크스페이스 위치와 workspace.json 검증.
import { readFileSync, existsSync, realpathSync } from 'node:fs';
import { homedir } from 'node:os';
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import { execFileSync } from 'node:child_process';
import { normalizeTs } from './time.ts';

export type CommandSource = {
  type: 'command';
  dialect: 'mysql';
  localCommand: string[];
  preamble: string[];
  nowQuery: string | null;
  ignoreStderrPattern: string | null;
};
export type SqliteSource = { type: 'sqlite'; path: string };

export type AgentConfig = {
  bin: string;
  model: string | null;
  callBudgetUsd: number;
  requestBudgetUsd: number;
  maxTurns: number;
  maxProbes: number;
  maxFixes: number;
  callTimeoutMs: number;
  concurrency: number;
  dataMode: 'pseudonymized' | 'schema_only';
};

export type WorkspaceConfig = {
  name: string;
  source: CommandSource | SqliteSource;
  /** panelReadablePrefixes: 패널 SQL이 읽을 수 있는 표(탐색은 readablePrefixes) */
  policy: { readablePrefixes: string[]; panelReadablePrefixes: string[] };
  params: Record<string, string | number | string[]>;
  agent: AgentConfig;
  run: { heapLimitMb: number };
  /** publicOrigin이 있으면 외부 운영 모드(HTTPS 프록시 뒤) */
  server: { port: number; publicOrigin: string | null; proxyHops: number; auditRetentionDays: number };
  /** 산출물 위치. 기본은 워크스페이스 자체 */
  outDir: string;
};

export type Workspace = { dir: string; config: WorkspaceConfig };

const AGENT_DEFAULTS: AgentConfig = {
  bin: 'claude',
  model: null,
  callBudgetUsd: 0.5,
  requestBudgetUsd: 1.0,
  maxTurns: 8,
  maxProbes: 4,
  maxFixes: 2,
  callTimeoutMs: 90_000,
  concurrency: 2,
  dataMode: 'pseudonymized',
};

/** 워크스페이스 위치: 환경변수 GROWTH_LAB_WORKSPACE → ./workspace */
export function findWorkspaceDir(env: NodeJS.ProcessEnv = process.env, cwd = process.cwd()): string {
  const fromEnv = env.GROWTH_LAB_WORKSPACE;
  return fromEnv ? resolve(cwd, fromEnv) : resolve(cwd, 'workspace');
}

export class ConfigError extends Error {}

const SECRET_KEY_RE = /pass(word|wd)?|secret|token|api[_-]?key|credential/i;
const IDENT_PREFIX_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** 시각 또는 "시각~시각"만 */
function paramValue(v: string, path: string): string {
  const parts = v.split('~').map((x) => x.trim());
  if (parts.length > 2) fail(path, '수, 시각, "시각~시각"만 쓸 수 있음');
  for (const p of parts) {
    try {
      normalizeTs(p);
    } catch {
      fail(path, '수, 시각, "시각~시각"만 쓸 수 있음(자유 문자열 금지)');
    }
  }
  return v;
}

function fail(path: string, msg: string): never {
  throw new ConfigError(`workspace.json ${path}: ${msg}`);
}

function rejectSecretKeys(value: unknown, path: string): void {
  if (Array.isArray(value)) {
    value.forEach((v, i) => rejectSecretKeys(v, `${path}[${i}]`));
  } else if (value && typeof value === 'object') {
    for (const [k, v] of Object.entries(value)) {
      if (SECRET_KEY_RE.test(k)) fail(`${path}.${k}`, '비밀값 키는 설정에 둘 수 없음 (mysql 옵션 파일을 쓰세요)');
      rejectSecretKeys(v, `${path}.${k}`);
    }
  }
}

function obj(v: unknown, path: string): Record<string, unknown> {
  if (!v || typeof v !== 'object' || Array.isArray(v)) fail(path, '객체여야 함');
  return v as Record<string, unknown>;
}
function str(v: unknown, path: string): string {
  if (typeof v !== 'string' || v.length === 0) fail(path, '비어 있지 않은 문자열이어야 함');
  return v;
}
function optStr(v: unknown, path: string): string | null {
  return v === undefined || v === null ? null : str(v, path);
}
function strArray(v: unknown, path: string, { nonEmpty = false } = {}): string[] {
  if (!Array.isArray(v) || v.some((x) => typeof x !== 'string')) fail(path, '문자열 배열이어야 함');
  if (nonEmpty && v.length === 0) fail(path, '비어 있으면 안 됨');
  return v as string[];
}
function num(v: unknown, path: string, { int = false, min = 0, exclusiveMin = false } = {}): number {
  if (typeof v !== 'number' || !Number.isFinite(v)) fail(path, '숫자여야 함');
  if (int && !Number.isInteger(v)) fail(path, '정수여야 함');
  if (exclusiveMin ? v <= min : v < min) fail(path, `${exclusiveMin ? '>' : '>='} ${min} 이어야 함`);
  return v;
}
function onlyKeys(o: Record<string, unknown>, allowed: string[], path: string): void {
  for (const k of Object.keys(o)) if (!allowed.includes(k)) fail(`${path}.${k}`, '알 수 없는 키');
}

/** argv 원소 앞의 `~/`, `=~/`를 홈 경로로 펼친다 */
export function expandHome(arg: string, home = homedir()): string {
  if (arg.startsWith('~/')) return join(home, arg.slice(2));
  const i = arg.indexOf('=~/');
  if (i >= 0) return `${arg.slice(0, i + 1)}${join(home, arg.slice(i + 3))}`;
  return arg;
}

export function parseWorkspaceConfig(raw: unknown, dir: string, home = homedir()): WorkspaceConfig {
  rejectSecretKeys(raw, '');
  const root = obj(raw, '(root)');
  onlyKeys(root, ['name', 'source', 'policy', 'params', 'agent', 'run', 'server', 'outDir'], '');

  const src = obj(root.source, '.source');
  let source: CommandSource | SqliteSource;
  if (src.type === 'command') {
    onlyKeys(src, ['type', 'dialect', 'localCommand', 'preamble', 'nowQuery', 'ignoreStderrPattern'], '.source');
    if (src.dialect !== 'mysql') fail('.source.dialect', '"mysql"만 지원');
    const pattern = optStr(src.ignoreStderrPattern, '.source.ignoreStderrPattern');
    if (pattern !== null) {
      try { new RegExp(pattern); } catch { fail('.source.ignoreStderrPattern', '정규식이 아님'); }
    }
    source = {
      type: 'command',
      dialect: 'mysql',
      localCommand: strArray(src.localCommand, '.source.localCommand', { nonEmpty: true }).map((a) => expandHome(a, home)),
      preamble: src.preamble === undefined ? [] : strArray(src.preamble, '.source.preamble'),
      nowQuery: optStr(src.nowQuery, '.source.nowQuery'),
      ignoreStderrPattern: pattern,
    };
  } else if (src.type === 'sqlite') {
    onlyKeys(src, ['type', 'path'], '.source');
    const p = expandHome(str(src.path, '.source.path'), home);
    source = { type: 'sqlite', path: isAbsolute(p) ? p : resolve(dir, p) };
  } else {
    fail('.source.type', '"command" 또는 "sqlite"');
  }

  const pol = obj(root.policy, '.policy');
  onlyKeys(pol, ['readablePrefixes', 'panelReadablePrefixes'], '.policy');
  const readablePrefixes = strArray(pol.readablePrefixes, '.policy.readablePrefixes', { nonEmpty: true });
  for (const p of readablePrefixes) {
    if (!IDENT_PREFIX_RE.test(p)) fail('.policy.readablePrefixes', `식별자 접두사가 아님: ${JSON.stringify(p)}`);
    if (p.toLowerCase().startsWith('sqlite_')) fail('.policy.readablePrefixes', 'sqlite_ 내부 테이블은 허용할 수 없음');
  }
  const panelReadablePrefixes = pol.panelReadablePrefixes === undefined ? ['d_'] : strArray(pol.panelReadablePrefixes, '.policy.panelReadablePrefixes', { nonEmpty: true });
  for (const p of panelReadablePrefixes) {
    if (!IDENT_PREFIX_RE.test(p)) fail('.policy.panelReadablePrefixes', `식별자 접두사가 아님: ${JSON.stringify(p)}`);
    if (!readablePrefixes.some((r) => p.startsWith(r))) fail('.policy.panelReadablePrefixes', `readablePrefixes보다 넓힐 수 없음: ${JSON.stringify(p)}`);
  }

  const params: Record<string, string | number | string[]> = {};
  for (const [k, v] of Object.entries(root.params === undefined ? {} : obj(root.params, '.params'))) {
    if (!IDENT_PREFIX_RE.test(k)) fail(`.params.${k}`, '키는 식별자여야 함');
    if (k === 'as_of') fail('.params.as_of', '엔진 예약어');
    // 값은 수·시각·"시각~시각"과 그 배열만(자유 문자열 금지)
    if (typeof v === 'number' && Number.isFinite(v)) params[k] = v;
    else if (typeof v === 'string') params[k] = paramValue(v, `.params.${k}`);
    else params[k] = strArray(v, `.params.${k}`).map((x, i) => paramValue(x, `.params.${k}[${i}]`));
  }

  const a = root.agent === undefined ? {} : obj(root.agent, '.agent');
  onlyKeys(a, Object.keys(AGENT_DEFAULTS), '.agent');
  const agent: AgentConfig = { ...AGENT_DEFAULTS };
  if (a.bin !== undefined) agent.bin = str(a.bin, '.agent.bin');
  if (a.model !== undefined) agent.model = optStr(a.model, '.agent.model');
  if (a.callBudgetUsd !== undefined) agent.callBudgetUsd = num(a.callBudgetUsd, '.agent.callBudgetUsd', { min: 0, exclusiveMin: true });
  if (a.requestBudgetUsd !== undefined) agent.requestBudgetUsd = num(a.requestBudgetUsd, '.agent.requestBudgetUsd', { min: 0, exclusiveMin: true });
  for (const k of ['maxTurns', 'maxProbes', 'maxFixes', 'callTimeoutMs', 'concurrency'] as const) {
    if (a[k] !== undefined) agent[k] = num(a[k], `.agent.${k}`, { int: true, min: k === 'maxFixes' || k === 'maxProbes' ? 0 : 1 });
  }
  if (a.dataMode !== undefined) {
    if (a.dataMode !== 'pseudonymized' && a.dataMode !== 'schema_only') fail('.agent.dataMode', '"pseudonymized" 또는 "schema_only"');
    agent.dataMode = a.dataMode;
  }

  const r = root.run === undefined ? {} : obj(root.run, '.run');
  onlyKeys(r, ['heapLimitMb'], '.run');
  const s = root.server === undefined ? {} : obj(root.server, '.server');
  onlyKeys(s, ['port', 'publicOrigin', 'proxyHops', 'auditRetentionDays'], '.server');
  let publicOrigin: string | null = null;
  if (s.publicOrigin !== undefined) {
    const raw = str(s.publicOrigin, '.server.publicOrigin');
    let u: URL;
    try {
      u = new URL(raw);
    } catch {
      fail('.server.publicOrigin', 'URL 형식 오류');
    }
    if (u.protocol !== 'https:' || u.username || u.password || (u.pathname !== '/' && u.pathname !== '') || u.search || u.hash) fail('.server.publicOrigin', 'https://호스트[:포트] 형식만');
    publicOrigin = u.origin;
  }
  if (s.proxyHops !== undefined && !publicOrigin) fail('.server.proxyHops', 'publicOrigin이 있을 때만');

  return {
    name: str(root.name, '.name'),
    source,
    policy: { readablePrefixes, panelReadablePrefixes },
    params,
    agent,
    run: { heapLimitMb: r.heapLimitMb === undefined ? 2048 : num(r.heapLimitMb, '.run.heapLimitMb', { int: true, min: 64 }) },
    server: {
      port: s.port === undefined ? 4170 : num(s.port, '.server.port', { int: true, min: 1 }),
      publicOrigin,
      proxyHops: s.proxyHops === undefined ? 1 : num(s.proxyHops, '.server.proxyHops', { int: true, min: 1 }),
      auditRetentionDays: s.auditRetentionDays === undefined ? 90 : num(s.auditRetentionDays, '.server.auditRetentionDays', { int: true, min: 1 }),
    },
    outDir: root.outDir === undefined ? dir : relativeOutDir(dir, str(root.outDir, '.outDir')),
  };
}

function relativeOutDir(dir: string, v: string): string {
  if (isAbsolute(v) || v.startsWith('~')) fail('.outDir', '워크스페이스 기준 상대 경로여야 함');
  const out = resolve(dir, v);
  const rel = relative(dir, out);
  if (rel === '' || rel.startsWith('..') || isAbsolute(rel)) fail('.outDir', '워크스페이스 안의 하위 폴더여야 함');
  return out;
}

/** 공개 예제 워크스페이스. 워크스페이스 경로 자체는 git 제외 검사에서 뺀다 */
export const PUBLIC_EXAMPLE_DIR = resolve(import.meta.dirname, '..', 'examples', 'demo');

/** git 제외 여부를 확인할 경로: 공개 예제가 아니면 워크스페이스 경로도 포함 */
export function guardedPaths(ws: Workspace): string[] {
  const isPublicExample = realpathLoose(ws.dir) === realpathLoose(PUBLIC_EXAMPLE_DIR);
  return [...(isPublicExample ? [] : [ws.dir]), ...outputPaths(ws.dir, ws.config.outDir)];
}

export function loadWorkspace(dir: string): Workspace {
  const file = join(dir, 'workspace.json');
  if (!existsSync(file)) throw new ConfigError(`워크스페이스 설정이 없음: ${file}`);
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(file, 'utf8'));
  } catch (e) {
    throw new ConfigError(`workspace.json을 읽지 못함: ${(e as Error).message}`);
  }
  return { dir, config: parseWorkspaceConfig(raw, dir) };
}

/** 엔진이 쓰는 산출물 경로 */
export function outputPaths(workspaceDir: string, outRoot = workspaceDir): string[] {
  return [outRoot, ...['snapshots', 'panels', 'conversations', 'results', 'logs', '.agent-cwd'].map((d) => join(outRoot, d))];
}

export function gitRoot(cwd: string): string | null {
  try {
    return execFileSync('git', ['rev-parse', '--show-toplevel'], { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
  } catch {
    return null;
  }
}

/** 아직 없는 경로도 가장 가까운 기존 조상 기준으로 realpath */
function realpathLoose(p: string): string {
  const abs = resolve(p);
  if (existsSync(abs)) return realpathSync(abs);
  const parent = dirname(abs);
  return parent === abs ? abs : join(realpathLoose(parent), basename(abs));
}

/** 저장소 안에 있으면서 git에서 제외되지 않은 경로 목록 */
export function findUnignoredOutputs(paths: string[], cwd = process.cwd()): string[] {
  const root = gitRoot(cwd);
  if (!root) return [];
  const bad: string[] = [];
  for (const p of paths) {
    const rel = relative(realpathLoose(root), realpathLoose(p));
    if (rel === '') {
      bad.push('.');
      continue;
    }
    if (rel.startsWith('..') || isAbsolute(rel)) continue;
    const target = rel.split(sep).join('/') + '/';
    try {
      execFileSync('git', ['check-ignore', '-q', '--no-index', target], { cwd: root, stdio: 'ignore' });
    } catch {
      bad.push(rel);
    }
  }
  return bad;
}

export function assertOutputsIgnored(paths: string[], cwd = process.cwd()): void {
  const bad = findUnignoredOutputs(paths, cwd);
  if (bad.length > 0) {
    throw new ConfigError(`git에서 제외되지 않은 산출물 경로: ${bad.join(', ')} (.gitignore에 추가하거나 워크스페이스를 저장소 밖에 두세요)`);
  }
}
