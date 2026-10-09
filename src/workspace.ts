// Workspace location and workspace.json validation.
import { readFileSync, existsSync, realpathSync } from 'node:fs';
import { homedir } from 'node:os';
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import { execFileSync } from 'node:child_process';
import { normalizeTs } from './time.ts';
import type { Language } from './i18n.ts';
import { parseConnection, type Ga4Connection } from './collect/ga4/config.ts';

export type ServerDatasource = { kind: 'mysql' | 'postgres'; host: string; port: number; user: string; password: string; database: string };
export type Datasource = ServerDatasource | { kind: 'sqlite'; path: string };

export type AgentConfig = {
  /** claude-code: `claude -p`. anthropic, openai: API (openai covers any OpenAI-compatible server) */
  provider: 'claude-code' | 'anthropic' | 'openai';
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
  apiKey: string;
  baseUrl: string | null;
  /** Dollars per million tokens. Without it, cost caps do not apply to API calls */
  pricing: { inputPerMTok: number; outputPerMTok: number } | null;
  /** Response length limit for anthropic */
  maxOutputTokens: number;
};

export type WorkspaceConfig = {
  name: string;
  /** Language of the web UI, server messages and agent-written text */
  language: Language;
  datasource: Datasource;
  /** panelReadablePrefixes: tables panel SQL may read (exploration uses readablePrefixes) */
  policy: { readablePrefixes: string[]; panelReadablePrefixes: string[] };
  params: Record<string, string | number | string[]>;
  agent: AgentConfig;
  run: { heapLimitMb: number };
  /** publicOrigin set: external mode (behind an HTTPS proxy). auth false: no sign-in, everyone is admin `local` */
  server: { port: number; auth: boolean; publicOrigin: string | null; proxyHops: number; auditRetentionDays: number };
  /** Output location. Defaults to the workspace itself */
  outDir: string;
  /** Null: GA4 is not used */
  ga4: Ga4Connection | null;
};

export type Workspace = { dir: string; config: WorkspaceConfig };

const AGENT_DEFAULTS: AgentConfig = {
  provider: 'claude-code',
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
  apiKey: '',
  baseUrl: null,
  pricing: null,
  maxOutputTokens: 8192,
};

/** Workspace location: GROWTH_LAB_WORKSPACE env var, then ./workspace */
export function findWorkspaceDir(env: NodeJS.ProcessEnv = process.env, cwd = process.cwd()): string {
  const fromEnv = env.GROWTH_LAB_WORKSPACE;
  return fromEnv ? resolve(cwd, fromEnv) : resolve(cwd, 'workspace');
}

export class ConfigError extends Error {}

const DEFAULT_PORTS = { mysql: 3306, postgres: 5432 };
const IDENT_PREFIX_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** Timestamp or "timestamp~timestamp" only */
function paramValue(v: string, path: string): string {
  const parts = v.split('~').map((x) => x.trim());
  if (parts.length > 2) fail(path, 'only numbers, timestamps or "timestamp~timestamp"');
  for (const p of parts) {
    try {
      normalizeTs(p);
    } catch {
      fail(path, 'only numbers, timestamps or "timestamp~timestamp" (no free text)');
    }
  }
  return v;
}

function fail(path: string, msg: string): never {
  throw new ConfigError(`workspace.json ${path}: ${msg}`);
}

function obj(v: unknown, path: string): Record<string, unknown> {
  if (!v || typeof v !== 'object' || Array.isArray(v)) fail(path, 'must be an object');
  return v as Record<string, unknown>;
}
function str(v: unknown, path: string): string {
  if (typeof v !== 'string' || v.length === 0) fail(path, 'must be a non-empty string');
  return v;
}
function optStr(v: unknown, path: string): string | null {
  return v === undefined || v === null ? null : str(v, path);
}
function strArray(v: unknown, path: string, { nonEmpty = false } = {}): string[] {
  if (!Array.isArray(v) || v.some((x) => typeof x !== 'string')) fail(path, 'must be an array of strings');
  if (nonEmpty && v.length === 0) fail(path, 'must not be empty');
  return v as string[];
}
function num(v: unknown, path: string, { int = false, min = 0, exclusiveMin = false } = {}): number {
  if (typeof v !== 'number' || !Number.isFinite(v)) fail(path, 'must be a number');
  if (int && !Number.isInteger(v)) fail(path, 'must be an integer');
  if (exclusiveMin ? v <= min : v < min) fail(path, `must be ${exclusiveMin ? '>' : '>='} ${min}`);
  return v;
}
function onlyKeys(o: Record<string, unknown>, allowed: string[], path: string): void {
  for (const k of Object.keys(o)) if (!allowed.includes(k)) fail(`${path}.${k}`, 'unknown key');
}

/** Expands a leading `~/` or `=~/` to the home directory */
export function expandHome(arg: string, home = homedir()): string {
  if (arg.startsWith('~/')) return join(home, arg.slice(2));
  const i = arg.indexOf('=~/');
  if (i >= 0) return `${arg.slice(0, i + 1)}${join(home, arg.slice(i + 3))}`;
  return arg;
}

export function parseWorkspaceConfig(raw: unknown, dir: string, home = homedir()): WorkspaceConfig {
  const root = obj(raw, '(root)');
  onlyKeys(root, ['name', 'language', 'datasource', 'policy', 'params', 'agent', 'run', 'server', 'outDir', 'ga4'], '');

  const datasource = parseDatasource(obj(root.datasource, '.datasource'), dir, home);

  const pol = obj(root.policy, '.policy');
  onlyKeys(pol, ['readablePrefixes', 'panelReadablePrefixes'], '.policy');
  const readablePrefixes = strArray(pol.readablePrefixes, '.policy.readablePrefixes', { nonEmpty: true });
  for (const p of readablePrefixes) {
    if (!IDENT_PREFIX_RE.test(p)) fail('.policy.readablePrefixes', `not an identifier prefix: ${JSON.stringify(p)}`);
    if (p.toLowerCase().startsWith('sqlite_')) fail('.policy.readablePrefixes', 'sqlite_ internal tables are not allowed');
  }
  const panelReadablePrefixes = pol.panelReadablePrefixes === undefined ? ['d_'] : strArray(pol.panelReadablePrefixes, '.policy.panelReadablePrefixes', { nonEmpty: true });
  for (const p of panelReadablePrefixes) {
    if (!IDENT_PREFIX_RE.test(p)) fail('.policy.panelReadablePrefixes', `not an identifier prefix: ${JSON.stringify(p)}`);
    if (!readablePrefixes.some((r) => p.startsWith(r))) fail('.policy.panelReadablePrefixes', `cannot be wider than readablePrefixes: ${JSON.stringify(p)}`);
  }

  const params: Record<string, string | number | string[]> = {};
  for (const [k, v] of Object.entries(root.params === undefined ? {} : obj(root.params, '.params'))) {
    if (!IDENT_PREFIX_RE.test(k)) fail(`.params.${k}`, 'key must be an identifier');
    if (k === 'as_of') fail('.params.as_of', 'reserved by the engine');
    // Values: numbers, timestamps, "timestamp~timestamp" or arrays of those (no free text)
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
  if (a.provider !== undefined) {
    if (a.provider !== 'claude-code' && a.provider !== 'anthropic' && a.provider !== 'openai') fail('.agent.provider', '"claude-code", "anthropic", "openai"');
    agent.provider = a.provider;
  }
  if (a.apiKey !== undefined) {
    if (typeof a.apiKey !== 'string') fail('.agent.apiKey', 'must be a string');
    agent.apiKey = a.apiKey;
  }
  if (a.baseUrl !== undefined) {
    const raw = str(a.baseUrl, '.agent.baseUrl');
    let u: URL;
    try {
      u = new URL(raw);
    } catch {
      fail('.agent.baseUrl', 'invalid URL');
    }
    if (u.protocol !== 'https:' && u.protocol !== 'http:') fail('.agent.baseUrl', 'http(s) URLs only');
    agent.baseUrl = raw;
  }
  if (a.pricing !== undefined) {
    const p = obj(a.pricing, '.agent.pricing');
    onlyKeys(p, ['inputPerMTok', 'outputPerMTok'], '.agent.pricing');
    agent.pricing = { inputPerMTok: num(p.inputPerMTok, '.agent.pricing.inputPerMTok'), outputPerMTok: num(p.outputPerMTok, '.agent.pricing.outputPerMTok') };
  }
  if (a.maxOutputTokens !== undefined) agent.maxOutputTokens = num(a.maxOutputTokens, '.agent.maxOutputTokens', { int: true, min: 256 });
  if (agent.provider !== 'claude-code') {
    if (!agent.model) fail('.agent.model', 'a model name is required for API providers');
    if (agent.provider === 'anthropic' && !agent.apiKey) fail('.agent.apiKey', 'anthropic requires an API key');
  } else {
    for (const k of ['apiKey', 'baseUrl', 'pricing', 'maxOutputTokens'] as const) if (a[k] !== undefined) fail(`.agent.${k}`, 'only for API providers (anthropic, openai)');
  }
  if (a.dataMode !== undefined) {
    if (a.dataMode !== 'pseudonymized' && a.dataMode !== 'schema_only') fail('.agent.dataMode', '"pseudonymized" or "schema_only"');
    agent.dataMode = a.dataMode;
  }

  const r = root.run === undefined ? {} : obj(root.run, '.run');
  onlyKeys(r, ['heapLimitMb'], '.run');
  const s = root.server === undefined ? {} : obj(root.server, '.server');
  onlyKeys(s, ['port', 'auth', 'publicOrigin', 'proxyHops', 'auditRetentionDays'], '.server');
  let publicOrigin: string | null = null;
  if (s.publicOrigin !== undefined) {
    const raw = str(s.publicOrigin, '.server.publicOrigin');
    let u: URL;
    try {
      u = new URL(raw);
    } catch {
      fail('.server.publicOrigin', 'invalid URL');
    }
    if (u.protocol !== 'https:' || u.username || u.password || (u.pathname !== '/' && u.pathname !== '') || u.search || u.hash) fail('.server.publicOrigin', 'only https://host[:port]');
    publicOrigin = u.origin;
  }
  if (s.proxyHops !== undefined && !publicOrigin) fail('.server.proxyHops', 'only with publicOrigin');
  if (s.auth !== undefined && typeof s.auth !== 'boolean') fail('.server.auth', 'true or false');
  const auth = s.auth === true;
  if (publicOrigin && !auth) fail('.server.auth', 'must be true with publicOrigin');

  return {
    name: str(root.name, '.name'),
    language: parseLanguage(root.language),
    datasource,
    policy: { readablePrefixes, panelReadablePrefixes },
    params,
    agent,
    run: { heapLimitMb: r.heapLimitMb === undefined ? 2048 : num(r.heapLimitMb, '.run.heapLimitMb', { int: true, min: 64 }) },
    server: {
      port: s.port === undefined ? 4170 : num(s.port, '.server.port', { int: true, min: 1 }),
      auth,
      publicOrigin,
      proxyHops: s.proxyHops === undefined ? 1 : num(s.proxyHops, '.server.proxyHops', { int: true, min: 1 }),
      auditRetentionDays: s.auditRetentionDays === undefined ? 90 : num(s.auditRetentionDays, '.server.auditRetentionDays', { int: true, min: 1 }),
    },
    outDir: root.outDir === undefined ? dir : relativeOutDir(dir, str(root.outDir, '.outDir')),
    ga4: root.ga4 === undefined ? null : parseGa4(root.ga4, dir, home),
  };
}

function parseLanguage(v: unknown): Language {
  if (v === undefined) return 'en';
  if (v !== 'en' && v !== 'ko') fail('.language', '"en" or "ko"');
  return v;
}

/** host: `mysql://host[:port]`, `postgres://host[:port]` or `sqlite://file path` */
function parseDatasource(o: Record<string, unknown>, dir: string, home: string): Datasource {
  const host = str(o.host, '.datasource.host');
  const m = /^(mysql|postgres|postgresql|sqlite):\/\/(.+)$/.exec(host);
  if (!m) fail('.datasource.host', 'must start with mysql://, postgres:// or sqlite://');
  if (m[1] === 'sqlite') {
    onlyKeys(o, ['host'], '.datasource');
    const p = expandHome(m[2], home);
    return { kind: 'sqlite', path: isAbsolute(p) ? p : resolve(dir, p) };
  }
  onlyKeys(o, ['host', 'user', 'password', 'database'], '.datasource');
  const kind = m[1] === 'mysql' ? 'mysql' : 'postgres';
  let u: URL;
  try {
    u = new URL(`${kind}://${m[2]}`);
  } catch {
    fail('.datasource.host', 'invalid host[:port]');
  }
  if (!u.hostname || u.username || u.password || (u.pathname !== '' && u.pathname !== '/') || u.search || u.hash) fail('.datasource.host', 'host[:port] only (user and database go in their own keys)');
  if (o.password !== undefined && typeof o.password !== 'string') fail('.datasource.password', 'must be a string');
  return {
    kind,
    host: u.hostname.replace(/^\[(.*)\]$/, '$1'),
    port: u.port ? Number(u.port) : DEFAULT_PORTS[kind],
    user: str(o.user, '.datasource.user'),
    password: (o.password as string | undefined) ?? '',
    database: str(o.database, '.datasource.database'),
  };
}

function parseGa4(v: unknown, dir: string, home: string): Ga4Connection {
  try {
    return parseConnection(v, (p) => {
      const x = expandHome(p, home);
      return isAbsolute(x) ? x : resolve(dir, x);
    });
  } catch (e) {
    fail('.ga4', (e as Error).message);
  }
}

function relativeOutDir(dir: string, v: string): string {
  if (isAbsolute(v) || v.startsWith('~')) fail('.outDir', 'must be a path relative to the workspace');
  const out = resolve(dir, v);
  const rel = relative(dir, out);
  if (rel === '' || rel.startsWith('..') || isAbsolute(rel)) fail('.outDir', 'must be a subfolder of the workspace');
  return out;
}

/** Public example workspace. Its own path is skipped by the git-ignore check */
export const PUBLIC_EXAMPLE_DIR = resolve(import.meta.dirname, '..', 'examples', 'demo');

/** Paths that must be git-ignored: includes the workspace itself unless it is the public example */
export function guardedPaths(ws: Workspace): string[] {
  const isPublicExample = realpathLoose(ws.dir) === realpathLoose(PUBLIC_EXAMPLE_DIR);
  return [...(isPublicExample ? [] : [ws.dir]), ...outputPaths(ws.dir, ws.config.outDir)];
}

export function loadWorkspace(dir: string): Workspace {
  const file = join(dir, 'workspace.json');
  if (!existsSync(file)) throw new ConfigError(`workspace config not found: ${file}`);
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(file, 'utf8'));
  } catch (e) {
    throw new ConfigError(`cannot read workspace.json: ${(e as Error).message}`);
  }
  return { dir, config: parseWorkspaceConfig(raw, dir) };
}

/** Output paths written by the engine */
export function outputPaths(workspaceDir: string, outRoot = workspaceDir): string[] {
  return [outRoot, ...['snapshots', 'panels', 'conversations', 'results', 'logs', '.agent-cwd', 'agent-sessions'].map((d) => join(outRoot, d))];
}

export function gitRoot(cwd: string): string | null {
  try {
    return execFileSync('git', ['rev-parse', '--show-toplevel'], { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
  } catch {
    return null;
  }
}

/** realpath that also works for paths that do not exist yet (via the nearest existing ancestor) */
function realpathLoose(p: string): string {
  const abs = resolve(p);
  if (existsSync(abs)) return realpathSync(abs);
  const parent = dirname(abs);
  return parent === abs ? abs : join(realpathLoose(parent), basename(abs));
}

/** Paths inside the repository that are not git-ignored */
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
    throw new ConfigError(`output paths are not git-ignored: ${bad.join(', ')} (add them to .gitignore or keep the workspace outside the repository)`);
  }
}
