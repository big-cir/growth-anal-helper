// Runs `claude -p` and reads its stream-json output.
import { spawn } from 'node:child_process';
import { appendFileSync, mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { StringDecoder } from 'node:string_decoder';
import { secretAssignment, secretShape } from './sensitive.ts';
import type { CallErrorType, CallOptions, CallResult } from './runner.ts';

/** Environment passed to the child (no DB or cloud credentials) */
const ENV_KEYS = new Set(['PATH', 'HOME', 'USER', 'LOGNAME', 'SHELL', 'LANG', 'LC_ALL', 'LC_CTYPE', 'TMPDIR', 'TERM', 'TZ', 'HTTPS_PROXY', 'HTTP_PROXY', 'NO_PROXY', 'https_proxy', 'http_proxy', 'no_proxy', 'NODE_EXTRA_CA_CERTS']);
const ENV_PREFIXES = ['ANTHROPIC_', 'CLAUDE_'];

export function childEnv(env: NodeJS.ProcessEnv, extraPrefixes: string[] = []): NodeJS.ProcessEnv {
  const out: NodeJS.ProcessEnv = {};
  for (const [k, v] of Object.entries(env)) {
    if (v === undefined) continue;
    if (ENV_KEYS.has(k) || [...ENV_PREFIXES, ...extraPrefixes].some((p) => k.startsWith(p))) out[k] = v;
  }
  return out;
}

export const STREAM_LIMITS = { lineBytes: 4 * 1024 * 1024, totalBytes: 16 * 1024 * 1024, stderrBytes: 64 * 1024 };

export function buildArgs(o: CallOptions): string[] {
  return [
    '-p', o.input,
    '--safe-mode', '--strict-mcp-config', '--tools', '',
    '--output-format', 'stream-json', '--verbose',
    '--json-schema', o.jsonSchema,
    '--append-system-prompt', o.systemPrompt,
    '--max-budget-usd', o.budgetUsd.toFixed(4),
    ...(o.model ? ['--model', o.model] : []),
    ...(o.sessionId ? ['--resume', o.sessionId] : []),
  ];
}

const RATE_RE = /rate.?limit|overloaded|\b429\b|\b529\b|too many requests/i;
const BUDGET_RE = /budget/i;
const RESUME_RE = /no conversation found|session.*(not found|does not exist)|could not (find|load|resume)/i;

/** Event order: starts with one init, ends with one result */
export class StreamParser {
  init: Record<string, unknown> | null = null;
  result: Record<string, unknown> | null = null;
  nonJson = 0;
  violation: string | null = null;

  line(text: string): void {
    if (this.violation || text.trim() === '') return;
    let ev: Record<string, unknown>;
    try {
      const v = JSON.parse(text);
      if (!v || typeof v !== 'object' || Array.isArray(v)) {
        this.nonJson++;
        return;
      }
      ev = v as Record<string, unknown>;
    } catch {
      this.nonJson++;
      return;
    }
    const isInit = ev.type === 'system' && ev.subtype === 'init';
    if (this.result) {
      this.violation = 'event after result';
      return;
    }
    if (!this.init) {
      if (!isInit) this.violation = 'event before init';
      else this.init = ev;
      return;
    }
    if (isInit) this.violation = 'duplicate init';
    else if (ev.type === 'result') this.result = ev;
  }
}

/** Output-only tool added by `--json-schema` */
export const ALLOWED_TOOLS = new Set(['StructuredOutput']);

export function isolationProblem(init: Record<string, unknown>): string | null {
  const tools = init.tools;
  const mcp = init.mcp_servers;
  if (!Array.isArray(tools)) return 'init event has no tools';
  if (!Array.isArray(mcp)) return 'init event has no mcp_servers';
  const extra = tools.filter((t) => typeof t !== 'string' || !ALLOWED_TOOLS.has(t));
  if (extra.length) return `init event has tools that are not allowed: ${extra.map(String).join(', ')}`;
  if (mcp.length) return `init event has ${mcp.length} mcp_servers`;
  return null;
}

const groupAlive = (pgid: number) => {
  try {
    process.kill(-pgid, 0);
    return true;
  } catch {
    return false;
  }
};
const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Kills the process group; true when it is gone */
export async function reapGroup(pgid: number): Promise<boolean> {
  if (!groupAlive(pgid)) return true;
  try {
    process.kill(-pgid, 'SIGTERM');
  } catch {
    return !groupAlive(pgid);
  }
  for (let i = 0; i < 20 && groupAlive(pgid); i++) await wait(100);
  if (!groupAlive(pgid)) return true;
  try {
    process.kill(-pgid, 'SIGKILL');
  } catch {
    return !groupAlive(pgid);
  }
  for (let i = 0; i < 50 && groupAlive(pgid); i++) await wait(100);
  return !groupAlive(pgid);
}

export class ClaudeRunner {
  private readonly bin: string;
  private readonly cwd: string;
  private readonly logDir: string;
  private readonly envPrefixes: string[];

  constructor(o: { bin: string; cwd: string; logDir: string; envPrefixes?: string[] }) {
    this.bin = o.bin;
    this.cwd = o.cwd;
    this.logDir = o.logDir;
    this.envPrefixes = o.envPrefixes ?? [];
    mkdirSync(this.cwd, { recursive: true });
  }

  call(o: CallOptions): Promise<CallResult> {
    const t0 = performance.now();
    const ms = () => Math.round(performance.now() - t0);
    if (o.signal?.aborted) return Promise.resolve({ ok: false, type: 'cancelled', message: 'cancelled', sessionId: null, costUsd: 0, ms: 0 });

    return new Promise((resolve) => {
      const child = spawn(this.bin, buildArgs(o), { cwd: this.cwd, detached: true, stdio: ['ignore', 'pipe', 'pipe'], env: childEnv(process.env, this.envPrefixes) });
      const parser = new StreamParser();
      const decoder = new StringDecoder('utf8');
      let total = 0;
      let buf = '';
      let stderr = '';
      let stopReason: { type: CallErrorType; message: string } | null = null;
      let killTimer: NodeJS.Timeout | null = null;

      const terminate = (type: CallErrorType, message: string) => {
        if (!stopReason) stopReason = { type, message };
        try {
          process.kill(-child.pid!, 'SIGTERM');
        } catch {
          // already gone
        }
        killTimer ??= setTimeout(() => {
          try {
            process.kill(-child.pid!, 'SIGKILL');
          } catch {
            // already gone
          }
        }, 2000);
      };
      const timer = setTimeout(() => terminate('timeout', `call timed out (${o.timeoutMs}ms)`), o.timeoutMs);
      const onAbort = () => terminate('cancelled', 'cancelled');
      o.signal?.addEventListener('abort', onAbort, { once: true });

      child.stdout!.on('data', (chunk: Buffer) => {
        total += chunk.length;
        if (total > STREAM_LIMITS.totalBytes) return terminate('process', 'output over 16MB');
        buf += decoder.write(chunk);
        let nl: number;
        while ((nl = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, nl);
          buf = buf.slice(nl + 1);
          if (Buffer.byteLength(line) > STREAM_LIMITS.lineBytes) return terminate('process', 'line over 4MB');
          const hadInit = parser.init !== null;
          parser.line(line);
          if (parser.violation) return terminate('process', `protocol violation: ${parser.violation}`);
          if (!hadInit && parser.init) {
            const problem = isolationProblem(parser.init);
            if (problem) return terminate('isolation', `agent isolation check failed: ${problem}`);
          }
        }
        if (Buffer.byteLength(buf) > STREAM_LIMITS.lineBytes) terminate('process', 'line over 4MB');
      });
      child.stderr!.on('data', (chunk: Buffer) => {
        if (stderr.length < STREAM_LIMITS.stderrBytes) stderr += chunk.toString('utf8').slice(0, STREAM_LIMITS.stderrBytes - stderr.length);
      });
      child.on('error', (e) => terminate('process', `failed to start: ${e.message}`));
      child.on('close', async (code) => {
        clearTimeout(timer);
        if (killTimer) clearTimeout(killTimer);
        o.signal?.removeEventListener('abort', onAbort);
        const reaped = await reapGroup(child.pid!);
        buf += decoder.end();
        if (buf.trim() !== '' && !stopReason) {
          parser.line(buf);
          if (parser.violation) stopReason = { type: 'process', message: `protocol violation: ${parser.violation}` };
        }
        if (stderr) this.saveStderr(stderr);
        const resultSession = typeof parser.result?.session_id === 'string' ? (parser.result.session_id as string) : null;
        const costUsd = typeof parser.result?.total_cost_usd === 'number' ? (parser.result.total_cost_usd as number) : 0;
        const fail = (type: CallErrorType, message: string): CallResult => ({ ok: false, type, message, sessionId: resultSession, costUsd, ms: ms() });

        if (!reaped) return resolve(fail('process', 'process group still alive after SIGKILL'));
        if (stopReason) return resolve(fail(stopReason.type, stopReason.message));
        if (o.signal?.aborted) return resolve(fail('cancelled', 'cancelled'));
        const r = parser.result;
        if (!r) {
          if (o.sessionId && RESUME_RE.test(stderr)) return resolve(fail('resume', 'could not resume session'));
          return resolve(fail('process', parser.init ? 'ended without a result event' : `empty output (exit code ${code})`));
        }
        if (typeof r.session_id !== 'string' || r.session_id !== parser.init?.session_id) return resolve({ ...fail('process', 'init and result session_id differ'), sessionId: null });
        if (o.sessionId && r.session_id !== o.sessionId) return resolve({ ...fail('resume', `continued a different session than ${o.sessionId}`), sessionId: null });
        const isError = r.is_error === true;
        const text = `${String(r.subtype ?? '')} ${String(r.result ?? '')} ${JSON.stringify(r.errors ?? '')}`;
        if (isError || r.subtype !== 'success') {
          if (BUDGET_RE.test(String(r.subtype ?? ''))) return resolve(fail('budget', 'call budget reached'));
          if (RATE_RE.test(text)) return resolve(fail('rate_limit', 'API rate limit or overload'));
          if (o.sessionId && RESUME_RE.test(text + stderr)) return resolve(fail('resume', 'could not resume session'));
          return resolve(fail('error', `result error: ${String(r.subtype ?? '')}`.trim()));
        }
        if (code !== 0) return resolve(fail('process', `result succeeded but exit code was ${code}`));
        resolve({ ok: true, sessionId: r.session_id, structured: r.structured_output, costUsd, ms: ms() });
      });
    });
  }

  private saveStderr(text: string): void {
    if (secretShape(text) || secretAssignment(text)) text = '(not logged: looks like it contains a secret)';
    try {
      const dir = join(this.logDir, 'agent-stderr');
      mkdirSync(dir, { recursive: true });
      appendFileSync(join(dir, `${new Date().toISOString().slice(0, 10)}.log`), `--- ${new Date().toISOString()}\n${text}\n`);
    } catch {
    }
  }
}
