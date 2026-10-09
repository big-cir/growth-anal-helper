// API agent: request shape, continuing conversations, cost, error classes.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer, type IncomingMessage } from 'node:http';
import type { AddressInfo } from 'node:net';
import { mkdtempSync, readdirSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { ApiRunner, type ApiProvider } from '../src/agent/api.ts';
import { ACTION_SCHEMA_ARG } from '../src/agent/actions.ts';
import { parseWorkspaceConfig } from '../src/workspace.ts';

type Seen = { url: string; headers: IncomingMessage['headers']; body: any };
type Respond = (req: Seen, n: number) => { status?: number; body: unknown; delayMs?: number };

const STEP = { step: { action: 'refuse', reason: 'ok', alternatives: [] } };

async function fakeApi(respond: Respond) {
  const seen: Seen[] = [];
  const server = createServer((req, res) => {
    let raw = '';
    req.on('data', (d) => { raw += d; });
    req.on('end', () => {
      const s: Seen = { url: req.url ?? '', headers: req.headers, body: JSON.parse(raw) };
      seen.push(s);
      const r = respond(s, seen.length);
      setTimeout(() => {
        res.writeHead(r.status ?? 200, { 'content-type': 'application/json' });
        res.end(JSON.stringify(r.body));
      }, r.delayMs ?? 0);
    });
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', () => r()));
  return { seen, base: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, close: () => new Promise<void>((r) => { server.closeAllConnections(); server.close(() => r()); }) };
}

const anthropicOk = (input: unknown) => ({ body: { content: [{ type: 'text', text: 'x' }, { type: 'tool_use', id: 't1', name: 'respond', input }], stop_reason: 'tool_use', usage: { input_tokens: 1000, output_tokens: 200 } } });
const openaiOk = (content: string) => ({ body: { choices: [{ message: { role: 'assistant', content }, finish_reason: 'stop' }], usage: { prompt_tokens: 1000, completion_tokens: 200 } } });

function runner(provider: ApiProvider, base: string, extra: Partial<ConstructorParameters<typeof ApiRunner>[0]> = {}) {
  const sessionDir = join(mkdtempSync(join(tmpdir(), 'gl-api-')), 'agent-sessions');
  return { sessionDir, r: new ApiRunner({ provider, baseUrl: base, apiKey: 'test-key-123', pricing: { inputPerMTok: 3, outputPerMTok: 15 }, maxOutputTokens: 4096, sessionDir, ...extra }) };
}
const call = (r: ApiRunner, o: { input?: string; sessionId?: string | null; budgetUsd?: number; timeoutMs?: number; signal?: AbortSignal } = {}) =>
  r.call({ input: o.input ?? 'question', systemPrompt: 'SYS', jsonSchema: ACTION_SCHEMA_ARG, budgetUsd: o.budgetUsd ?? 1, sessionId: o.sessionId ?? null, model: 'm-1', timeoutMs: o.timeoutMs ?? 5000, signal: o.signal });

test('Anthropic: forced output tool, continued conversation, cost from token prices', async () => {
  const api = await fakeApi(() => anthropicOk(STEP));
  try {
    const { r, sessionDir } = runner('anthropic', api.base);
    const a = await call(r, { input: 'first question' });
    assert.ok(a.ok);
    assert.deepEqual(a.structured, STEP);
    assert.equal(a.costUsd, (1000 * 3 + 200 * 15) / 1e6);
    const first = api.seen[0];
    assert.equal(first.url, '/v1/messages');
    assert.equal(first.headers['x-api-key'], 'test-key-123');
    assert.equal(first.headers['anthropic-version'], '2023-06-01');
    assert.equal(first.body.system, 'SYS');
    assert.equal(first.body.max_tokens, 4096);
    assert.deepEqual(first.body.tool_choice, { type: 'tool', name: 'respond' });
    assert.deepEqual(first.body.tools[0].input_schema, JSON.parse(ACTION_SCHEMA_ARG));
    const b = await call(r, { input: 'second', sessionId: a.sessionId });
    assert.ok(b.ok);
    assert.equal(b.sessionId, a.sessionId);
    assert.deepEqual(api.seen[1].body.messages.map((m: any) => [m.role, m.content]), [['user', 'first question'], ['assistant', JSON.stringify(STEP)], ['user', 'second']]);
    const f = readdirSync(sessionDir);
    assert.deepEqual(f, [`${a.sessionId}.json`]);
    assert.equal(statSync(join(sessionDir, f[0])).mode & 0o777, 0o600);
  } finally {
    await api.close();
  }
});

test('OpenAI-compatible: schema in response_format and instructions, fenced JSON accepted, keyless local server', async () => {
  const api = await fakeApi(() => openaiOk('```json\n' + JSON.stringify(STEP) + '\n```'));
  try {
    const { r } = runner('openai', `${api.base}/v1/`, { apiKey: '', pricing: null });
    const a = await call(r);
    assert.ok(a.ok);
    assert.deepEqual(a.structured, STEP);
    assert.equal(a.costUsd, 0);
    const s = api.seen[0];
    assert.equal(s.url, '/v1/chat/completions');
    assert.equal(s.headers.authorization, undefined);
    assert.equal(s.body.model, 'm-1');
    assert.equal(s.body.response_format.type, 'json_schema');
    assert.equal(s.body.messages[0].role, 'system');
    assert.match(s.body.messages[0].content, /^SYS\n\nRespond with one JSON object/);
    assert.equal(s.body.messages.at(-1).content, 'question');
  } finally {
    await api.close();
  }
});

test('a non-JSON reply is passed on as a schema error so the loop asks again', async () => {
  const api = await fakeApi(() => openaiOk('just a sentence'));
  try {
    const a = await call(runner('openai', api.base).r);
    assert.ok(a.ok);
    assert.deepEqual(a.structured, { not_json: 'just a sentence' });
  } finally {
    await api.close();
  }
});

test('error classes: 429/529 are rate_limit, 401 is an error without the key, truncated reply, budget, missing session is resume', async () => {
  const api = await fakeApi((s, n) => {
    if (n === 1) return { status: 429, body: { error: { message: 'slow down' } } };
    if (n === 2) return { status: 529, body: { error: { type: 'overloaded_error', message: 'busy' } } };
    if (n === 3) return { status: 401, body: { error: { message: 'invalid x-api-key' } } };
    if (n === 4) return { body: { content: [], stop_reason: 'max_tokens', usage: { input_tokens: 10, output_tokens: 10 } } };
    return anthropicOk(STEP);
  });
  try {
    const { r, sessionDir } = runner('anthropic', api.base);
    assert.equal(((await call(r)) as { type: string }).type, 'rate_limit');
    assert.equal(((await call(r)) as { type: string }).type, 'rate_limit');
    const auth = await call(r);
    assert.ok(!auth.ok && auth.type === 'error' && /API authentication failed \(401\)/.test(auth.message) && !auth.message.includes('test-key-123'));
    const cut = await call(r);
    assert.ok(!cut.ok && /cut off/.test(cut.message));
    const over = await call(r, { budgetUsd: 0.001 });
    assert.ok(!over.ok && over.type === 'budget' && over.costUsd > 0.001);
    assert.deepEqual(readdirSync(sessionDir), [], 'failed calls do not save the conversation');
    const missing = await call(r, { sessionId: 'api_000000000000000000000000' });
    assert.ok(!missing.ok && missing.type === 'resume' && missing.sessionId === null);
    const bad = await call(r, { sessionId: '../../etc/passwd' });
    assert.ok(!bad.ok && bad.type === 'resume');
  } finally {
    await api.close();
  }
});

test('timeout, cancel, connection failure', async () => {
  const api = await fakeApi(() => ({ ...anthropicOk(STEP), delayMs: 500 }));
  try {
    const { r } = runner('anthropic', api.base);
    const t = await call(r, { timeoutMs: 50 });
    assert.ok(!t.ok && t.type === 'timeout');
    const ac = new AbortController();
    setTimeout(() => ac.abort(), 30);
    const c = await call(r, { signal: ac.signal });
    assert.ok(!c.ok && c.type === 'cancelled');
  } finally {
    await api.close();
  }
  const down = await call(runner('anthropic', 'http://127.0.0.1:1').r);
  assert.ok(!down.ok && down.type === 'process' && /API connection failed/.test(down.message));
});

test('config: required values per provider, CLI-only and API-only keys', () => {
  const base = { name: 'x', datasource: { host: 'sqlite://s.sqlite' }, policy: { readablePrefixes: ['r_', 'd_'] } };
  const agent = (a: unknown) => parseWorkspaceConfig({ ...base, agent: a }, '/ws').agent;
  assert.equal(agent({}).provider, 'claude-code');
  const o = agent({ provider: 'openai', model: 'gemini-2.5-flash', apiKey: 'k', baseUrl: 'https://generativelanguage.googleapis.com/v1beta/openai', pricing: { inputPerMTok: 0.3, outputPerMTok: 2.5 } });
  assert.deepEqual([o.provider, o.model, o.baseUrl, o.pricing], ['openai', 'gemini-2.5-flash', 'https://generativelanguage.googleapis.com/v1beta/openai', { inputPerMTok: 0.3, outputPerMTok: 2.5 }]);
  assert.equal(agent({ provider: 'openai', model: 'llama3', baseUrl: 'http://127.0.0.1:11434/v1' }).apiKey, '');
  for (const bad of [
    { provider: 'codex' },
    { provider: 'anthropic', apiKey: 'k' },
    { provider: 'anthropic', model: 'm' },
    { provider: 'openai', model: 'm', baseUrl: 'ftp://x' },
    { provider: 'openai', model: 'm', pricing: { inputPerMTok: 1 } },
    { apiKey: 'k' },
    { model: 'sonnet', baseUrl: 'https://x.example' },
  ]) assert.throws(() => agent(bad), /workspace\.json \.agent/, JSON.stringify(bad));
});

test('server: with an API provider the startup check calls the API once and passes', async () => {
  const { buildDemoSnapshot } = await import('./helpers/demo-snapshot.ts');
  const { App } = await import('../src/server/app.ts');
  const api = await fakeApi(() => openaiOk(JSON.stringify(STEP)));
  try {
    const demo = await buildDemoSnapshot();
    const ws = { ...demo.ws, config: { ...demo.ws.config, agent: { ...demo.ws.config.agent, provider: 'openai' as const, model: 'gpt-x', apiKey: 'k', baseUrl: api.base } } };
    const app = new App(ws);
    await app.startIsolationCheck();
    assert.deepEqual(app.agent, { state: 'ok', message: 'API connection check passed (openai)' });
    assert.equal(api.seen.length, 1);
  } finally {
    await api.close();
  }
});
