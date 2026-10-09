// Agent: claude runner, action schema, turn input, context, state machine.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, existsSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildArgs, childEnv, ClaudeRunner, StreamParser } from '../src/agent/claude.ts';
import type { CallResult } from '../src/agent/runner.ts';
import { ACTION_SCHEMA_ARG, parseAction, ActionError } from '../src/agent/actions.ts';
import { Outbound, OutboundBlocked, render, columnStats } from '../src/agent/outbound.ts';
import { buildContext, CONTEXT_LIMIT, ContextError, columnDescriptions } from '../src/agent/context.ts';
import { AgentRequest, type LoopDeps, type LoopEvent } from '../src/agent/loop.ts';
import { parsePanelSpec, type PanelSpec } from '../src/panels/spec.ts';
import type { PanelRunResult } from '../src/panels/run.ts';
import type { QueryResult } from '../src/query/executor.ts';
import type { Role } from '../src/collect/spec.ts';

const FAKE = join(import.meta.dirname, 'fake-claude.ts');

function fakeClaude(script: unknown[]) {
  const dir = mkdtempSync(join(tmpdir(), 'gl-fc-'));
  const bin = join(dir, 'claude');
  // The engine passes almost no env to the child, so the script path is baked into the executable
  writeFileSync(bin, `#!/bin/sh\nFAKE_CLAUDE_SCRIPT="${join(dir, 'script.json')}" FAKE_CLAUDE_DIR="${dir}" exec "${process.execPath}" "${FAKE}" "$@"\n`);
  chmodSync(bin, 0o755);
  writeFileSync(join(dir, 'script.json'), JSON.stringify(script));
  const runner = new ClaudeRunner({ bin, cwd: join(dir, '.agent-cwd'), logDir: join(dir, 'logs') });
  const argv = () => (existsSync(join(dir, 'argv.jsonl')) ? readFileSync(join(dir, 'argv.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l) as string[]) : []);
  const children = () => (existsSync(join(dir, 'children.txt')) ? readFileSync(join(dir, 'children.txt'), 'utf8').trim().split('\n').map(Number) : []);
  return { runner, argv, children, dir };
}

test('child env: only the allow-list and ANTHROPIC_/CLAUDE_ are passed', () => {
  const env = childEnv({ PATH: '/bin', HOME: '/h', ANTHROPIC_API_KEY: 'k', CLAUDE_CONFIG_DIR: '/c', MYSQL_PWD: 'x', AWS_SECRET_ACCESS_KEY: 'y', DATABASE_URL: 'z', GROWTH_LAB_WORKSPACE: 'w' });
  assert.deepEqual(Object.keys(env).sort(), ['ANTHROPIC_API_KEY', 'CLAUDE_CONFIG_DIR', 'HOME', 'PATH']);
});

const callOpts = { input: 'hi', systemPrompt: 'ctx', jsonSchema: ACTION_SCHEMA_ARG, budgetUsd: 0.5, timeoutMs: 5000 };
const alive = (pid: number) => { try { process.kill(pid, 0); return true; } catch { return false; } };
const errType = (r: CallResult) => (r.ok ? 'ok' : r.type);


test('arguments: isolation flags, stream-json, schema, budget; model and resume only when given', () => {
  const a = buildArgs({ ...callOpts, sessionId: 's1', model: 'm' });
  assert.deepEqual(a.slice(0, 8), ['-p', 'hi', '--safe-mode', '--strict-mcp-config', '--tools', '', '--output-format', 'stream-json']);
  assert.ok(a.includes('--verbose'));
  assert.equal(a[a.indexOf('--max-budget-usd') + 1], '0.5000');
  assert.deepEqual(a.slice(-4), ['--model', 'm', '--resume', 's1']);
  assert.ok(!buildArgs(callOpts).includes('--resume'));
});

test('stream contract: one init first, one result last, non-JSON lines only counted', () => {
  const p = new StreamParser();
  for (const l of ['not json', '{"type":"system","subtype":"init"}', '{"type":"assistant"}', '[1]', '{"type":"result"}']) p.line(l);
  assert.equal(p.violation, null);
  assert.equal(p.nonJson, 2);
  const bad = (lines: string[]) => { const q = new StreamParser(); for (const l of lines) q.line(l); return q.violation; };
  assert.equal(bad(['{"type":"assistant"}']), 'event before init');
  assert.equal(bad(['{"type":"system","subtype":"init"}', '{"type":"system","subtype":"init"}']), 'duplicate init');
  assert.equal(bad(['{"type":"system","subtype":"init"}', '{"type":"result"}', '{"type":"result"}']), 'event after result');
  assert.equal(bad(['{"type":"system","subtype":"init"}', '{"type":"result"}', '{"type":"assistant"}']), 'event after result');
});

test('claude run: chunked output succeeds with session, cost and structured_output', async () => {
  const f = fakeClaude([{ structured: { action: 'refuse', reason: 'r', alternatives: [] }, sessionId: 'abc', cost: 0.07, chunk: 7, delayMs: 1 }]);
  const r = await f.runner.call(callOpts);
  assert.ok(r.ok);
  if (r.ok) assert.deepEqual([r.sessionId, r.costUsd, (r.structured as { step: { action: string } }).step.action], ['abc', 0.07, 'refuse']);
});

test('claude run: truncated output, duplicate init/result, output after result, exit code mismatch and empty output are process errors', async () => {
  const init = { type: 'system', subtype: 'init', session_id: 's', tools: [], mcp_servers: [] };
  const result = { type: 'result', subtype: 'success', is_error: false, session_id: 's', total_cost_usd: 0.01, structured_output: {} };
  const cases: unknown[] = [
    { events: [init] },
    { events: [init, init, result] },
    { events: [init, result, result] },
    { events: [init, result, { type: 'assistant' }] },
    { events: [init, result], exitCode: 1 },
    { raw: [] },
    { raw: ['{"type":"system","subtype":"init","session_id":"s","tools":[],"mcp_servers":[]}', '{"type":"result","subtype":"succ'] },
  ];
  for (const c of cases) assert.equal(errType(await fakeClaude([c]).runner.call(callOpts)), 'process', JSON.stringify(c));
});

test('claude run: result error classes — budget, rate limit, resume failure, other', async () => {
  assert.equal(errType(await fakeClaude([{ isError: true, subtype: 'error_max_budget_usd' }]).runner.call(callOpts)), 'budget');
  assert.equal(errType(await fakeClaude([{ isError: true, resultText: 'API Error: 529 Overloaded' }]).runner.call(callOpts)), 'rate_limit');
  assert.equal(errType(await fakeClaude([{ raw: [], stderr: 'No conversation found with session ID: x', exitCode: 1 }]).runner.call({ ...callOpts, sessionId: 'x' })), 'resume');
  assert.equal(errType(await fakeClaude([{ isError: true, subtype: 'error_during_execution' }]).runner.call(callOpts)), 'error');
});

test('isolation check: any tool other than StructuredOutput, or any MCP, kills the process with isolation', async () => {
  assert.ok((await fakeClaude([{ structured: {}, tools: ['StructuredOutput'] }]).runner.call(callOpts)).ok);
  assert.equal(errType(await fakeClaude([{ structured: {}, tools: ['Bash'] }]).runner.call(callOpts)), 'isolation');
  assert.equal(errType(await fakeClaude([{ structured: {}, tools: ['StructuredOutput', 'Read'] }]).runner.call(callOpts)), 'isolation');
  assert.equal(errType(await fakeClaude([{ structured: {}, mcpServers: [{ name: 'x' }] }]).runner.call(callOpts)), 'isolation');
  const noField = { type: 'system', subtype: 'init', session_id: 's' };
  assert.equal(errType(await fakeClaude([{ events: [noField] }]).runner.call(callOpts)), 'isolation');
});

test('timeout and cancel: child processes are killed, a result arriving during cancel is dropped', async () => {
  const f = fakeClaude([{ structured: {}, hang: true, spawnChild: true }]);
  const r = await f.runner.call({ ...callOpts, timeoutMs: 400 });
  assert.equal(errType(r), 'timeout');
  await new Promise((res) => setTimeout(res, 100));
  for (const pid of f.children()) assert.equal(alive(pid), false, `child ${pid} is still alive`);

  const g = fakeClaude([{ structured: { action: 'refuse', reason: 'r', alternatives: [] }, hang: true, spawnChild: true }]);
  const ac = new AbortController();
  const p = g.runner.call({ ...callOpts, signal: ac.signal });
  await new Promise((res) => setTimeout(res, 300));
  ac.abort();
  assert.equal(errType(await p), 'cancelled');
  await new Promise((res) => setTimeout(res, 100));
  for (const pid of g.children()) assert.equal(alive(pid), false);
});

test('claude run: multibyte characters split across chunks parse correctly', async () => {
  const f = fakeClaude([{ structured: { action: 'refuse', reason: '가입 주별 연결률은 그릴 수 없어요 🙏', alternatives: ['한글 대안'] }, chunkBytes: 5, delayMs: 1 }]);
  const r = await f.runner.call(callOpts);
  assert.ok(r.ok, JSON.stringify(r));
  if (r.ok) assert.equal((r.structured as { step: { reason: string } }).step.reason, '가입 주별 연결률은 그릴 수 없어요 🙏');
});

test('process cleanup: descendants ignoring SIGTERM are killed before the result (normal exit and timeout)', async () => {
  const ok = fakeClaude([{ structured: { action: 'refuse', reason: 'r', alternatives: [] }, spawnChild: true, childIgnoresTerm: true, chunk: 50, delayMs: 30 }]);
  const t0 = Date.now();
  assert.ok((await ok.runner.call(callOpts)).ok);
  assert.ok(Date.now() - t0 >= 2000, 'should have reached SIGKILL');
  for (const pid of ok.children()) assert.equal(alive(pid), false, `descendant ${pid} alive after normal exit`);
  const slow = fakeClaude([{ structured: {}, hang: true, spawnChild: true, childIgnoresTerm: true }]);
  assert.equal(errType(await slow.runner.call({ ...callOpts, timeoutMs: 300 })), 'timeout');
  for (const pid of slow.children()) assert.equal(alive(pid), false, `descendant ${pid} alive after timeout`);
});

test('session id: init/result mismatch is process, a different session than requested is resume (checked even on error results)', async () => {
  assert.equal(errType(await fakeClaude([{ structured: {}, sessionId: 'a', resultSessionId: 'b' }]).runner.call(callOpts)), 'process');
  const mismatchedError = await fakeClaude([{ isError: true, resultText: 'overloaded', sessionId: 'a', resultSessionId: 'b' }]).runner.call(callOpts);
  assert.deepEqual([errType(mismatchedError), mismatchedError.ok ? '' : mismatchedError.sessionId], ['process', null]);
  const wrongResumeError = await fakeClaude([{ isError: true, resultText: 'overloaded', sessionId: 'other' }]).runner.call({ ...callOpts, sessionId: 'wanted' });
  assert.deepEqual([errType(wrongResumeError), wrongResumeError.ok ? '' : wrongResumeError.sessionId], ['resume', null]);
  assert.equal(errType(await fakeClaude([{ structured: {}, sessionId: 'other' }]).runner.call({ ...callOpts, sessionId: 'wanted' })), 'resume');
  assert.ok((await fakeClaude([{ structured: {}, sessionId: 'wanted' }]).runner.call({ ...callOpts, sessionId: 'wanted' })).ok);
});


const pa = (a: unknown) => parseAction({ step: a });

const panelRaw = {
  metric: 'demo_metric',
  title: 't', question: 'q', sql: 'SELECT 1 AS numerator, 2 AS denominator',
  display: { type: 'number', x: null, numerator: 'numerator', denominator: 'denominator', series: null, headline: null },
  definition: [['Population', 'x']], caveats: [], answers: [],
};

test('actions: all four parse, null role columns allowed', () => {
  assert.equal(pa({ action: 'ask', questions: [{ id: 'win', text: 'Period?', options: [{ label: '7 days', is_default: true }, { label: '14 days', is_default: false }], allow_free_text: true }] }).action, 'ask');
  assert.equal(pa({ action: 'probe', plan: 'p', purpose: 'u', sql: 'SELECT 1' }).action, 'probe');
  const p = pa({ action: 'panel', plan: 'p', panel: panelRaw });
  assert.ok(p.action === 'panel' && p.panel.display.columns.numerator === 'numerator');
  assert.equal(pa({ action: 'refuse', reason: 'r', alternatives: ['a'] }).action, 'refuse');
  assert.equal(parseAction({ step: { action: 'refuse', reason: 'r', alternatives: [] } }).action, 'refuse');
  assert.throws(() => parseAction({ step: { action: 'refuse', reason: 'r', alternatives: [] }, extra: 1 }), ActionError);
  assert.throws(() => parseAction({ action: 'refuse', reason: 'r', alternatives: [] }), ActionError);
});

test('actions: violations are ActionError (one default, duplicate id, unknown key, panel spec error)', () => {
  const q = (o: Record<string, unknown>) => ({ action: 'ask', questions: [{ id: 'a', text: 't', options: [{ label: 'x', is_default: true }], allow_free_text: false, ...o }] });
  for (const bad of [
    null, {}, { action: 'run' },
    q({ options: [{ label: 'x', is_default: false }] }),
    q({ options: [{ label: 'x', is_default: true }, { label: 'y', is_default: true }] }),
    { action: 'ask', questions: [q({}).questions[0], q({}).questions[0]] },
    q({ id: 'Bad-Id' }),
    { action: 'probe', plan: 'p', purpose: 'u', sql: 'SELECT 1', extra: 1 },
    { action: 'probe', plan: 'p', sql: 'SELECT 1' },
    { action: 'panel', plan: 'p', panel: { ...panelRaw, definition: [] } },
    { action: 'refuse', reason: 'r', alternatives: ['a', 'b', 'c', 'd'] },
  ]) assert.throws(() => pa(bad), ActionError, JSON.stringify(bad));
});


const roles = new Map<string, Role>([['r_m.id', { identifier: 'm' }], ['r_m.n', 'ordinary']]);
const probeOk: QueryResult = {
  ok: true, ms: 3, more: true, truncatedCells: 0,
  columns: [{ name: 'id', table: 'r_m', column: 'id' }, { name: 'n', table: 'r_m', column: 'n' }, { name: 'c', table: null, column: null }],
  rows: [[['i', 1000000123], ['i', 5], ['i', 2]], [['i', 1000000456], ['n', null], ['i', 9]]],
};
const panelClean = { ...panelRaw, display: { type: 'number', numerator: 'numerator', denominator: 'denominator' } };
const spec1: PanelSpec = parsePanelSpec(panelClean);
const invFail: Extract<PanelRunResult, { ok: false }> = {
  ok: false, stage: 'invariant', message: 'invariant violations:\n- x=SECRET: numerator(11) > denominator(10)',
  violations: [{ row: 'x=SECRET', problem: 'numerator(11) > denominator(10)', index: 3, rule: 'numerator_le_denominator', column: 'numerator' }],
};

test('outbound (pseudonymized): probe rows (pseudonymous) and remaining count, panel failure message, default answers', () => {
  const o = new Outbound('pseudonymized', roles);
  const t = o.probeResult(probeOk, 2);
  assert.match(t, /1000000123/);
  assert.match(t, /Probes left: 2/);
  assert.match(o.panelFailure(invFail, 1), /x=SECRET/);
  const a = o.answers([{ id: 'w', text: 'Period', options: [{ label: '7 days', is_default: true }], allow_free_text: true }], {});
  assert.match(a, /"answer": "7 days"/);
  assert.match(a, /"defaulted": true/);
});

test('outbound (schema_only): no row values leave on any path', () => {
  const o = new Outbound('schema_only', roles);
  const outputs = [
    o.request('Show recent signups', spec1, null),
    o.probeResult(probeOk, 1),
    o.panelFailure(invFail, 1),
    o.recoverySummary({ inputs: ['a'], lastAsk: null, current: spec1 }),
    o.schemaMismatch('x'),
  ];
  for (const t of outputs) {
    assert.doesNotMatch(t, /1000000123|1000000456|SECRET|numerator\(11\)/, t);
  }
  assert.match(outputs[1], /"row_count":2/);
  assert.match(outputs[2], /"rule":"numerator_le_denominator","index":3,"column":"numerator"/);
  assert.throws(() => render('schema_only', [{ kind: 'rows', target: 'agent', columns: ['a'], rows: [[['i', 1]]], more: false, truncatedCells: 0 }]), OutboundBlocked);
});

test('schema_only stats: identifier columns only have a NULL count, computed columns have min and max', () => {
  const s = columnStats(probeOk.ok ? probeOk.columns : [], probeOk.ok ? probeOk.rows : [], roles);
  assert.deepEqual(s, [
    { name: 'id', nulls: 0, min: null, max: null, identifier: true },
    { name: 'n', nulls: 1, min: 5, max: 5, identifier: false },
    { name: 'c', nulls: 0, min: 2, max: 9, identifier: false },
  ]);
});

test('recovery summary: last 5 inputs (500 chars each), 8,000-char cap (oldest dropped first)', () => {
  const o = new Outbound('pseudonymized', roles);
  const s = o.recoverySummary({ inputs: ['1', '2', '3', '4', '5', '6'], lastAsk: null, current: null });
  assert.doesNotMatch(s, /"1"/);
  assert.match(s, /"6"/);
  const bigSpec = parsePanelSpec({ ...panelClean, sql: `SELECT 1 /* ${'s'.repeat(5_500)} */` });
  const long = o.recoverySummary({ inputs: Array.from({ length: 5 }, (_, i) => `${i}`.repeat(3000)), lastAsk: null, current: bigSpec });
  assert.ok(long.length <= 8000, String(long.length));
  assert.ok(!long.includes('0'.repeat(500)), 'the oldest input should be dropped');
  assert.ok(long.includes('4'.repeat(500)), 'the newest input should remain');
  const hugeSpec = parsePanelSpec({ ...panelClean, sql: `SELECT 1 /* ${'s'.repeat(11_900)} */` });
  const shrunk = o.recoverySummary({ inputs: ['x'], lastAsk: null, current: hugeSpec });
  assert.ok(shrunk.length <= 8000, String(shrunk.length));
  const block = /Current preview panel spec:\n```json\n([\s\S]*?)\n```/.exec(shrunk);
  assert.ok(block, 'should have a panel spec block');
  const parsed = JSON.parse(block![1]);
  assert.match(parsed.sql, /…\(truncated\)$/);
  const qs = Array.from({ length: 4 }, (_, i) => ({ id: `q${i}`, text: 'question'.repeat(45), options: [{ label: 'default', is_default: true }], allow_free_text: true }));
  const answers = Object.fromEntries(qs.map((q) => [q.id, 'answer'.repeat(1000)]));
  const withAsk = o.recoverySummary({ inputs: Array.from({ length: 5 }, () => 'y'.repeat(3000)), lastAsk: { questions: qs, answers }, current: hugeSpec });
  assert.ok(withAsk.length <= 8000, String(withAsk.length));
  for (const m of withAsk.matchAll(/```json\n([\s\S]*?)\n```/g)) JSON.parse(m[1]);
});


test('context: column descriptions found; over 60,000 chars drops seed SQL, then seed panels, from the end', () => {
  const d = columnDescriptions('| `d_x.a` | first |\n- `d_x.b`: second\nanother line');
  assert.deepEqual([...d], [['d_x.a', 'first'], ['d_x.b', 'second']]);
  const seed = (id: string, sqlLen: number) => ({ id, spec: parsePanelSpec({ ...panelClean, title: id, sql: `SELECT 1 /* ${'x'.repeat(sqlLen)} */` }) });
  const state = { asOf: '2024-01-01 00:00:00.000000', today: '2024-01-02', calendarStart: null, params: { a: 'b', arr: ['x'] } };
  const small = buildContext({ schema: 's', metrics: 'm', guide: 'g', seedPanels: [seed('p1', 10)], state });
  assert.match(small, /p1/);
  assert.match(small, /:as_of, :a\b/);
  assert.doesNotMatch(small, /:arr/);
  const five = ['p1', 'p2', 'p3', 'p4', 'p5'].map((id) => seed(id, 11_900));
  const big = buildContext({ schema: 's', metrics: 'm', guide: 'g', seedPanels: five, state });
  assert.ok(big.length <= CONTEXT_LIMIT, String(big.length));
  assert.match(big, /p5/);
  assert.ok((big.match(/x{11900}/g) ?? []).length < 5);
  const baseLen = buildContext({ schema: 's', metrics: 'm', guide: '', seedPanels: [], state }).length;
  const huge = buildContext({ schema: 's', metrics: 'm', guide: 'g'.repeat(CONTEXT_LIMIT - baseLen - 250), seedPanels: five, state });
  assert.ok(huge.length <= CONTEXT_LIMIT);
  assert.match(huge, /p1/);
  assert.doesNotMatch(huge, /### p5/);
  assert.throws(() => buildContext({ schema: 's', metrics: 'm', guide: 'x'.repeat(CONTEXT_LIMIT), seedPanels: [], state }), ContextError);
});


type Harness = { req: AgentRequest; events: (LoopEvent & { turnNo: number })[]; probes: string[]; panels: PanelSpec[]; disabled: () => boolean; argv: () => string[][] };

function harness(script: unknown[], over: Partial<LoopDeps> = {}, panelResults: PanelRunResult[] = []): Harness {
  const f = fakeClaude(script);
  const events: (LoopEvent & { turnNo: number })[] = [];
  const probes: string[] = [];
  const panels: PanelSpec[] = [];
  let disabled = false;
  const deps: LoopDeps = {
    call: ({ input, sessionId, budgetUsd, signal }) => f.runner.call({ input, sessionId, budgetUsd, signal, systemPrompt: 'ctx', jsonSchema: ACTION_SCHEMA_ARG, timeoutMs: 5000 }),
    probe: async (sql) => { probes.push(sql); return probeOk; },
    panel: async (spec) => { panels.push(spec); return panelResults.shift() ?? { ok: true, columns: [{ name: 'numerator', table: null, column: null }, { name: 'denominator', table: null, column: null }], real: [[1, 2]], agent: [[1, 2]], ms: 1, tables: ['d_x'] }; },
    sleep: (ms, signal) => new Promise((res) => { const t = setTimeout(res, Math.min(ms, 20)); signal.addEventListener('abort', () => { clearTimeout(t); res(); }); }),
    outbound: new Outbound('pseudonymized', roles),
    limits: { maxTurns: 8, maxProbes: 4, maxFixes: 2, callBudgetUsd: 0.5, requestBudgetUsd: 1.0 },
    emit: (ev, turnNo) => events.push({ ...ev, turnNo }),
    onIsolationFailure: () => { disabled = true; },
    heartbeatMs: 50,
    ...over,
  };
  return { req: new AgentRequest('req1', deps), events, probes, panels, disabled: () => disabled, argv: f.argv };
}

const start = { text: 'Show the connection rate by signup week', current: null, resumeSessionId: null, recovery: null };
const types = (h: Harness) => h.events.map((e) => e.type).filter((t) => t !== 'step');
const ask = { action: 'ask', questions: [{ id: 'win', text: 'Period?', options: [{ label: '7 days', is_default: true }, { label: '14 days', is_default: false }], allow_free_text: false }] };
const probe = { action: 'probe', plan: 'check counts', purpose: 'cohort size', sql: 'SELECT count(*) FROM d_x' };
const panel = { action: 'panel', plan: 'counted', panel: panelRaw };

test('loop: ask → answer → probe → panel fails → fix → success', async () => {
  const h = harness([{ structured: ask }, { structured: probe }, { structured: panel }, { structured: panel }],
    {}, [{ ok: false, stage: 'invariant', message: 'invariant violations', violations: [] }]);
  await h.req.run(start);
  assert.equal(h.req.state, 'waiting_user');
  const qTurn = h.events.find((e) => e.type === 'question')!.turnNo;
  assert.equal(h.req.answer(qTurn + 1, { win: '14 days' }), null);
  await h.req.answer(qTurn, { win: '14 days' });
  assert.equal(h.req.state, 'done');
  assert.deepEqual(types(h), ['request_started', 'session', 'question', 'answers', 'plan', 'plan', 'plan', 'preview', 'done']);
  assert.deepEqual(h.req.counts, { turns: 4, probes: 1, fixes: 1 });
  assert.equal(h.probes.length, 1);
  assert.equal(h.panels.length, 2);
  const argv = h.argv();
  assert.ok(!argv[0].includes('--resume'));
  for (const a of argv.slice(1)) assert.equal(a[a.indexOf('--resume') + 1], 'sess-1');
  assert.match(argv[2][1], /Probe query result: 2 rows/);
});

test('loop: an all-zero panel is sent back once to check, and accepted if sent again', async () => {
  const zero: PanelRunResult = { ok: true, columns: [{ name: 'numerator', table: null, column: null }, { name: 'denominator', table: null, column: null }], real: [[0, 0], [0, null]], agent: [[0, 0], [0, null]], ms: 1, tables: ['d_x'] };
  const h = harness([{ structured: panel }, { structured: panel }], {}, [zero, zero]);
  await h.req.run(start);
  assert.equal(h.req.state, 'done');
  assert.deepEqual(types(h), ['request_started', 'session', 'plan', 'plan', 'preview', 'done']);
  assert.equal(h.panels.length, 2);
  assert.match(h.argv()[1][1], /Every number column in the panel result is 0/);
  assert.equal(h.req.counts.fixes, 0, 'the warning does not count as a fix');
  const { allZero } = await import('../src/agent/loop.ts');
  assert.equal(allZero([['2026-07-13', 0, 0]]), true);
  assert.equal(allZero([['2026-07-13', 0, 3]]), false);
  assert.equal(allZero([['a', 'b']]), false, 'not all-zero without number columns');
  assert.equal(allZero([]), false);
});

test('loop: off-dictionary panels wait for approval; approve shows the preview, reject asks for a dictionary metric', async () => {
  const off = { action: 'panel', plan: 'counted', panel: { ...panelRaw, metric: null } };
  const a = harness([{ structured: off }]);
  await a.req.run(start);
  assert.equal(a.req.state, 'waiting_user');
  const ev = a.events.find((e) => e.type === 'offdict') as LoopEvent & { turnNo: number; tables: string[] };
  assert.deepEqual(ev.tables, ['d_x']);
  assert.equal(a.req.approveOffdict(ev.turnNo + 1, true), null);
  await a.req.approveOffdict(ev.turnNo, true);
  assert.equal(a.req.state, 'done');
  assert.deepEqual(types(a).slice(-4), ['offdict', 'offdict_answer', 'preview', 'done']);
  assert.equal(a.req.approveOffdict(ev.turnNo, true), null, 'cannot approve twice');

  const b = harness([{ structured: off }, { structured: panel }]);
  await b.req.run(start);
  const t = (b.events.find((e) => e.type === 'offdict')!).turnNo;
  await b.req.approveOffdict(t, false);
  assert.equal(b.req.state, 'done');
  assert.deepEqual(types(b).slice(-5), ['offdict', 'offdict_answer', 'plan', 'preview', 'done']);
  assert.match(b.argv()[1][1], /did not accept a panel built on a definition outside the metric dictionary/);
  assert.equal(b.req.counts.fixes, 0);

  const c = harness([{ structured: off }]);
  await c.req.run(start);
  await c.req.cancel();
  assert.equal(c.req.state, 'cancelled');
});

test('actions: a panel without metric is a schema mismatch', () => {
  const { metric: _m, ...noMetric } = panelRaw;
  assert.throws(() => pa({ action: 'panel', plan: 'p', panel: noMetric }), /metric: required/);
});

test('loop: budget — call cap is min(call cap, request cap − spent); no call when under 0.02 is left', async () => {
  const h = harness([{ structured: probe, cost: 0.6 }, { structured: probe, cost: 0.385 }, { structured: panel }]);
  await h.req.run(start);
  assert.equal(h.req.state, 'failed');
  const budgets = h.argv().map((a) => Number(a[a.indexOf('--max-budget-usd') + 1]));
  assert.deepEqual(budgets, [0.5, 0.4]);
  assert.equal(h.events.at(-1)!.type === 'failed' && (h.events.at(-1) as { reason: string }).reason, 'request_budget');
});

test('loop: limits — turns, probes, fixes', async () => {
  const turns = harness([{ structured: probe }], { limits: { maxTurns: 3, maxProbes: 10, maxFixes: 2, callBudgetUsd: 0.5, requestBudgetUsd: 10 } });
  await turns.req.run(start);
  assert.equal((turns.events.at(-1) as { reason?: string }).reason, 'turn_limit');
  assert.equal(turns.req.counts.turns, 3);

  const probes = harness([{ structured: probe }], { limits: { maxTurns: 10, maxProbes: 2, maxFixes: 2, callBudgetUsd: 0.5, requestBudgetUsd: 10 } });
  await probes.req.run(start);
  assert.equal((probes.events.at(-1) as { reason?: string }).reason, 'probe_limit');
  assert.equal(probes.probes.length, 2);

  const bad: PanelRunResult = { ok: false, stage: 'exec', message: 'no such column' };
  const fixes = harness([{ structured: panel }], {}, [bad, bad, bad]);
  await fixes.req.run(start);
  assert.equal((fixes.events.at(-1) as { reason?: string }).reason, 'fix_limit');
  assert.equal(fixes.req.counts.fixes, 2);
});

test('loop: a fix is counted when the next call actually starts (not when blocked by the turn limit)', async () => {
  const bad: PanelRunResult = { ok: false, stage: 'exec', message: 'no such column' };
  const h = harness([{ structured: panel }], { limits: { maxTurns: 1, maxProbes: 4, maxFixes: 2, callBudgetUsd: 0.5, requestBudgetUsd: 1 } }, [bad]);
  await h.req.run(start);
  assert.equal((h.events.at(-1) as { reason?: string }).reason, 'turn_limit');
  assert.equal(h.req.counts.fixes, 0);
});

test('loop: a retryable result error continues with the session id it returned', async () => {
  const h = harness([{ isError: true, resultText: 'overloaded', sessionId: 's-first' }, { structured: panel, sessionId: 's-first' }]);
  await h.req.run(start);
  assert.equal(h.req.state, 'done');
  const second = h.argv()[1];
  assert.equal(second[second.indexOf('--resume') + 1], 's-first');
});

test('loop: a schema mismatch is retried once in the same session with the error, failed the second time', async () => {
  const once = harness([{ structured: { action: 'nope' } }, { structured: panel }]);
  await once.req.run(start);
  assert.equal(once.req.state, 'done');
  assert.match(once.argv()[1][1], /does not match the action schema/);
  assert.equal(once.argv()[1][once.argv()[1].indexOf('--resume') + 1], 'sess-1');
  const twice = harness([{ structured: { action: 'nope' } }, { structured: { action: 'nope' } }]);
  await twice.req.run(start);
  assert.equal((twice.events.at(-1) as { reason?: string }).reason, 'schema');
});

test('loop: retries — process error once, rate limit twice after waiting, budget fails without retry', async () => {
  const proc = harness([{ raw: [] }, { structured: panel }]);
  await proc.req.run(start);
  assert.equal(proc.req.state, 'done');
  assert.equal(proc.req.counts.turns, 2);

  const rate = harness([{ isError: true, resultText: 'overloaded' }, { isError: true, resultText: 'overloaded' }, { structured: panel }]);
  await rate.req.run(start);
  assert.equal(rate.req.state, 'done');
  const rate3 = harness([{ isError: true, resultText: 'rate limit' }]);
  await rate3.req.run(start);
  assert.equal((rate3.events.at(-1) as { reason?: string }).reason, 'rate_limit');
  assert.equal(rate3.req.counts.turns, 3);

  const budget = harness([{ isError: true, subtype: 'error_max_budget_usd' }, { structured: panel }]);
  await budget.req.run(start);
  assert.equal((budget.events.at(-1) as { reason?: string }).reason, 'call_budget');
  assert.equal(budget.req.counts.turns, 1);
});

test('loop: isolation failure turns off the agent and fails', async () => {
  const h = harness([{ structured: panel, tools: ['Bash'] }]);
  await h.req.run(start);
  assert.equal((h.events.at(-1) as { reason?: string }).reason, 'isolation');
  assert.ok(h.disabled());
});

test('loop: same context resumes; a different one starts a new session with a summary; resume failure also recovers with the summary', async () => {
  const resume = harness([{ structured: panel, sessionId: 'old' }]);
  await resume.req.run({ ...start, resumeSessionId: 'old', recovery: 'summary' });
  assert.equal(resume.req.state, 'done');
  assert.equal(resume.argv()[0][resume.argv()[0].indexOf('--resume') + 1], 'old');
  assert.doesNotMatch(resume.argv()[0][1], /Summary of the earlier conversation/);

  const fresh = harness([{ structured: panel }]);
  await fresh.req.run({ ...start, resumeSessionId: null, recovery: 'summary text' });
  assert.ok(!fresh.argv()[0].includes('--resume'));
  assert.match(fresh.argv()[0][1], /Summary of the earlier conversation \(continuing in a new session\):\nsummary text/);

  const broken = harness([{ raw: [], stderr: 'No conversation found with session ID: old', exitCode: 1 }, { structured: panel }]);
  await broken.req.run({ ...start, resumeSessionId: 'old', recovery: 'summary text' });
  assert.equal(broken.req.state, 'done');
  assert.ok(!broken.argv()[1].includes('--resume'));
  assert.match(broken.argv()[1][1], /summary text/);
});

test('loop: cancel — while calling, probing or waiting for answers; cancelled after the process is confirmed gone', async () => {
  const calling = harness([{ structured: panel, hang: true, spawnChild: true }]);
  const p = calling.req.run(start);
  await new Promise((r) => setTimeout(r, 300));
  await calling.req.cancel();
  await p;
  assert.equal(calling.req.state, 'cancelled');
  assert.equal(calling.events.at(-1)!.type, 'cancelled');
  assert.ok(!calling.events.some((e) => e.type === 'preview' || e.type === 'failed'));

  let release: () => void = () => {};
  const querying = harness([{ structured: probe }], {
    probe: (_sql, signal) => new Promise((res) => {
      release = () => res({ ok: false, kind: 'cancelled', message: 'cancelled' });
      if (signal.aborted) release();
      else signal.addEventListener('abort', () => release());
    }),
  });
  const q = querying.req.run(start);
  await new Promise((r) => setTimeout(r, 400));
  assert.equal(querying.req.state, 'querying');
  assert.ok(querying.events.some((e) => e.type === 'step' && e.kind === 'querying' && /s\)$/.test(e.text)));
  await querying.req.cancel();
  await q;
  assert.equal(querying.req.state, 'cancelled');

  const waiting = harness([{ structured: ask }]);
  await waiting.req.run(start);
  await waiting.req.cancel();
  assert.equal(waiting.req.state, 'cancelled');
  assert.equal(waiting.req.answer(1, {}), null);
});

test('loop: cancelled right as an action arrives, no probe or panel starts', async () => {
  for (const action of [probe, panel]) {
    let reqRef: AgentRequest | null = null;
    const f = fakeClaude([{ structured: action }]);
    const h = harness([{ structured: action }], {
      call: async (o) => {
        const r = await f.runner.call({ ...o, systemPrompt: 'ctx', jsonSchema: ACTION_SCHEMA_ARG, timeoutMs: 5000 });
        void reqRef!.cancel();
        return r;
      },
    });
    reqRef = h.req;
    await h.req.run(start);
    await h.req.cancel();
    assert.equal(h.req.state, 'cancelled');
    assert.deepEqual([h.probes.length, h.panels.length], [0, 0], action.action);
  }
});

test('loop: in schema_only, rows on an outbound path end with outbound_blocked', async () => {
  const blocked = new Outbound('schema_only', roles);
  blocked.probeResult = () => render('schema_only', [{ kind: 'rows', target: 'agent', columns: ['a'], rows: [], more: false, truncatedCells: 0 }]);
  const h = harness([{ structured: probe }], { outbound: blocked });
  await h.req.run(start);
  assert.equal((h.events.at(-1) as { reason?: string }).reason, 'outbound_blocked');
});
