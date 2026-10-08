// 에이전트: claude 실행, 행동 스키마, 턴 입력, 컨텍스트, 상태 머신.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, existsSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildArgs, childEnv, ClaudeRunner, StreamParser, type CallResult } from '../src/agent/claude.ts';
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
  // 엔진은 자식에게 환경 변수를 거의 넘기지 않으므로 대본 위치는 실행 파일에 적는다
  writeFileSync(bin, `#!/bin/sh\nFAKE_CLAUDE_SCRIPT="${join(dir, 'script.json')}" FAKE_CLAUDE_DIR="${dir}" exec "${process.execPath}" "${FAKE}" "$@"\n`);
  chmodSync(bin, 0o755);
  writeFileSync(join(dir, 'script.json'), JSON.stringify(script));
  const runner = new ClaudeRunner({ bin, cwd: join(dir, '.agent-cwd'), logDir: join(dir, 'logs') });
  const argv = () => (existsSync(join(dir, 'argv.jsonl')) ? readFileSync(join(dir, 'argv.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l) as string[]) : []);
  const children = () => (existsSync(join(dir, 'children.txt')) ? readFileSync(join(dir, 'children.txt'), 'utf8').trim().split('\n').map(Number) : []);
  return { runner, argv, children, dir };
}

test('자식 환경 변수: 허용 목록과 ANTHROPIC_·CLAUDE_만 넘긴다', () => {
  const env = childEnv({ PATH: '/bin', HOME: '/h', ANTHROPIC_API_KEY: 'k', CLAUDE_CONFIG_DIR: '/c', MYSQL_PWD: 'x', AWS_SECRET_ACCESS_KEY: 'y', DATABASE_URL: 'z', GROWTH_LAB_WORKSPACE: 'w' });
  assert.deepEqual(Object.keys(env).sort(), ['ANTHROPIC_API_KEY', 'CLAUDE_CONFIG_DIR', 'HOME', 'PATH']);
});

const callOpts = { input: 'hi', systemPrompt: 'ctx', jsonSchema: ACTION_SCHEMA_ARG, budgetUsd: 0.5, timeoutMs: 5000 };
const alive = (pid: number) => { try { process.kill(pid, 0); return true; } catch { return false; } };
const errType = (r: CallResult) => (r.ok ? 'ok' : r.type);


test('실행 인자: 격리 옵션, stream-json, 스키마, 예산, 모델·재개는 있을 때만', () => {
  const a = buildArgs({ ...callOpts, sessionId: 's1', model: 'm' });
  assert.deepEqual(a.slice(0, 8), ['-p', 'hi', '--safe-mode', '--strict-mcp-config', '--tools', '', '--output-format', 'stream-json']);
  assert.ok(a.includes('--verbose'));
  assert.equal(a[a.indexOf('--max-budget-usd') + 1], '0.5000');
  assert.deepEqual(a.slice(-4), ['--model', 'm', '--resume', 's1']);
  assert.ok(!buildArgs(callOpts).includes('--resume'));
});

test('스트림 계약: init이 처음 한 번, result가 마지막 한 번, JSON 아닌 줄은 세기만', () => {
  const p = new StreamParser();
  for (const l of ['not json', '{"type":"system","subtype":"init"}', '{"type":"assistant"}', '[1]', '{"type":"result"}']) p.line(l);
  assert.equal(p.violation, null);
  assert.equal(p.nonJson, 2);
  const bad = (lines: string[]) => { const q = new StreamParser(); for (const l of lines) q.line(l); return q.violation; };
  assert.equal(bad(['{"type":"assistant"}']), 'init 전에 다른 이벤트');
  assert.equal(bad(['{"type":"system","subtype":"init"}', '{"type":"system","subtype":"init"}']), 'init 중복');
  assert.equal(bad(['{"type":"system","subtype":"init"}', '{"type":"result"}', '{"type":"result"}']), 'result 뒤에 다른 이벤트');
  assert.equal(bad(['{"type":"system","subtype":"init"}', '{"type":"result"}', '{"type":"assistant"}']), 'result 뒤에 다른 이벤트');
});

test('claude 실행: 잘게 나뉜 출력도 성공, 세션·비용·structured_output', async () => {
  const f = fakeClaude([{ structured: { action: 'refuse', reason: 'r', alternatives: [] }, sessionId: 'abc', cost: 0.07, chunk: 7, delayMs: 1 }]);
  const r = await f.runner.call(callOpts);
  assert.ok(r.ok);
  if (r.ok) assert.deepEqual([r.sessionId, r.costUsd, (r.structured as { step: { action: string } }).step.action], ['abc', 0.07, 'refuse']);
});

test('claude 실행: 잘린 출력·init/result 중복·result 뒤 출력·종료 코드 불일치·빈 출력은 process 오류', async () => {
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

test('claude 실행: 결과 오류 분류 — 비용 상한, 제한·과부하, 재개 실패, 그 밖', async () => {
  assert.equal(errType(await fakeClaude([{ isError: true, subtype: 'error_max_budget_usd' }]).runner.call(callOpts)), 'budget');
  assert.equal(errType(await fakeClaude([{ isError: true, resultText: 'API Error: 529 Overloaded' }]).runner.call(callOpts)), 'rate_limit');
  assert.equal(errType(await fakeClaude([{ raw: [], stderr: 'No conversation found with session ID: x', exitCode: 1 }]).runner.call({ ...callOpts, sessionId: 'x' })), 'resume');
  assert.equal(errType(await fakeClaude([{ isError: true, subtype: 'error_during_execution' }]).runner.call(callOpts)), 'error');
});

test('격리 점검: 출력 전용 StructuredOutput 외의 도구나 MCP가 있으면 프로세스를 죽이고 isolation', async () => {
  assert.ok((await fakeClaude([{ structured: {}, tools: ['StructuredOutput'] }]).runner.call(callOpts)).ok);
  assert.equal(errType(await fakeClaude([{ structured: {}, tools: ['Bash'] }]).runner.call(callOpts)), 'isolation');
  assert.equal(errType(await fakeClaude([{ structured: {}, tools: ['StructuredOutput', 'Read'] }]).runner.call(callOpts)), 'isolation');
  assert.equal(errType(await fakeClaude([{ structured: {}, mcpServers: [{ name: 'x' }] }]).runner.call(callOpts)), 'isolation');
  const noField = { type: 'system', subtype: 'init', session_id: 's' };
  assert.equal(errType(await fakeClaude([{ events: [noField] }]).runner.call(callOpts)), 'isolation');
});

test('시간 초과·취소: 자식 프로세스까지 종료, 취소 중 도착한 result는 버림', async () => {
  const f = fakeClaude([{ structured: {}, hang: true, spawnChild: true }]);
  const r = await f.runner.call({ ...callOpts, timeoutMs: 400 });
  assert.equal(errType(r), 'timeout');
  await new Promise((res) => setTimeout(res, 100));
  for (const pid of f.children()) assert.equal(alive(pid), false, `자식 ${pid}가 살아 있음`);

  const g = fakeClaude([{ structured: { action: 'refuse', reason: 'r', alternatives: [] }, hang: true, spawnChild: true }]);
  const ac = new AbortController();
  const p = g.runner.call({ ...callOpts, signal: ac.signal });
  await new Promise((res) => setTimeout(res, 300));
  ac.abort();
  assert.equal(errType(await p), 'cancelled');
  await new Promise((res) => setTimeout(res, 100));
  for (const pid of g.children()) assert.equal(alive(pid), false);
});

test('claude 실행: 다바이트 글자가 바이트 조각 경계에서 끊겨도 정상 파싱', async () => {
  const f = fakeClaude([{ structured: { action: 'refuse', reason: '가입 주별 연결률은 그릴 수 없어요 🙏', alternatives: ['한글 대안'] }, chunkBytes: 5, delayMs: 1 }]);
  const r = await f.runner.call(callOpts);
  assert.ok(r.ok, JSON.stringify(r));
  if (r.ok) assert.equal((r.structured as { step: { reason: string } }).step.reason, '가입 주별 연결률은 그릴 수 없어요 🙏');
});

test('프로세스 정리: SIGTERM을 무시하는 자손도 결과 전에 죽인다(정상 종료·시간 초과 모두)', async () => {
  const ok = fakeClaude([{ structured: { action: 'refuse', reason: 'r', alternatives: [] }, spawnChild: true, childIgnoresTerm: true, chunk: 50, delayMs: 30 }]);
  const t0 = Date.now();
  assert.ok((await ok.runner.call(callOpts)).ok);
  assert.ok(Date.now() - t0 >= 2000, 'SIGKILL 단계까지 갔어야 함');
  for (const pid of ok.children()) assert.equal(alive(pid), false, `정상 종료 뒤 자손 ${pid}가 살아 있음`);
  const slow = fakeClaude([{ structured: {}, hang: true, spawnChild: true, childIgnoresTerm: true }]);
  assert.equal(errType(await slow.runner.call({ ...callOpts, timeoutMs: 300 })), 'timeout');
  for (const pid of slow.children()) assert.equal(alive(pid), false, `시간 초과 뒤 자손 ${pid}가 살아 있음`);
});

test('세션 ID: init과 result가 다르면 process, 재개 요청과 다른 세션이면 resume (오류 결과여도 먼저 검증)', async () => {
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
  definition: [['모집단', 'x']], caveats: [], answers: [],
};

test('행동: 네 가지 모두 파싱, null 역할 칸 허용', () => {
  assert.equal(pa({ action: 'ask', questions: [{ id: 'win', text: '기간?', options: [{ label: '7일', is_default: true }, { label: '14일', is_default: false }], allow_free_text: true }] }).action, 'ask');
  assert.equal(pa({ action: 'probe', plan: 'p', purpose: 'u', sql: 'SELECT 1' }).action, 'probe');
  const p = pa({ action: 'panel', plan: 'p', panel: panelRaw });
  assert.ok(p.action === 'panel' && p.panel.display.columns.numerator === 'numerator');
  assert.equal(pa({ action: 'refuse', reason: 'r', alternatives: ['a'] }).action, 'refuse');
  assert.equal(parseAction({ step: { action: 'refuse', reason: 'r', alternatives: [] } }).action, 'refuse');
  assert.throws(() => parseAction({ step: { action: 'refuse', reason: 'r', alternatives: [] }, extra: 1 }), ActionError);
  assert.throws(() => parseAction({ action: 'refuse', reason: 'r', alternatives: [] }), ActionError);
});

test('행동: 위반은 ActionError (기본값 하나, id 중복, 알 수 없는 키, 패널 사양 오류)', () => {
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
  ok: false, stage: 'invariant', message: '불변식 위반:\n- x=SECRET: numerator(11) > denominator(10)',
  violations: [{ row: 'x=SECRET', problem: 'numerator(11) > denominator(10)', index: 3, rule: 'numerator_le_denominator', column: 'numerator' }],
};

test('전송(pseudonymized): 탐색 결과 행(가명)과 남은 횟수, 패널 실패 메시지, 답변 기본값', () => {
  const o = new Outbound('pseudonymized', roles);
  const t = o.probeResult(probeOk, 2);
  assert.match(t, /1000000123/);
  assert.match(t, /남은 탐색 횟수: 2회/);
  assert.match(o.panelFailure(invFail, 1), /x=SECRET/);
  const a = o.answers([{ id: 'w', text: '기간', options: [{ label: '7일', is_default: true }], allow_free_text: true }], {});
  assert.match(a, /"answer": "7일"/);
  assert.match(a, /"defaulted": true/);
});

test('전송(schema_only): 어떤 경로에서도 행 값이 나가지 않는다', () => {
  const o = new Outbound('schema_only', roles);
  const outputs = [
    o.request('최근 가입자 보여줘', spec1, null),
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

test('schema_only 통계: identifier 칸은 최솟값·최댓값 없이 NULL 수만, 계산 칸은 최솟값·최댓값', () => {
  const s = columnStats(probeOk.ok ? probeOk.columns : [], probeOk.ok ? probeOk.rows : [], roles);
  assert.deepEqual(s, [
    { name: 'id', nulls: 0, min: null, max: null, identifier: true },
    { name: 'n', nulls: 1, min: 5, max: 5, identifier: false },
    { name: 'c', nulls: 0, min: 2, max: 9, identifier: false },
  ]);
});

test('세션 복구 요약: 최근 입력 5개(각 500자), 8,000자 상한(오래된 입력부터 뺌)', () => {
  const o = new Outbound('pseudonymized', roles);
  const s = o.recoverySummary({ inputs: ['1', '2', '3', '4', '5', '6'], lastAsk: null, current: null });
  assert.doesNotMatch(s, /"1"/);
  assert.match(s, /"6"/);
  const bigSpec = parsePanelSpec({ ...panelClean, sql: `SELECT 1 /* ${'s'.repeat(5_500)} */` });
  const long = o.recoverySummary({ inputs: Array.from({ length: 5 }, (_, i) => `${i}`.repeat(3000)), lastAsk: null, current: bigSpec });
  assert.ok(long.length <= 8000, String(long.length));
  assert.ok(!long.includes('0'.repeat(500)), '가장 오래된 입력이 빠져야 함');
  assert.ok(long.includes('4'.repeat(500)), '가장 최근 입력은 남아야 함');
  const hugeSpec = parsePanelSpec({ ...panelClean, sql: `SELECT 1 /* ${'s'.repeat(11_900)} */` });
  const shrunk = o.recoverySummary({ inputs: ['x'], lastAsk: null, current: hugeSpec });
  assert.ok(shrunk.length <= 8000, String(shrunk.length));
  const block = /현재 미리보기 패널 사양:\n```json\n([\s\S]*?)\n```/.exec(shrunk);
  assert.ok(block, '패널 사양 블록이 있어야 함');
  const parsed = JSON.parse(block![1]);
  assert.match(parsed.sql, /…\(길어서 생략\)$/);
  const qs = Array.from({ length: 4 }, (_, i) => ({ id: `q${i}`, text: '질문'.repeat(90), options: [{ label: '기본', is_default: true }], allow_free_text: true }));
  const answers = Object.fromEntries(qs.map((q) => [q.id, '답'.repeat(5000)]));
  const withAsk = o.recoverySummary({ inputs: Array.from({ length: 5 }, () => 'y'.repeat(3000)), lastAsk: { questions: qs, answers }, current: hugeSpec });
  assert.ok(withAsk.length <= 8000, String(withAsk.length));
  for (const m of withAsk.matchAll(/```json\n([\s\S]*?)\n```/g)) JSON.parse(m[1]);
});


test('컨텍스트: 칸 설명 찾기, 60,000자 넘으면 시드 SQL → 시드 패널 순으로 뒤에서부터 뺀다', () => {
  const d = columnDescriptions('| `d_x.a` | 첫째 |\n- `d_x.b`: 둘째\n그 밖의 줄');
  assert.deepEqual([...d], [['d_x.a', '첫째'], ['d_x.b', '둘째']]);
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

const start = { text: '가입 주별 연결률 보여줘', current: null, resumeSessionId: null, recovery: null };
const types = (h: Harness) => h.events.map((e) => e.type).filter((t) => t !== 'step');
const ask = { action: 'ask', questions: [{ id: 'win', text: '기간?', options: [{ label: '7일', is_default: true }, { label: '14일', is_default: false }], allow_free_text: false }] };
const probe = { action: 'probe', plan: '인원 확인', purpose: '코호트 인원', sql: 'SELECT count(*) FROM d_x' };
const panel = { action: 'panel', plan: '셌음', panel: panelRaw };

test('루프: ask → 답 → probe → panel 실패 → 수정 → 성공', async () => {
  const h = harness([{ structured: ask }, { structured: probe }, { structured: panel }, { structured: panel }],
    {}, [{ ok: false, stage: 'invariant', message: '불변식 위반', violations: [] }]);
  await h.req.run(start);
  assert.equal(h.req.state, 'waiting_user');
  const qTurn = h.events.find((e) => e.type === 'question')!.turnNo;
  assert.equal(h.req.answer(qTurn + 1, { win: '14일' }), null);
  await h.req.answer(qTurn, { win: '14일' });
  assert.equal(h.req.state, 'done');
  assert.deepEqual(types(h), ['request_started', 'session', 'question', 'answers', 'plan', 'plan', 'plan', 'preview', 'done']);
  assert.deepEqual(h.req.counts, { turns: 4, probes: 1, fixes: 1 });
  assert.equal(h.probes.length, 1);
  assert.equal(h.panels.length, 2);
  const argv = h.argv();
  assert.ok(!argv[0].includes('--resume'));
  for (const a of argv.slice(1)) assert.equal(a[a.indexOf('--resume') + 1], 'sess-1');
  assert.match(argv[2][1], /탐색 쿼리 결과: 2행/);
});

test('루프: 사전 밖 패널은 승인을 기다리고, 승인하면 미리보기, 거절하면 사전 지표로 다시 만들게 한다', async () => {
  const off = { action: 'panel', plan: '셌음', panel: { ...panelRaw, metric: null } };
  const a = harness([{ structured: off }]);
  await a.req.run(start);
  assert.equal(a.req.state, 'waiting_user');
  const ev = a.events.find((e) => e.type === 'offdict') as LoopEvent & { turnNo: number; tables: string[] };
  assert.deepEqual(ev.tables, ['d_x']);
  assert.equal(a.req.approveOffdict(ev.turnNo + 1, true), null);
  await a.req.approveOffdict(ev.turnNo, true);
  assert.equal(a.req.state, 'done');
  assert.deepEqual(types(a).slice(-4), ['offdict', 'offdict_answer', 'preview', 'done']);
  assert.equal(a.req.approveOffdict(ev.turnNo, true), null, '두 번 승인할 수 없음');

  const b = harness([{ structured: off }, { structured: panel }]);
  await b.req.run(start);
  const t = (b.events.find((e) => e.type === 'offdict')!).turnNo;
  await b.req.approveOffdict(t, false);
  assert.equal(b.req.state, 'done');
  assert.deepEqual(types(b).slice(-5), ['offdict', 'offdict_answer', 'plan', 'preview', 'done']);
  assert.match(b.argv()[1][1], /사전에 없는 정의로 만든 패널을 받지 않았습니다/);
  assert.equal(b.req.counts.fixes, 0);

  const c = harness([{ structured: off }]);
  await c.req.run(start);
  await c.req.cancel();
  assert.equal(c.req.state, 'cancelled');
});

test('행동: panel에 metric이 없으면 스키마 불일치', () => {
  const { metric: _m, ...noMetric } = panelRaw;
  assert.throws(() => pa({ action: 'panel', plan: 'p', panel: noMetric }), /metric: 필수/);
});

test('루프: 예산 — 호출 상한은 min(호출 상한, 요청 상한 − 누적), 남은 예산이 0.02 미만이면 호출하지 않음', async () => {
  const h = harness([{ structured: probe, cost: 0.6 }, { structured: probe, cost: 0.385 }, { structured: panel }]);
  await h.req.run(start);
  assert.equal(h.req.state, 'failed');
  const budgets = h.argv().map((a) => Number(a[a.indexOf('--max-budget-usd') + 1]));
  assert.deepEqual(budgets, [0.5, 0.4]);
  assert.equal(h.events.at(-1)!.type === 'failed' && (h.events.at(-1) as { reason: string }).reason, 'request_budget');
});

test('루프: 상한 — 턴, 탐색, 수정', async () => {
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

test('루프: 수정 횟수는 실제 다음 호출을 시작할 때 센다(턴 한도로 막히면 세지 않음)', async () => {
  const bad: PanelRunResult = { ok: false, stage: 'exec', message: 'no such column' };
  const h = harness([{ structured: panel }], { limits: { maxTurns: 1, maxProbes: 4, maxFixes: 2, callBudgetUsd: 0.5, requestBudgetUsd: 1 } }, [bad]);
  await h.req.run(start);
  assert.equal((h.events.at(-1) as { reason?: string }).reason, 'turn_limit');
  assert.equal(h.req.counts.fixes, 0);
});

test('루프: 재시도할 수 있는 결과 오류가 준 세션 ID로 재시도를 이어간다', async () => {
  const h = harness([{ isError: true, resultText: 'overloaded', sessionId: 's-first' }, { structured: panel, sessionId: 's-first' }]);
  await h.req.run(start);
  assert.equal(h.req.state, 'done');
  const second = h.argv()[1];
  assert.equal(second[second.indexOf('--resume') + 1], 's-first');
});

test('루프: 스키마 불일치는 오류 문구를 붙여 같은 세션으로 1회 재요청, 두 번이면 failed', async () => {
  const once = harness([{ structured: { action: 'nope' } }, { structured: panel }]);
  await once.req.run(start);
  assert.equal(once.req.state, 'done');
  assert.match(once.argv()[1][1], /행동 스키마에 맞지 않습니다/);
  assert.equal(once.argv()[1][once.argv()[1].indexOf('--resume') + 1], 'sess-1');
  const twice = harness([{ structured: { action: 'nope' } }, { structured: { action: 'nope' } }]);
  await twice.req.run(start);
  assert.equal((twice.events.at(-1) as { reason?: string }).reason, 'schema');
});

test('루프: 재시도 — 프로세스 오류 1회, 제한·과부하는 대기 후 2회, 비용 상한은 재시도 없이 failed', async () => {
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

test('루프: 격리 실패면 에이전트 기능을 끄고 failed', async () => {
  const h = harness([{ structured: panel, tools: ['Bash'] }]);
  await h.req.run(start);
  assert.equal((h.events.at(-1) as { reason?: string }).reason, 'isolation');
  assert.ok(h.disabled());
});

test('루프: 컨텍스트가 같으면 재개, 다르면 새 세션 + 이전 대화 요약, 재개 실패도 요약으로 복구', async () => {
  const resume = harness([{ structured: panel, sessionId: 'old' }]);
  await resume.req.run({ ...start, resumeSessionId: 'old', recovery: '요약' });
  assert.equal(resume.req.state, 'done');
  assert.equal(resume.argv()[0][resume.argv()[0].indexOf('--resume') + 1], 'old');
  assert.doesNotMatch(resume.argv()[0][1], /이전 대화 요약/);

  const fresh = harness([{ structured: panel }]);
  await fresh.req.run({ ...start, resumeSessionId: null, recovery: '요약 내용' });
  assert.ok(!fresh.argv()[0].includes('--resume'));
  assert.match(fresh.argv()[0][1], /이전 대화 요약\(새 세션으로 이어감\):\n요약 내용/);

  const broken = harness([{ raw: [], stderr: 'No conversation found with session ID: old', exitCode: 1 }, { structured: panel }]);
  await broken.req.run({ ...start, resumeSessionId: 'old', recovery: '요약 내용' });
  assert.equal(broken.req.state, 'done');
  assert.ok(!broken.argv()[1].includes('--resume'));
  assert.match(broken.argv()[1][1], /요약 내용/);
});

test('루프: 취소 — 호출 중·탐색 중·답 대기 중, 프로세스 종료 확인 뒤 cancelled', async () => {
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
      release = () => res({ ok: false, kind: 'cancelled', message: '취소됨' });
      if (signal.aborted) release();
      else signal.addEventListener('abort', () => release());
    }),
  });
  const q = querying.req.run(start);
  await new Promise((r) => setTimeout(r, 400));
  assert.equal(querying.req.state, 'querying');
  assert.ok(querying.events.some((e) => e.type === 'step' && e.kind === 'querying' && /초\)/.test(e.text)));
  await querying.req.cancel();
  await q;
  assert.equal(querying.req.state, 'cancelled');

  const waiting = harness([{ structured: ask }]);
  await waiting.req.run(start);
  await waiting.req.cancel();
  assert.equal(waiting.req.state, 'cancelled');
  assert.equal(waiting.req.answer(1, {}), null);
});

test('루프: 행동을 받은 순간 취소되면 탐색·패널을 시작하지 않는다', async () => {
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

test('루프: schema_only에서 전송 경로에 행이 섞이면 outbound_blocked로 끝난다', async () => {
  const blocked = new Outbound('schema_only', roles);
  blocked.probeResult = () => render('schema_only', [{ kind: 'rows', target: 'agent', columns: ['a'], rows: [], more: false, truncatedCells: 0 }]);
  const h = harness([{ structured: probe }], { outbound: blocked });
  await h.req.run(start);
  assert.equal((h.events.at(-1) as { reason?: string }).reason, 'outbound_blocked');
});
