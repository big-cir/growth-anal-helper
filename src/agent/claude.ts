// `claude -p` 실행과 stream-json 결과 처리.
import { spawn } from 'node:child_process';
import { appendFileSync, mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { StringDecoder } from 'node:string_decoder';
import { secretAssignment, secretShape } from './sensitive.ts';

export type CallOptions = {
  input: string;
  systemPrompt: string;
  jsonSchema: string;
  budgetUsd: number;
  sessionId?: string | null;
  model?: string | null;
  timeoutMs: number;
  signal?: AbortSignal;
};

export type CallErrorType =
  | 'timeout'
  | 'process'
  | 'rate_limit'
  | 'budget'
  | 'resume'
  | 'isolation'
  | 'cancelled'
  | 'error';

export type CallResult =
  | { ok: true; sessionId: string; structured: unknown; costUsd: number; ms: number }
  | { ok: false; type: CallErrorType; message: string; sessionId: string | null; costUsd: number; ms: number };

/** 자식 프로세스에 넘기는 환경 변수(DB·클라우드 자격 증명 등은 넘기지 않는다) */
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

/** 이벤트 순서: init 하나로 시작, result 하나로 끝 */
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
      this.violation = 'result 뒤에 다른 이벤트';
      return;
    }
    if (!this.init) {
      if (!isInit) this.violation = 'init 전에 다른 이벤트';
      else this.init = ev;
      return;
    }
    if (isInit) this.violation = 'init 중복';
    else if (ev.type === 'result') this.result = ev;
  }
}

/** `--json-schema`가 넣는 출력 전용 도구 */
export const ALLOWED_TOOLS = new Set(['StructuredOutput']);

export function isolationProblem(init: Record<string, unknown>): string | null {
  const tools = init.tools;
  const mcp = init.mcp_servers;
  if (!Array.isArray(tools)) return 'init 이벤트에 tools가 없음';
  if (!Array.isArray(mcp)) return 'init 이벤트에 mcp_servers가 없음';
  const extra = tools.filter((t) => typeof t !== 'string' || !ALLOWED_TOOLS.has(t));
  if (extra.length) return `init 이벤트에 허용되지 않은 도구가 있음: ${extra.map(String).join(', ')}`;
  if (mcp.length) return `init 이벤트의 mcp_servers가 비어 있지 않음(${mcp.length}개)`;
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

/** 프로세스 그룹을 정리하고, 비었으면 true */
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
    if (o.signal?.aborted) return Promise.resolve({ ok: false, type: 'cancelled', message: '취소됨', sessionId: null, costUsd: 0, ms: 0 });

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
          // 이미 끝남
        }
        killTimer ??= setTimeout(() => {
          try {
            process.kill(-child.pid!, 'SIGKILL');
          } catch {
            // 이미 끝남
          }
        }, 2000);
      };
      const timer = setTimeout(() => terminate('timeout', `호출 시간 초과(${o.timeoutMs}ms)`), o.timeoutMs);
      const onAbort = () => terminate('cancelled', '취소됨');
      o.signal?.addEventListener('abort', onAbort, { once: true });

      child.stdout!.on('data', (chunk: Buffer) => {
        total += chunk.length;
        if (total > STREAM_LIMITS.totalBytes) return terminate('process', '출력이 16MB를 넘음');
        buf += decoder.write(chunk);
        let nl: number;
        while ((nl = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, nl);
          buf = buf.slice(nl + 1);
          if (Buffer.byteLength(line) > STREAM_LIMITS.lineBytes) return terminate('process', '한 줄이 4MB를 넘음');
          const hadInit = parser.init !== null;
          parser.line(line);
          if (parser.violation) return terminate('process', `프로토콜 위반: ${parser.violation}`);
          if (!hadInit && parser.init) {
            const problem = isolationProblem(parser.init);
            if (problem) return terminate('isolation', `에이전트 격리 점검 실패: ${problem}`);
          }
        }
        if (Buffer.byteLength(buf) > STREAM_LIMITS.lineBytes) terminate('process', '한 줄이 4MB를 넘음');
      });
      child.stderr!.on('data', (chunk: Buffer) => {
        if (stderr.length < STREAM_LIMITS.stderrBytes) stderr += chunk.toString('utf8').slice(0, STREAM_LIMITS.stderrBytes - stderr.length);
      });
      child.on('error', (e) => terminate('process', `실행 실패: ${e.message}`));
      child.on('close', async (code) => {
        clearTimeout(timer);
        if (killTimer) clearTimeout(killTimer);
        o.signal?.removeEventListener('abort', onAbort);
        const reaped = await reapGroup(child.pid!);
        buf += decoder.end();
        if (buf.trim() !== '' && !stopReason) {
          parser.line(buf);
          if (parser.violation) stopReason = { type: 'process', message: `프로토콜 위반: ${parser.violation}` };
        }
        if (stderr) this.saveStderr(stderr);
        const resultSession = typeof parser.result?.session_id === 'string' ? (parser.result.session_id as string) : null;
        const costUsd = typeof parser.result?.total_cost_usd === 'number' ? (parser.result.total_cost_usd as number) : 0;
        const fail = (type: CallErrorType, message: string): CallResult => ({ ok: false, type, message, sessionId: resultSession, costUsd, ms: ms() });

        if (!reaped) return resolve(fail('process', '프로세스 그룹을 정리하지 못함(SIGKILL 뒤에도 남음)'));
        if (stopReason) return resolve(fail(stopReason.type, stopReason.message));
        if (o.signal?.aborted) return resolve(fail('cancelled', '취소됨'));
        const r = parser.result;
        if (!r) {
          if (o.sessionId && RESUME_RE.test(stderr)) return resolve(fail('resume', '세션 재개 실패'));
          return resolve(fail('process', parser.init ? 'result 이벤트 없이 끝남' : `빈 출력 (종료 코드 ${code})`));
        }
        if (typeof r.session_id !== 'string' || r.session_id !== parser.init?.session_id) return resolve({ ...fail('process', 'init과 result의 session_id가 다름'), sessionId: null });
        if (o.sessionId && r.session_id !== o.sessionId) return resolve({ ...fail('resume', `요청한 세션(${o.sessionId})이 아닌 세션으로 이어짐`), sessionId: null });
        const isError = r.is_error === true;
        const text = `${String(r.subtype ?? '')} ${String(r.result ?? '')} ${JSON.stringify(r.errors ?? '')}`;
        if (isError || r.subtype !== 'success') {
          if (BUDGET_RE.test(String(r.subtype ?? ''))) return resolve(fail('budget', '호출 비용 상한 도달'));
          if (RATE_RE.test(text)) return resolve(fail('rate_limit', 'API 제한·과부하'));
          if (o.sessionId && RESUME_RE.test(text + stderr)) return resolve(fail('resume', '세션 재개 실패'));
          return resolve(fail('error', `결과 오류: ${String(r.subtype ?? '')}`.trim()));
        }
        if (code !== 0) return resolve(fail('process', `result는 성공인데 종료 코드 ${code}`));
        resolve({ ok: true, sessionId: r.session_id, structured: r.structured_output, costUsd, ms: ms() });
      });
    });
  }

  private saveStderr(text: string): void {
    if (secretShape(text) || secretAssignment(text)) text = '(비밀처럼 보이는 값이 있어 기록하지 않음)';
    try {
      const dir = join(this.logDir, 'agent-stderr');
      mkdirSync(dir, { recursive: true });
      appendFileSync(join(dir, `${new Date().toISOString().slice(0, 10)}.log`), `--- ${new Date().toISOString()}\n${text}\n`);
    } catch {
    }
  }
}
