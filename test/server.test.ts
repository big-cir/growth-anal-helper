// Server: request checks, routing, SSE, conversation flow.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, mkdtempSync, writeFileSync } from 'node:fs';
import { request } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { App } from '../src/server/app.ts';
import { startServer, type ServerHandle } from '../src/server/http.ts';
import { buildDemoSnapshot } from './helpers/demo-snapshot.ts';
import { Conversation } from '../src/server/conversations.ts';
import { login, makeAccount } from './helpers/auth.ts';

const FAKE = join(import.meta.dirname, 'fake-claude.ts');

let h: ServerHandle;
let app: App;
let base: string;
let fakeDir: string;
let cookie = '';

const panelAction = {
  action: 'panel', plan: 'Counted participation by signup week',
  panel: {
    metric: 'first_week_activation',
    title: 'Board participation by signup week', question: 'What share joined a board within 7 days, by signup week?',
    sql: "SELECT signup_week AS x, sum(board_state = 'reached') AS numerator, count(*) AS denominator FROM d_member_first_week GROUP BY 1 ORDER BY 1",
    display: { type: 'line', x: 'x', numerator: 'numerator', denominator: 'denominator', series: null, headline: null },
    definition: [['Population', 'Members 7+ days after signup']], caveats: [], answers: [{ question: 'Window', answer: '7 days', defaulted: true }],
  },
};
const ask = { action: 'ask', questions: [{ id: 'win', text: 'Window?', options: [{ label: '7 days', is_default: true }, { label: '14 days', is_default: false }], allow_free_text: true }] };
const probe = { action: 'probe', plan: 'Check counts', purpose: 'Members per signup week', sql: 'SELECT signup_week, count(*) AS n FROM d_member_first_week GROUP BY 1' };

function setScript(steps: unknown[]): void {
  writeFileSync(join(fakeDir, 'script.json'), JSON.stringify(steps));
  writeFileSync(join(fakeDir, 'state.json'), JSON.stringify({ calls: 0 }));
}

before(async () => {
  const demo = await buildDemoSnapshot();
  fakeDir = mkdtempSync(join(tmpdir(), 'gl-srv-'));
  const bin = join(fakeDir, 'claude');
  writeFileSync(bin, `#!/bin/sh\nFAKE_CLAUDE_SCRIPT="${join(fakeDir, 'script.json')}" FAKE_CLAUDE_DIR="${fakeDir}" exec "${process.execPath}" "${FAKE}" "$@"\n`);
  chmodSync(bin, 0o755);
  process.env.FAKE_CLAUDE_SCRIPT = join(fakeDir, 'script.json');
  process.env.FAKE_CLAUDE_DIR = fakeDir;
  setScript([{ structured: { action: 'refuse', reason: 'ok', alternatives: [] } }]);
  demo.ws.config.agent.bin = bin;
  demo.ws.config.agent.callTimeoutMs = 10_000;
  await makeAccount(demo.ws.config.outDir, 'editor1', 'editor');
  app = new App(demo.ws);
  h = await startServer(app, 0);
  base = `http://127.0.0.1:${h.port}`;
  cookie = await login(base, 'editor1');
  await app.startIsolationCheck();
  assert.equal(app.agent.state, 'ok');
});
after(async () => {
  await h.close();
});

type Res = { status: number; body: any };
function raw(method: string, path: string, o: { headers?: Record<string, string>; body?: string } = {}): Promise<Res> {
  return new Promise((resolve, reject) => {
    const r = request(`${base}${path}`, { method, headers: { Host: `127.0.0.1:${h.port}`, Cookie: cookie, ...o.headers } }, (res) => {
      let t = '';
      res.on('data', (c) => { t += c; });
      res.on('end', () => {
        let body: unknown = t;
        try { body = JSON.parse(t); } catch { /* plain string */ }
        resolve({ status: res.statusCode!, body });
      });
    });
    r.on('error', reject);
    if (o.body !== undefined) r.write(o.body);
    r.end();
  });
}
const okHeaders = () => ({ Origin: `http://127.0.0.1:${h.port}`, 'X-Growth-Lab': '1', 'Content-Type': 'application/json' });
const post = (path: string, body: unknown) => raw('POST', path, { headers: okHeaders(), body: JSON.stringify(body) });

function sse(path: string, headers: Record<string, string> = {}) {
  const events: any[] = [];
  const ac = new AbortController();
  let buf = '';
  const done = fetch(`${base}${path}`, { headers: { Cookie: cookie, ...headers }, signal: ac.signal }).then(async (res) => {
    const reader = res.body!.getReader();
    const dec = new TextDecoder();
    for (;;) {
      const { value, done: end } = await reader.read();
      if (end) break;
      buf += dec.decode(value, { stream: true });
      let i: number;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const block = buf.slice(0, i);
        buf = buf.slice(i + 2);
        const ev = /^event: (.+)$/m.exec(block)?.[1];
        const data = /^data: (.+)$/m.exec(block)?.[1];
        if (ev && data) events.push({ type: ev, ...JSON.parse(data) });
      }
    }
  }).catch(() => {});
  return {
    events,
    close: () => { ac.abort(); return done; },
    async until(pred: (e: any) => boolean, ms = 15_000) {
      const t0 = Date.now();
      while (Date.now() - t0 < ms) {
        const hit = events.find(pred);
        if (hit) return hit;
        await new Promise((r) => setTimeout(r, 30));
      }
      throw new Error(`timed out waiting for events: ${JSON.stringify(events.map((e) => e.type))}`);
    },
  };
}


test('403 for another Host', async () => {
  assert.equal((await raw('GET', '/api/state', { headers: { Host: `evil.example:${h.port}` } })).status, 403);
  assert.equal((await raw('GET', '/', { headers: { Host: 'localhost:1' } })).status, 403);
  assert.equal((await raw('GET', '/api/state', { headers: { Host: `localhost:${h.port}` } })).status, 200);
});

test('writes: 403 for another Origin or without the custom header', async () => {
  const body = '{}';
  assert.equal((await raw('POST', '/api/conversations', { headers: { ...okHeaders(), Origin: 'http://evil.example' }, body })).status, 403);
  assert.equal((await raw('POST', '/api/conversations', { headers: { 'Content-Type': 'application/json', Origin: `http://127.0.0.1:${h.port}` }, body })).status, 403);
  const noOrigin = { ...okHeaders() } as Record<string, string>;
  delete noOrigin.Origin;
  assert.equal((await raw('POST', '/api/conversations', { headers: noOrigin, body })).status, 403);
  assert.equal((await post('/api/conversations', {})).status, 201);
});

test('routing: bad or unknown path 404, bad ID 400, method 405, size 413, type 415', async () => {
  assert.equal((await raw('GET', '/api/conversations/%E0%A4%A')).status, 400);
  for (const p of ['/../package.json', '/..%2fpackage.json', '/%2e%2e/src/cli.ts', '/web/app.js', '/api/nope', '/api/conversations/aaaaaaaaaaaa/nope']) {
    assert.equal((await raw('GET', p)).status, 404, p);
  }
  assert.equal((await raw('GET', '/api/conversations/BAD')).status, 400);
  assert.equal((await raw('GET', '/api/conversations/aaaaaaaaaaaa')).status, 404);
  assert.equal((await raw('DELETE', '/api/conversations', { headers: okHeaders() })).status, 405);
  assert.equal((await raw('POST', '/app.js', { headers: okHeaders(), body: '{}' })).status, 405);
  const c = (await post('/api/conversations', {})).body.conversation_id;
  assert.equal((await raw('POST', `/api/conversations/${c}/messages`, { headers: okHeaders(), body: JSON.stringify({ text: 'x'.repeat(70_000) }) })).status, 413);
  assert.equal((await raw('POST', `/api/conversations/${c}/messages`, { headers: { ...okHeaders(), 'Content-Type': 'text/plain' }, body: '{}' })).status, 415);
  assert.equal((await post(`/api/conversations/${c}/messages`, { text: 'x'.repeat(2001) })).status, 400);
  assert.equal((await post(`/api/conversations/${c}/messages`, {})).status, 400);
});

test('static files: screen files and response headers', async () => {
  const res = await fetch(`${base}/`);
  assert.equal((await fetch(`${base}/package.json`)).status, 404);
  assert.equal(res.status, 200);
  assert.match(res.headers.get('content-security-policy') ?? '', /default-src 'self'/);
  const html = await res.text();
  assert.match(html, /<script type="module" src="\/app.js">/);
  assert.doesNotMatch(html, /https?:\/\//);
  for (const f of ['/app.js', '/charts.js', '/styles.css']) {
    const t = await (await fetch(`${base}${f}`)).text();
    assert.doesNotMatch(t, /https?:\/\/(?!127\.0\.0\.1)/, f);
  }
});


test('flow: question → answer → probe → panel → preview (SSE order, request_id and turn_no)', async () => {
  setScript([{ structured: ask }, { structured: probe }, { structured: panelAction }]);
  const c = (await post('/api/conversations', {})).body.conversation_id;
  const s = sse(`/api/conversations/${c}/events?since=0`);
  const r = await post(`/api/conversations/${c}/messages`, { text: 'show participation by signup week' });
  assert.equal(r.status, 202);
  const q = await s.until((e) => e.type === 'question');
  assert.equal(q.request_id, r.body.request_id);
  assert.equal((await post(`/api/conversations/${c}/answers`, { request_id: q.request_id, turn_no: q.turn_no + 1, answers: {} })).status, 409);
  assert.equal((await post(`/api/conversations/${c}/answers`, { request_id: q.request_id, turn_no: q.turn_no, answers: { win: '14 days' } })).status, 202);
  const pv = await s.until((e) => e.type === 'preview');
  await s.until((e) => e.type === 'done');
  const p = pv.data.preview;
  assert.equal(p.spec.title, 'Board participation by signup week');
  assert.ok(p.rows.length > 5);
  assert.match(p.preview_hash, /^[0-9a-f]{64}$/);
  assert.ok(p.headline && typeof p.headline.value === 'number');
  const types = s.events.map((e) => e.type).filter((t) => t !== 'step');
  assert.deepEqual(types, ['user_input', 'request_started', 'question', 'answers', 'plan', 'plan', 'preview', 'done']);
  assert.ok(s.events.every((e) => e.conversation_id === c && Number.isInteger(e.event_id)));
  assert.ok(s.events.some((e) => e.type === 'step' && e.data.kind === 'query_done'));
  assert.ok(!JSON.stringify(s.events).includes('sess-1'));
  const st = (await raw('GET', `/api/conversations/${c}`)).body;
  assert.equal(st.preview.preview_hash, p.preview_hash);
  await s.close();
});

test('SSE reconnect: only events after Last-Event-ID are resent', async () => {
  setScript([{ structured: panelAction }]);
  const c = (await post('/api/conversations', {})).body.conversation_id;
  const s = sse(`/api/conversations/${c}/events?since=0`);
  await post(`/api/conversations/${c}/messages`, { text: 'participation' });
  await s.until((e) => e.type === 'done');
  await s.close();
  const mid = s.events[1].event_id;
  const again = sse(`/api/conversations/${c}/events`, { 'Last-Event-ID': String(mid) });
  await again.until((e) => e.type === 'done');
  assert.deepEqual(again.events.map((e) => e.event_id), s.events.filter((e) => e.event_id > mid).map((e) => e.event_id));
  await again.close();
  const none = sse(`/api/conversations/${c}/events`);
  await new Promise((r) => setTimeout(r, 300));
  assert.equal(none.events.length, 0);
  await none.close();
});

test('new input in the same conversation: cancel the previous request (confirmed) before starting', async () => {
  setScript([{ structured: panelAction, hang: true }, { structured: panelAction }]);
  const c = (await post('/api/conversations', {})).body.conversation_id;
  const s = sse(`/api/conversations/${c}/events?since=0`);
  const first = (await post(`/api/conversations/${c}/messages`, { text: 'first request' })).body.request_id;
  await s.until((e) => e.type === 'step' && e.request_id === first);
  await new Promise((r) => setTimeout(r, 300));
  const second = (await post(`/api/conversations/${c}/messages`, { text: 'second' })).body.request_id;
  const cancelled = await s.until((e) => e.type === 'cancelled');
  assert.equal(cancelled.request_id, first);
  const done = await s.until((e) => e.type === 'done');
  assert.equal(done.request_id, second);
  const order = s.events.filter((e) => ['user_input', 'cancelled', 'done'].includes(e.type)).map((e) => `${e.type}:${e.request_id === first ? 1 : 2}`);
  assert.deepEqual(order, ['user_input:1', 'cancelled:1', 'user_input:2', 'done:2']);
  await s.close();
});

test('[Stop]: stops the running request, cancelled', async () => {
  setScript([{ structured: panelAction, hang: true }]);
  const c = (await post('/api/conversations', {})).body.conversation_id;
  const s = sse(`/api/conversations/${c}/events?since=0`);
  await post(`/api/conversations/${c}/messages`, { text: 'slow request' });
  await s.until((e) => e.type === 'step');
  const r = await post(`/api/conversations/${c}/stop`, {});
  assert.deepEqual(r.body, { stopped: true });
  await s.until((e) => e.type === 'cancelled');
  assert.deepEqual((await post(`/api/conversations/${c}/stop`, {})).body, { stopped: false });
  await s.close();
});

test('SSE reconnect outside the buffer (500) resyncs, inside resends later events only', () => {
  const c = new Conversation({} as App, 'aaaaaaaaaaaa', mkdtempSync(join(tmpdir(), 'gl-cv-')));
  const push = (c as unknown as { push: (t: string, r: null, n: number, d: object) => void }).push.bind(c);
  for (let i = 0; i < 600; i++) push('step', null, 0, { i });
  assert.equal(c.subscribe(() => {}, 10).replay, 'resync');
  const r = c.subscribe(() => {}, 595).replay;
  assert.ok(Array.isArray(r) && r.map((e) => e.event_id).join(',') === '596,597,598,599,600');
  assert.deepEqual(c.subscribe(() => {}, null).replay, []);
});

test('inputs arriving during the isolation check: only the last runs, earlier ones are recorded as cancelled', async () => {
  setScript([{ structured: panelAction }]);
  let release: () => void = () => {};
  const gate = new Promise<void>((r) => { release = r; });
  const original = app.waitAgent.bind(app);
  app.waitAgent = () => gate;
  try {
    const c = (await post('/api/conversations', {})).body.conversation_id;
    const s = sse(`/api/conversations/${c}/events?since=0`);
    const a = (await post(`/api/conversations/${c}/messages`, { text: 'one' })).body.request_id;
    const b = (await post(`/api/conversations/${c}/messages`, { text: 'two' })).body.request_id;
    const third = (await post(`/api/conversations/${c}/messages`, { text: 'three' })).body.request_id;
    release();
    const done = await s.until((e) => e.type === 'done' || e.type === 'failed');
    assert.equal(done.request_id, third);
    const order = s.events.filter((e) => ['user_input', 'cancelled', 'done', 'request_started'].includes(e.type)).map((e) => `${e.type}:${e.request_id === a ? 'a' : e.request_id === b ? 'b' : 'c'}`);
    assert.deepEqual(order, ['user_input:a', 'cancelled:a', 'user_input:c', 'request_started:c', 'done:c']);
    await s.close();
    const { readFileSync } = await import('node:fs');
    const log = readFileSync(join(app.ws.config.outDir, 'conversations', `${c}.jsonl`), 'utf8');
    assert.match(log, new RegExp(`"type":"cancelled","request_id":"${a}"`));
  } finally {
    app.waitAgent = original;
  }
});

test('off-dictionary panel: approval card → preview after approval, bad requests 409', async () => {
  setScript([{ structured: { ...panelAction, panel: { ...panelAction.panel, metric: null } } }]);
  const c = (await post('/api/conversations', {})).body.conversation_id;
  const s = sse(`/api/conversations/${c}/events?since=0`);
  await post(`/api/conversations/${c}/messages`, { text: 'a metric not in the dictionary' });
  const ev = await s.until((e) => e.type === 'offdict');
  assert.deepEqual(ev.data.tables, ['d_member_first_week']);
  assert.equal(ev.data.rows, undefined, 'no results before approval');
  assert.match(ev.data.caveats[0], /definition outside the metric dictionary/);
  const st = (await raw('GET', `/api/conversations/${c}`)).body;
  assert.equal(st.offdict.turn_no, ev.turn_no);
  assert.equal((await post(`/api/conversations/${c}/offdict`, { request_id: ev.request_id, turn_no: ev.turn_no, approve: 'yes' })).status, 400);
  assert.equal((await post(`/api/conversations/${c}/offdict`, { request_id: ev.request_id, turn_no: ev.turn_no + 1, approve: true })).status, 409);
  assert.equal((await post(`/api/conversations/${c}/offdict`, { request_id: ev.request_id, turn_no: ev.turn_no, approve: true })).status, 202);
  const pv = await s.until((e) => e.type === 'preview');
  assert.deepEqual(pv.data.preview.tables, ['d_member_first_week']);
  assert.equal(pv.data.preview.spec.metric, null);
  await s.until((e) => e.type === 'done');
  await s.close();
  const { readFileSync } = await import('node:fs');
  assert.match(readFileSync(join(app.ws.config.outDir, 'conversations', `${c}.jsonl`), 'utf8'), /"type":"offdict_approved"/);
});

test('a request stopped by a restart stays failed in the conversation state', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-rs-'));
  writeFileSync(join(dir, 'bbbbbbbbbbbb.jsonl'), JSON.stringify({ type: 'request_started', request_id: 'cccccccccccc' }) + '\n');
  const c = new Conversation({} as App, 'bbbbbbbbbbbb', dir);
  c.load();
  assert.deepEqual(c.state().failed, { request_id: 'cccccccccccc', reason: 'server_restart', message: 'The server restarted, so the running request was stopped' });
});
