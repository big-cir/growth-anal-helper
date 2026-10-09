// Fake claude CLI that prints scripted stream-json.
// FAKE_CLAUDE_SCRIPT = script JSON (next item per call), FAKE_CLAUDE_DIR = state and log folder.
import { spawn } from 'node:child_process';
import { appendFileSync, existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

type Step = {
  /** Wrapped in { step: … } (as-is with rawStructured) */
  structured?: unknown;
  rawStructured?: boolean;
  events?: Record<string, unknown>[];
  raw?: string[];
  sessionId?: string;
  cost?: number;
  isError?: boolean;
  subtype?: string;
  resultText?: string;
  tools?: unknown[];
  mcpServers?: unknown[];
  chunk?: number;
  delayMs?: number;
  exitCode?: number;
  stderr?: string;
  spawnChild?: boolean;
  childIgnoresTerm?: boolean;
  chunkBytes?: number;
  resultSessionId?: string;
  hang?: boolean;
};

const dir = process.env.FAKE_CLAUDE_DIR!;
const script = JSON.parse(readFileSync(process.env.FAKE_CLAUDE_SCRIPT!, 'utf8')) as Step[];
const stateFile = join(dir, 'state.json');
const n = existsSync(stateFile) ? (JSON.parse(readFileSync(stateFile, 'utf8')) as { calls: number }).calls : 0;
writeFileSync(stateFile, JSON.stringify({ calls: n + 1 }));
appendFileSync(join(dir, 'argv.jsonl'), JSON.stringify(process.argv.slice(2)) + '\n');
const step: Step = script[Math.min(n, script.length - 1)];

const sid = step.sessionId ?? 'sess-1';
let lines: string[];
if (step.raw) lines = step.raw;
else if (step.events) lines = step.events.map((e) => JSON.stringify(e));
else {
  lines = [
    JSON.stringify({ type: 'system', subtype: 'init', session_id: sid, tools: step.tools ?? ['StructuredOutput'], mcp_servers: step.mcpServers ?? [], model: 'fake' }),
    JSON.stringify({ type: 'assistant', message: { content: [{ type: 'text', text: '...' }] }, session_id: sid }),
    JSON.stringify({
      type: 'result', subtype: step.subtype ?? (step.isError ? 'error_during_execution' : 'success'), is_error: step.isError ?? false,
      result: step.resultText ?? '', session_id: step.resultSessionId ?? sid, total_cost_usd: step.cost ?? 0.01, structured_output: step.rawStructured || step.structured === undefined ? step.structured : { step: step.structured },
    }),
  ];
}

if (step.spawnChild) {
  const code = step.childIgnoresTerm ? "process.on('SIGTERM', () => {}); setInterval(() => {}, 1000)" : 'setInterval(() => {}, 1000)';
  const child = spawn(process.execPath, ['-e', code], { stdio: 'ignore' });
  appendFileSync(join(dir, 'children.txt'), `${child.pid}\n`);
  child.unref();
}
if (step.stderr) process.stderr.write(step.stderr);

const out = lines.join('\n') + '\n';
const pieces: (string | Buffer)[] = [];
if (step.chunkBytes) {
  const b = Buffer.from(out, 'utf8');
  for (let i = 0; i < b.length; i += step.chunkBytes) pieces.push(b.subarray(i, i + step.chunkBytes));
} else {
  const chunk = step.chunk ?? out.length;
  for (let i = 0; i < out.length; i += chunk) pieces.push(out.slice(i, i + chunk));
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
for (const p of pieces) {
  process.stdout.write(p);
  if (step.delayMs) await sleep(step.delayMs);
}
if (step.hang) await new Promise(() => setInterval(() => {}, 1000));
process.exitCode = step.exitCode ?? 0;
