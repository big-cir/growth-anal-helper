// Save, dashboard, recompute and quality check APIs.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, cpSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { App } from '../src/server/app.ts';
import { startServer, type ServerHandle } from '../src/server/http.ts';
import { buildDemoSnapshot, DEMO_DIR } from './helpers/demo-snapshot.ts';
import { PanelStore } from '../src/panels/store.ts';
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

function setScript(steps: unknown[]): void {
  writeFileSync(join(fakeDir, 'script.json'), JSON.stringify(steps));
  writeFileSync(join(fakeDir, 'state.json'), JSON.stringify({ calls: 0 }));
}

before(async () => {
  const demo = await buildDemoSnapshot();
  cpSync(join(DEMO_DIR, 'quality'), join(demo.ws.dir, 'quality'), { recursive: true });
  fakeDir = mkdtempSync(join(tmpdir(), 'gl-dash-'));
  const bin = join(fakeDir, 'claude');
  writeFileSync(bin, `#!/bin/sh\nFAKE_CLAUDE_SCRIPT="${join(fakeDir, 'script.json')}" FAKE_CLAUDE_DIR="${fakeDir}" exec "${process.execPath}" "${FAKE}" "$@"\n`);
  chmodSync(bin, 0o755);
  process.env.FAKE_CLAUDE_SCRIPT = join(fakeDir, 'script.json');
  process.env.FAKE_CLAUDE_DIR = fakeDir;
  setScript([{ structured: { action: 'refuse', reason: 'ok', alternatives: [] } }]);
  demo.ws.config.agent.bin = bin;
  demo.ws.config.agent.callTimeoutMs = 10_000;
  await makeAccount(demo.ws.config.outDir, 'admin1', 'admin');
  app = new App(demo.ws);
  h = await startServer(app, 0);
  base = `http://127.0.0.1:${h.port}`;
  cookie = await login(base, 'admin1');
  await app.startIsolationCheck();
});
after(async () => {
  await h.close();
});

const headers = () => ({ Origin: base, 'X-Growth-Lab': '1', 'Content-Type': 'application/json', Cookie: cookie });
async function call(method: string, path: string, body?: unknown): Promise<{ status: number; body: any }> {
  const res = await fetch(`${base}${path}`, { method, headers: method === 'GET' ? { Cookie: cookie } : headers(), body: body === undefined ? undefined : JSON.stringify(body) });
  return { status: res.status, body: await res.json().catch(() => null) };
}
const get = (p: string) => call('GET', p);
const post = (p: string, b: unknown = {}) => call('POST', p, b);

function sse(path: string) {
  const events: any[] = [];
  const ac = new AbortController();
  let buf = '';
  const done = fetch(`${base}${path}`, { headers: { Cookie: cookie }, signal: ac.signal }).then(async (res) => {
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

async function waitFor<T>(fn: () => Promise<T | null | undefined | false>, ms = 15_000): Promise<T> {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) {
    const v = await fn();
    if (v) return v;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error('timed out waiting for the condition');
}

/** A conversation that reached a preview */
async function makePreview(): Promise<{ conv: string; preview: any }> {
  setScript([{ structured: panelAction }]);
  const conv = (await post('/api/conversations')).body.conversation_id;
  const s = sse(`/api/conversations/${conv}/events?since=0`);
  await post(`/api/conversations/${conv}/messages`, { text: 'participation by signup week' });
  const pv = await s.until((e) => e.type === 'preview');
  await s.until((e) => e.type === 'done');
  await s.close();
  return { conv, preview: pv.data.preview };
}

function summaryFor(preview: any) {
  const last = preview.rows.at(-1);
  const rate = ((last[1] / last[2]) * 100).toFixed(1);
  return {
    rawStructured: true,
    structured: {
      prose: `The latest signup week (${last[0]}) has a participation rate of {c1}.`,
      claims: [{ id: 'c1', op: 'rate', refs: [{ row: [{ column: 'x', value: last[0] }], column: 'numerator' }, { row: [{ column: 'x', value: last[0] }], column: 'denominator' }], display: `${rate}%` }],
    },
    expected: `The latest signup week (${last[0]}) has a participation rate of ${rate}%.`,
  };
}

test('save: save-draft fills the description; save, duplicate save, list, detail, delete', async () => {
  const { conv, preview } = await makePreview();
  const sum = summaryFor(preview);
  setScript([{ structured: { prose: 'The participation rate is 99%.', claims: [] }, rawStructured: true }, sum]);
  const draftReq = { request_id: preview.request_id, preview_hash: preview.preview_hash };
  assert.equal((await post(`/api/conversations/${conv}/save-draft`, { ...draftReq, preview_hash: 'f'.repeat(64) })).status, 409);
  const d = await post(`/api/conversations/${conv}/save-draft`, draftReq);
  assert.equal(d.status, 200);
  assert.equal(d.body.summary_status, 'ok');
  assert.equal(d.body.summary, sum.expected);
  assert.equal(d.body.title, 'Board participation by signup week');
  const argv = readFileSync(join(fakeDir, 'argv.jsonl'), 'utf8').trim().split('\n').slice(-2).map((l) => JSON.parse(l));
  assert.ok(!argv[0].includes('--resume'), 'description uses a new session');
  assert.ok(argv[1].includes('--resume'), 'retry uses the same description session');
  assert.match(argv[0][argv[0].indexOf('-p') + 1], /pseudonymous ID/);

  const body = { conversation_id: conv, ...draftReq, title: 'Participation', description: 'Short description', summary: d.body.summary };
  const s = sse('/api/panels/events');
  await s.until((e) => e.type === 'panel_status_all');
  const c1 = await post('/api/panels', body);
  assert.equal(c1.status, 201);
  const id = c1.body.panel.id;
  assert.equal(c1.body.panel.status, 'ok');
  assert.equal(c1.body.panel.prompt, 'participation by signup week');
  assert.equal(c1.body.panel.summary_snapshot_id, preview.snapshot_id);
  assert.equal(c1.body.panel.last_result.mode, 'preview');
  assert.deepEqual(c1.body.panel.last_result.rows, preview.rows);
  await s.until((e) => e.type === 'panels_changed');
  const c2 = await post('/api/panels', body);
  assert.equal(c2.status, 200);
  assert.equal(c2.body.panel.id, id);

  const list = await get('/api/panels');
  assert.equal(list.body.panels.filter((p: any) => p.id === id).length, 1);
  assert.equal((await get('/api/state')).body.dashboard_count >= 1, true);
  const one = await get(`/api/panels/${id}`);
  assert.equal(one.body.panel.job_status, 'idle');
  assert.deepEqual(one.body.panel.changed_rules, []);
  assert.equal((await get('/api/panels/aaaaaaaaaaaa')).status, 404);
  assert.equal((await post('/api/panels', { ...body, title: 'x'.repeat(61) })).status, 400);

  assert.equal((await call('DELETE', `/api/panels/${id}`)).status, 200);
  assert.equal((await get(`/api/panels/${id}`)).status, 404);
  await s.close();
});

test('recompute: rule change → review → [recompute with these rules]; snapshot-only change → automatic; stop', async () => {
  const { conv, preview } = await makePreview();
  const created = await post('/api/panels', { conversation_id: conv, request_id: preview.request_id, preview_hash: preview.preview_hash, title: 'Participation', description: '', summary: '' });
  const id = created.body.panel.id;
  const store = new PanelStore(join(app.ws.config.outDir, 'panels'));
  const s = sse('/api/panels/events');
  const panel = async () => (await get(`/api/panels/${id}`)).body.panel;

  const p = store.get(id)!;
  store.write({ ...p, versions: { ...p.versions, docs_version: 'old' } });
  assert.equal((await post(`/api/panels/${id}/recompute`)).status, 409, 'no manual recompute in ok state');
  h.panels.scan();
  await s.until((e) => e.type === 'panel_status' && e.panel_id === id && e.status === 'review');
  assert.deepEqual((await panel()).changed_rules, ['docs_version']);
  assert.equal((await post(`/api/panels/${id}/recompute`)).status, 202);
  const manual = await waitFor(async () => { const x = await panel(); return x.status === 'ok' && x.job_status === 'idle' && x.last_result.mode === 'manual_rule' && x; });
  assert.deepEqual(manual.changed_rules, []);

  const q = store.get(id)!;
  store.write({ ...q, last_result: { ...q.last_result, snapshot_id: 'old-snapshot' } });
  h.panels.scan();
  const auto = await waitFor(async () => { const x = await panel(); return x.job_status === 'idle' && x.last_result.mode === 'auto' && x; });
  assert.equal(auto.status, 'ok');
  assert.equal(auto.last_result.snapshot_id, app.snapshot()!.id);
  assert.deepEqual(auto.last_result.rows, preview.rows);

  // Holding the background slot makes the recompute wait → delete is 409; stopping keeps the previous result
  const lease = await app.slots.acquire('background');
  const r = store.get(id)!;
  store.write({ ...r, last_result: { ...r.last_result, snapshot_id: 'old-snapshot' } });
  h.panels.scan();
  await waitFor(async () => (await panel()).job_status === 'running');
  assert.equal((await call('DELETE', `/api/panels/${id}`)).status, 409);
  const c = await post(`/api/panels/${id}/recompute/cancel`);
  assert.deepEqual(c.body, { job_status: 'cancelled' });
  lease.release();
  const after = await panel();
  assert.equal(after.last_result.snapshot_id, 'old-snapshot');
  assert.equal(after.status, 'ok');
  assert.equal((await post(`/api/panels/${id}/recompute/cancel`)).status, 409);
  assert.ok(s.events.some((e) => e.type === 'panel_status' && e.panel_id === id && e.job_status === 'cancelling'));
  await s.close();
});

test('a snapshot change during a recompute triggers another recompute afterwards', async () => {
  const { conv, preview } = await makePreview();
  const id = (await post('/api/panels', { conversation_id: conv, request_id: preview.request_id, preview_hash: `${preview.preview_hash}`, title: 'Participation', description: '', summary: '' })).body.panel.id;
  const store = new PanelStore(join(app.ws.config.outDir, 'panels'));
  const real = app.snapshot()!;
  const lease = await app.slots.acquire('background');
  const p = store.get(id)!;
  store.write({ ...p, last_result: { ...p.last_result, snapshot_id: 'old-snapshot' } });
  h.panels.scan();
  await waitFor(async () => (await get(`/api/panels/${id}`)).body.panel.job_status === 'running');
  const original = app.snapshot.bind(app);
  app.snapshot = () => ({ ...real, id: 'snap-b' });
  try {
    lease.release();
    const done = await waitFor(async () => { const x = (await get(`/api/panels/${id}`)).body.panel; return x.job_status === 'idle' && x.last_result.snapshot_id === 'snap-b' && x; });
    assert.equal(done.status, 'ok');
  } finally {
    app.snapshot = original;
  }
  assert.equal((await call('DELETE', `/api/panels/${id}`)).status, 200);
});

test('the previous preview cannot be saved while a later request is running', async () => {
  const { conv, preview } = await makePreview();
  setScript([{ structured: panelAction, hang: true }]);
  await post(`/api/conversations/${conv}/messages`, { text: 'again' });
  const body = { conversation_id: conv, request_id: preview.request_id, preview_hash: preview.preview_hash, title: 't', description: '', summary: '' };
  assert.equal((await post(`/api/conversations/${conv}/save-draft`, { request_id: preview.request_id, preview_hash: preview.preview_hash })).status, 409);
  assert.equal((await post('/api/panels', body)).status, 409);
  await post(`/api/conversations/${conv}/stop`);
  assert.equal((await post('/api/panels', body)).status, 201);
});

test('pre-dictionary panels stay in review and are not recomputed', async () => {
  const { conv, preview } = await makePreview();
  const id = (await post('/api/panels', { conversation_id: conv, request_id: preview.request_id, preview_hash: preview.preview_hash, title: 't', description: '', summary: '' })).body.panel.id;
  const file = join(app.ws.config.outDir, 'panels', `${id}.json`);
  const raw = JSON.parse(readFileSync(file, 'utf8'));
  delete raw.spec.metric;
  writeFileSync(file, JSON.stringify(raw));
  h.panels.scan();
  const p = await waitFor(async () => { const x = (await get(`/api/panels/${id}`)).body.panel; return x.status === 'review' && x; });
  assert.equal(p.legacy, true);
  assert.equal(p.metric, null);
  const r = await post(`/api/panels/${id}/recompute`);
  assert.equal(r.status, 409);
  assert.match(r.body.error, /Regenerate/);
  assert.ok(!('metric' in JSON.parse(readFileSync(file, 'utf8')).spec), 'legacy format kept');
  assert.equal((await call('DELETE', `/api/panels/${id}`)).status, 200);
});

test('recompute failure: broken result contract keeps the previous result + recompute_failed', async () => {
  const { conv, preview } = await makePreview();
  const id = (await post('/api/panels', { conversation_id: conv, request_id: preview.request_id, preview_hash: preview.preview_hash, title: 'Participation', description: '', summary: '' })).body.panel.id;
  const store = new PanelStore(join(app.ws.config.outDir, 'panels'));
  const p = store.get(id)!;
  // Change the SQL so the hash does not collide with another preview
  store.write({ ...p, spec: { ...p.spec, sql: 'SELECT 1 AS x, 5 AS numerator, 2 AS denominator FROM d_member_first_week LIMIT 1' }, last_result: { ...p.last_result, snapshot_id: 'old-snapshot' } });
  h.panels.scan();
  const x = await waitFor(async () => { const v = (await get(`/api/panels/${id}`)).body.panel; return v.job_status === 'idle' && v.status !== 'ok' && v; });
  assert.equal(x.status, 'recompute_failed');
  assert.match(x.last_error.message, /invariant/);
  assert.equal(x.last_result.snapshot_id, 'old-snapshot');
  assert.equal((await call('DELETE', `/api/panels/${id}`)).status, 200);
});

test('rewrite description and regenerate with agent', async () => {
  const { conv, preview } = await makePreview();
  const id = (await post('/api/panels', { conversation_id: conv, request_id: preview.request_id, preview_hash: preview.preview_hash, title: 'Participation', description: '', summary: '' })).body.panel.id;
  const sum = summaryFor(preview);
  setScript([sum]);
  const r = await post(`/api/panels/${id}/resummarize`);
  assert.equal(r.status, 200, JSON.stringify(r.body));
  assert.equal(r.body.summary_status, 'ok');
  assert.equal(r.body.panel.summary, sum.expected);

  setScript([{ structured: { ...panelAction, panel: { ...panelAction.panel, title: 'Rebuilt participation', sql: `${panelAction.panel.sql}\n-- rebuilt` } } }]);
  const g = await post(`/api/panels/${id}/regenerate`);
  assert.equal(g.status, 201);
  const nc = g.body.conversation_id;
  const s = sse(`/api/conversations/${nc}/events?since=0`);
  const input = await s.until((e) => e.type === 'user_input');
  assert.match(input.data.text, /^participation by signup week\n\nRebuild it using the previously chosen definitions as defaults/);
  assert.match(input.data.text, /- Window: 7 days/);
  const pv = (await s.until((e) => e.type === 'preview')).data.preview;
  await s.until((e) => e.type === 'done');
  await s.close();
  const saved = await post('/api/panels', { conversation_id: nc, request_id: pv.request_id, preview_hash: pv.preview_hash, title: 'New participation', description: '', summary: '' });
  assert.equal(saved.status, 201);
  assert.equal(saved.body.replaces, id);
  assert.equal(saved.body.panel.prompt, 'participation by signup week');
});

test('quality checks: built-in + workspace checks', async () => {
  const q = await waitFor(async () => { const v = (await get('/api/quality')).body; return v.status === 'idle' && v.snapshot_id && v; });
  assert.deepEqual(q.items.map((i: any) => i.id), ['q_collect', 'q_cutoff_drops', 'q_dates', 'q_daily_activity', 'q_orphans', 'q_seed_panels']);
  const seeds = q.items.find((i: any) => i.id === 'q_seed_panels');
  assert.deepEqual(seeds.rows.map((r: any[]) => r[2]), ['pass', 'pass', 'pass', 'pass']);
  for (const i of q.items) assert.equal(i.error, null, `${i.id}: ${i.error}`);
  const dates = q.items.find((i: any) => i.id === 'q_dates');
  const after = dates.columns.indexOf('after_as_of');
  assert.ok(dates.rows.every((r: any[]) => r[after] === 0), 'no values after the cutoff once cleaned');
  const orphans = q.items.find((i: any) => i.id === 'q_orphans');
  assert.equal(orphans.rows.length, 6);
  assert.equal(q.items.find((i: any) => i.id === 'q_daily_activity').display, 'line');
});

test('schema_only: no description, resummarize is 409', async () => {
  const { conv, preview } = await makePreview();
  app.ws.config.agent.dataMode = 'schema_only';
  try {
    const d = await post(`/api/conversations/${conv}/save-draft`, { request_id: preview.request_id, preview_hash: preview.preview_hash });
    assert.equal(d.body.summary_status, 'disabled');
    assert.equal(d.body.summary, '');
    const id = (await post('/api/panels', { conversation_id: conv, request_id: preview.request_id, preview_hash: preview.preview_hash, title: 't', description: '', summary: '' })).body.panel.id;
    assert.equal((await post(`/api/panels/${id}/resummarize`)).status, 409);
  } finally {
    app.ws.config.agent.dataMode = 'pseudonymized';
  }
});
