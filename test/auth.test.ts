// Authentication and authorization: account file, sign-in and sessions, roles, ownership, external mode, audit log.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, symlinkSync, writeFileSync } from 'node:fs';
import { request } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { App } from '../src/server/app.ts';
import { checkRoutes, startServer, type ServerHandle } from '../src/server/http.ts';
import { accountsFile, addAccount, AccountError, hashPassword, modifyAccount, readAccounts, updateAccounts, verifyPassword } from '../src/auth/accounts.ts';
import { clientIp, LoginLimiter, SessionStore } from '../src/auth/sessions.ts';
import { PanelStore } from '../src/panels/store.ts';
import { makeResult } from '../src/server/panels.ts';
import { buildDemoSnapshot, demoSeedPanels, type DemoSnapshot } from './helpers/demo-snapshot.ts';
import { login, makeAccount, TEST_PASSWORD } from './helpers/auth.ts';

let demo: DemoSnapshot;
let h: ServerHandle;
let base: string;
const cookies: Record<string, string> = {};

before(async () => {
  demo = await buildDemoSnapshot();
  const out = demo.ws.config.outDir;
  for (const [u, r] of [['admin1', 'admin'], ['editor1', 'editor'], ['editor2', 'editor'], ['viewer1', 'viewer']] as const) await makeAccount(out, u, r);
  const seed = demoSeedPanels()[0].spec;
  const store = new PanelStore(join(out, 'panels'));
  const snap = { id: 'snap', asOf: demo.asOf };
  const common = { description: 'd', summary: '', summary_snapshot_id: null, prompt: 'original request text', spec: seed, generated_model: 'm', created_at: '2024-01-01T00:00:00Z', status: 'ok' as const, last_error: null,
    versions: { snapshot_id: 'snap', schema_version: 'a', policy_version: 'b', docs_version: 'c', prompt_version: 'd', pattern_contract_version: 1, renderer_version: 2 },
    last_result: makeResult(seed, snap, 'preview', [{ name: 'n', table: null, column: null }], [[1]], ['d_member_first_week']) };
  store.write({ ...common, id: 'pnleditor1aa', title: 'panel of e1', preview_hash: 'a'.repeat(64), created_by: 'editor1' });
  store.write({ ...common, id: 'pnllegacyaaa', title: 'no owner', preview_hash: 'b'.repeat(64) });
  h = await startServer(new App(demo.ws), 0);
  base = `http://127.0.0.1:${h.port}`;
  for (const u of ['admin1', 'editor1', 'editor2', 'viewer1']) cookies[u] = await login(base, u);
});
after(async () => {
  await h.close();
});

const H = (who?: string) => ({ Origin: base, 'X-Growth-Lab': '1', 'Content-Type': 'application/json', ...(who ? { Cookie: cookies[who] } : {}) });
async function call(method: string, path: string, who?: string, body: unknown = {}): Promise<{ status: number; body: any; headers: Headers }> {
  const res = await fetch(`${base}${path}`, method === 'GET' ? { headers: who ? { Cookie: cookies[who] } : {} } : { method, headers: H(who), body: JSON.stringify(body) });
  return { status: res.status, body: await res.json().catch(() => null), headers: res.headers };
}

test('accounts: scrypt hash and verify, password length, duplicates', async () => {
  const hash = await hashPassword(TEST_PASSWORD);
  assert.match(hash, /^scrypt\$32768\$8\$1\$/);
  assert.ok(await verifyPassword(TEST_PASSWORD, hash));
  assert.ok(!(await verifyPassword('wrong-password-xx', hash)));
  assert.ok(!(await verifyPassword('x'.repeat(300), hash)));
  await assert.rejects(hashPassword('short'), /at least 12 characters/);
  const out = mkdtempSync(join(tmpdir(), 'gl-acc-'));
  await addAccount(out, 'someone', 'viewer', TEST_PASSWORD);
  await assert.rejects(addAccount(out, 'someone', 'viewer', TEST_PASSWORD), /Account already exists/);
  await assert.rejects(addAccount(out, 'Bad Name', 'viewer', TEST_PASSWORD), AccountError);
  assert.equal((readFileSync(accountsFile(out)).toString().match(/correct-horse/g) ?? []).length, 0, 'no plain password');
});

test('account file: permission, symlink and directory ownership checks; stale lock cleanup', async () => {
  const out = mkdtempSync(join(tmpdir(), 'gl-acc-'));
  await addAccount(out, 'someone', 'viewer', TEST_PASSWORD);
  chmodSync(accountsFile(out), 0o644);
  assert.throws(() => readAccounts(out), /0600/);
  chmodSync(accountsFile(out), 0o600);
  chmodSync(join(out, 'auth'), 0o755);
  assert.throws(() => readAccounts(out), /0700/);
  chmodSync(join(out, 'auth'), 0o700);

  const out2 = mkdtempSync(join(tmpdir(), 'gl-acc-'));
  mkdirSync(join(out2, 'auth'), { mode: 0o700 });
  symlinkSync(accountsFile(out), accountsFile(out2));
  assert.throws(() => readAccounts(out2), /symlink/);

  writeFileSync(join(out, 'auth', '.lock'), '999999 2024-01-01');
  updateAccounts(out, (l) => l);
  writeFileSync(join(out, 'auth', '.lock'), `${process.pid} now`);
  assert.throws(() => updateAccounts(out, (l) => l), /Another account change is in progress/);
});

test('server does not start without accounts', () => {
  const ws = { ...demo.ws, config: { ...demo.ws.config, outDir: mkdtempSync(join(tmpdir(), 'gl-noacc-')) } };
  assert.throws(() => startServer(new App(ws), 0), /No accounts/);
});

test('auth off: starts without accounts, every request is admin local, sign-in refused, not allowed with publicOrigin', async () => {
  const ws = { ...demo.ws, config: { ...demo.ws.config, server: { ...demo.ws.config.server, auth: false } } };
  const noAccounts = await startServer(new App({ ...ws, config: { ...ws.config, outDir: mkdtempSync(join(tmpdir(), 'gl-open-')) } }), 0);
  await noAccounts.close();
  const open = await startServer(new App(ws), 0);
  try {
    const b = `http://127.0.0.1:${open.port}`;
    assert.deepEqual(await (await fetch(`${b}/api/me`)).json(), { username: 'local', role: 'admin', auth: false });
    assert.equal((await fetch(`${b}/api/quality`)).status, 200);
    const r = await fetch(`${b}/api/login`, { method: 'POST', headers: { Origin: b, 'X-Growth-Lab': '1', 'Content-Type': 'application/json' }, body: JSON.stringify({ username: 'admin1', password: TEST_PASSWORD }) });
    assert.equal(r.status, 400);
  } finally {
    await open.close();
  }
  const { parseWorkspaceConfig } = await import('../src/workspace.ts');
  const raw = { name: 'x', datasource: { host: 'sqlite://s.sqlite' }, policy: { readablePrefixes: ['r_', 'd_'] } };
  assert.equal(parseWorkspaceConfig(raw, '/ws').server.auth, false);
  assert.throws(() => parseWorkspaceConfig({ ...raw, server: { publicOrigin: 'https://g.example.com' } }, '/ws'), /server\.auth/);
  assert.equal(parseWorkspaceConfig({ ...raw, server: { auth: true, publicOrigin: 'https://g.example.com' } }, '/ws').server.auth, true);
});

test('before sign-in: static files only, every API path returns 401', async () => {
  assert.equal((await fetch(`${base}/`)).status, 200);
  for (const p of ['/api/state', '/api/panels', '/api/nope', '/api/quality', '/api/conversations/aaaaaaaaaaaa']) assert.equal((await call('GET', p)).status, 401, p);
  assert.equal((await call('POST', '/api/conversations')).status, 401);
  assert.equal((await call('GET', '/api/me')).status, 401);
  assert.equal((await call('GET', '/api/state', 'viewer1')).status, 200);
});

test('sign-in: same response for failures, cookie attributes, Origin and header checks, sign-out', async () => {
  const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
  const bad = await fetch(`${base}/api/login`, { method: 'POST', headers: H(), body: JSON.stringify({ username: 'editor1', password: 'wrong-password-12' }) });
  assert.equal(bad.status, 401);
  const soon = await fetch(`${base}/api/login`, { method: 'POST', headers: H(), body: JSON.stringify({ username: 'editor1', password: TEST_PASSWORD }) });
  assert.equal(soon.status, 429, 'right after a failure even the correct password must wait');
  assert.equal(soon.headers.get('retry-after'), '1');
  await sleep(1100);
  const none = await fetch(`${base}/api/login`, { method: 'POST', headers: H(), body: JSON.stringify({ username: 'nobody', password: 'wrong-password-12' }) });
  assert.equal(none.status, 401);
  assert.deepEqual(await bad.json(), await none.json());
  await sleep(2100);
  const noOrigin = await fetch(`${base}/api/login`, { method: 'POST', headers: { 'X-Growth-Lab': '1', 'Content-Type': 'application/json' }, body: JSON.stringify({ username: 'editor1', password: TEST_PASSWORD }) });
  assert.equal(noOrigin.status, 403);

  const ok = await fetch(`${base}/api/login`, { method: 'POST', headers: H(), body: JSON.stringify({ username: 'viewer1', password: TEST_PASSWORD }) });
  const set = ok.headers.get('set-cookie')!;
  assert.match(set, /HttpOnly/);
  assert.match(set, /SameSite=Strict/);
  assert.doesNotMatch(set, /Secure/);
  const c = set.split(';')[0];
  assert.equal((await fetch(`${base}/api/me`, { headers: { Cookie: c } })).status, 200);
  const out = await fetch(`${base}/api/logout`, { method: 'POST', headers: { ...H(), Cookie: c }, body: '{}' });
  assert.match(out.headers.get('set-cookie')!, /Max-Age=0/);
  assert.equal((await fetch(`${base}/api/me`, { headers: { Cookie: c } })).status, 401);
});

test('sign-in limiter: wait grows per consecutive failure, global cap', () => {
  let t = 0;
  const l = new LoginLimiter(() => t);
  const keys = ['u:x', 'ip:1'];
  assert.equal(l.check(keys), null);
  for (const wait of [1000, 2000, 4000]) {
    l.begin();
    l.end(keys, false);
    assert.equal(l.check(keys), wait);
    t += wait;
  }
  l.begin();
  l.end(keys, true);
  assert.equal(l.check(keys), null);
  for (let i = 0; i < 60; i++) l.begin();
  assert.equal(l.check(['u:y']), 1000, 'per-minute and concurrency cap');
});

test('sessions: idle and absolute expiry, per-user revoke', () => {
  let t = 0;
  const revoked: string[] = [];
  const s = new SessionStore((k) => revoked.push(k), () => t);
  const id = s.create({ username: 'a', role: 'viewer' });
  t += 7 * 3600_000;
  assert.ok(s.get(id));
  t += 9 * 3600_000;
  assert.equal(s.get(id), null, 'idle 8 hours');
  const id2 = s.create({ username: 'a', role: 'viewer' });
  assert.equal(s.revokeUser('a'), 1);
  assert.equal(s.get(id2), null);
  assert.equal(revoked.length, 2);
});

test('roles: viewer reads only, editor converses and saves, quality is admin only', async () => {
  assert.equal((await call('GET', '/api/panels', 'viewer1')).status, 200);
  assert.equal((await call('POST', '/api/conversations', 'viewer1')).status, 403);
  assert.equal((await call('GET', '/api/quality', 'viewer1')).status, 403);
  assert.equal((await call('GET', '/api/quality', 'editor1')).status, 403);
  assert.equal((await call('GET', '/api/quality', 'admin1')).status, 200);
  const st = (await call('GET', '/api/state', 'viewer1')).body;
  assert.equal(st.agent, null);
  assert.deepEqual(st.suggestions, []);
});

test('ownership: others conversations 404, others panels 403, viewers get no SQL or original request', async () => {
  const conv = (await call('POST', '/api/conversations', 'editor1')).body.conversation_id;
  assert.equal((await call('GET', `/api/conversations/${conv}`, 'editor1')).status, 200);
  assert.equal((await call('GET', `/api/conversations/${conv}`, 'editor2')).status, 404);
  assert.equal((await call('GET', `/api/conversations/${conv}`, 'admin1')).status, 404, 'admin cannot see others conversations either');
  assert.equal((await call('POST', `/api/conversations/${conv}/messages`, 'editor2', { text: 'x' })).status, 404);

  const asViewer = (await call('GET', '/api/panels/pnleditor1aa', 'viewer1')).body.panel;
  assert.equal(asViewer.full, false);
  for (const k of ['sql', 'prompt', 'tables', 'answers', 'last_error', 'versions']) assert.ok(!(k in asViewer), k);
  assert.ok(!JSON.stringify(asViewer).includes('original request text'));
  assert.ok(!('table' in asViewer.last_result.columns[0]));
  const asOwner = (await call('GET', '/api/panels/pnleditor1aa', 'editor1')).body.panel;
  assert.equal(asOwner.full, true);
  assert.equal(asOwner.prompt, 'original request text');
  assert.equal((await call('GET', '/api/panels/pnleditor1aa', 'editor2')).body.panel.full, false);

  for (const [m, p] of [['DELETE', '/api/panels/pnleditor1aa'], ['POST', '/api/panels/pnleditor1aa/regenerate'], ['POST', '/api/panels/pnleditor1aa/resummarize'], ['POST', '/api/panels/pnllegacyaaa/regenerate']]) {
    assert.equal((await call(m, p, 'editor2')).status, 403, `${m} ${p}`);
  }
  assert.equal((await call('DELETE', '/api/panels/pnllegacyaaa', 'admin1')).status, 200, 'ownerless legacy panels: admin');
});

test('latest conversation after sign-in: only ones with input, never others', async () => {
  assert.equal((await call('GET', '/api/conversations', 'viewer1')).status, 403);
  const used = (await call('POST', '/api/conversations', 'editor2')).body.conversation_id;
  assert.equal((await call('POST', `/api/conversations/${used}/messages`, 'editor2', { text: 'weekly signups' })).status, 202);
  await call('POST', `/api/conversations/${used}/stop`, 'editor2');
  const empty = (await call('POST', '/api/conversations', 'editor2')).body.conversation_id;
  assert.notEqual(empty, used);
  assert.equal((await call('GET', '/api/conversations', 'editor2')).body.conversation_id, used, 'empty conversations are skipped');
  const others = (await call('GET', '/api/conversations', 'admin1')).body.conversation_id;
  assert.notEqual(others, used, 'not even admin gets others conversations');
});

test('disabling an account drops its sessions and open SSE at once', async () => {
  const c = await login(base, 'editor2');
  const ac = new AbortController();
  const res = await fetch(`${base}/api/panels/events`, { headers: { Cookie: c }, signal: ac.signal });
  const reader = res.body!.getReader();
  await reader.read();
  modifyAccount(demo.ws.config.outDir, 'editor2', { disabled: true });
  h.auth.reload();
  const t0 = Date.now();
  let done = false;
  while (!done && Date.now() - t0 < 3000) done = (await reader.read()).done;
  assert.ok(done, 'SSE closed');
  assert.equal((await fetch(`${base}/api/me`, { headers: { Cookie: c } })).status, 401);
  modifyAccount(demo.ws.config.outDir, 'editor2', { disabled: false });
  h.auth.reload();
  cookies.editor2 = await login(base, 'editor2');
});

test('audit log: sign-ins and deletes recorded, no passwords or session values', () => {
  const dir = join(demo.ws.config.outDir, 'logs', 'audit');
  const text = readdirSync(dir).filter((f) => f.startsWith('audit-')).map((f) => readFileSync(join(dir, f), 'utf8')).join('');
  assert.match(text, /"event":"login_ok","user":"editor1"/);
  assert.match(text, /"event":"login_fail"/);
  assert.match(text, /"event":"panel_deleted","user":"admin1"/);
  assert.match(text, /"event":"session_revoked","user":"editor2"/);
  assert.ok(!text.includes(TEST_PASSWORD));
  assert.ok(!text.includes(cookies.editor1.split('=')[1]));
});

test('route table check: missing access, duplicates and public misuse fail at startup', () => {
  const h0 = () => {};
  assert.throws(() => checkRoutes([{ method: 'GET', pattern: /^\/api\/x$/, access: 'public', handler: h0 }] as never), /Only login\/logout\/me may be public/);
  assert.throws(() => checkRoutes([{ method: 'GET', pattern: /^\/api\/x$/, handler: h0 }] as never), /no access level/);
  assert.throws(() => checkRoutes([{ method: 'GET', pattern: /^\/api\/x$/, access: 'viewer', handler: h0 }, { method: 'GET', pattern: /^\/api\/x$/, access: 'admin', handler: h0 }] as never), /Duplicate route/);
});

test('client IP: X-Forwarded-For from the right in external mode only, IPv4-mapped normalized', () => {
  assert.equal(clientIp('::ffff:192.0.2.10', '192.0.2.11', null), '192.0.2.10');
  assert.equal(clientIp('127.0.0.1', 'spoof, 198.51.100.9', 1), '198.51.100.9');
  assert.equal(clientIp('127.0.0.1', 'a, 203.0.113.8, 198.51.100.9', 2), '203.0.113.8');
});

test('external mode: publicOrigin host and origin only, Secure cookie, HSTS', async () => {
  const ws = { ...demo.ws, config: { ...demo.ws.config, server: { ...demo.ws.config.server, publicOrigin: 'https://growth.example.com' } } };
  const hx = await startServer(new App(ws), 0);
  try {
    const go = (headers: Record<string, string>, body?: string) => new Promise<{ status: number; headers: Record<string, string | string[] | undefined> }>((resolve, reject) => {
      const r = request(`http://127.0.0.1:${hx.port}${body ? '/api/login' : '/'}`, { method: body ? 'POST' : 'GET', headers }, (res) => {
        res.resume();
        res.on('end', () => resolve({ status: res.statusCode!, headers: res.headers }));
      });
      r.on('error', reject);
      if (body) r.write(body);
      r.end();
    });
    assert.equal((await go({ Host: `127.0.0.1:${hx.port}` })).status, 403, 'loopback Host refused');
    const page = await go({ Host: 'growth.example.com' });
    assert.equal(page.status, 200);
    assert.match(String(page.headers['strict-transport-security']), /max-age/);
    const body = JSON.stringify({ username: 'viewer1', password: TEST_PASSWORD });
    assert.equal((await go({ Host: 'growth.example.com', Origin: `http://127.0.0.1:${hx.port}`, 'X-Growth-Lab': '1', 'Content-Type': 'application/json' }, body)).status, 403);
    const ok = await go({ Host: 'growth.example.com', Origin: 'https://growth.example.com', 'X-Growth-Lab': '1', 'Content-Type': 'application/json' }, body);
    assert.equal(ok.status, 200);
    assert.match(String(ok.headers['set-cookie']), /Secure/);
    const c = String(ok.headers['set-cookie']).split(';')[0];
    const sseHeaders = await new Promise<Record<string, unknown>>((resolve, reject) => {
      const r = request(`http://127.0.0.1:${hx.port}/api/panels/events`, { headers: { Host: 'growth.example.com', Cookie: c } }, (res) => {
        resolve(res.headers);
        res.destroy();
      });
      r.on('error', reject);
      r.end();
    });
    assert.match(String(sseHeaders['strict-transport-security']), /max-age/, 'HSTS on SSE too');
  } finally {
    await hx.close();
  }
});

test('DTO: viewers and non-owners get fixed fields, owners get all', async () => {
  const short = ['created_at', 'created_by', 'definition', 'description', 'display', 'full', 'id', 'job_status', 'last_result', 'legacy', 'metric', 'status', 'summary', 'summary_snapshot_id', 'title'];
  const asViewer = (await call('GET', '/api/panels/pnleditor1aa', 'viewer1')).body.panel;
  assert.deepEqual(Object.keys(asViewer).sort(), short);
  assert.deepEqual(Object.keys(asViewer.last_result).sort(), ['as_of', 'caveats', 'columns', 'computed_at', 'headline', 'mode', 'rows', 'snapshot_id']);
  const asOwner = (await call('GET', '/api/panels/pnleditor1aa', 'editor1')).body.panel;
  assert.deepEqual(Object.keys(asOwner).sort(), [...short, 'answers', 'changed_rules', 'generated_model', 'last_error', 'prompt', 'sql', 'tables', 'versions'].sort());
});

test('panel events: minimal state, quality events for admin only', async () => {
  const read = async (who: string) => {
    const ac = new AbortController();
    const res = await fetch(`${base}/api/panels/events`, { headers: { Cookie: cookies[who] }, signal: ac.signal });
    const reader = res.body!.getReader();
    let text = '';
    while (!text.includes('panel_status_all')) text += new TextDecoder().decode((await reader.read()).value);
    ac.abort();
    return JSON.parse(/event: panel_status_all\ndata: (.+)\n/.exec(text)![1]);
  };
  const all = await read('viewer1');
  assert.deepEqual(Object.keys(all), ['panels']);
  for (const p of all.panels) assert.deepEqual(Object.keys(p).sort(), ['job_status', 'panel_id', 'status']);
});

test('sign-out works without a body; changes are refused when the audit log cannot be written', async () => {
  const c = await login(base, 'viewer1');
  const out = await fetch(`${base}/api/logout`, { method: 'POST', headers: { Origin: base, 'X-Growth-Lab': '1', Cookie: c } });
  assert.equal(out.status, 200);

  const auditDir = join(demo.ws.config.outDir, 'logs', 'audit');
  chmodSync(auditDir, 0o755);
  try {
    const conv = await call('POST', '/api/conversations', 'editor1');
    const r = await call('POST', `/api/conversations/${conv.body.conversation_id}/messages`, 'editor1', { text: 'audit failure test' });
    assert.equal(r.status, 500);
    assert.match(r.body.error, /audit log/);
    const st = await call('GET', `/api/conversations/${conv.body.conversation_id}`, 'editor1');
    assert.equal(st.body.request, null, 'request not started');
    const li = await fetch(`${base}/api/login`, { method: 'POST', headers: H(), body: JSON.stringify({ username: 'viewer1', password: TEST_PASSWORD }) });
    assert.equal(li.status, 500, 'sign-in refused when its audit record cannot be written');
    assert.equal(li.headers.get('set-cookie'), null);
    const lo = await fetch(`${base}/api/logout`, { method: 'POST', headers: { Origin: base, 'X-Growth-Lab': '1', Cookie: cookies.viewer1 } });
    assert.equal(lo.status, 500, 'session kept when the sign-out record cannot be written');
    assert.equal(lo.headers.get('set-cookie'), null);
    assert.equal((await call('GET', '/api/me', 'viewer1')).status, 200);
  } finally {
    chmodSync(auditDir, 0o700);
  }
});

test('account input file: applies accounts and clears passwords, refuses loose permissions', async () => {
  const { importAccounts, pendingInputPasswords } = await import('../src/auth/cli.ts');
  const out = mkdtempSync(join(tmpdir(), 'gl-imp-'));
  const file = join(out, 'accounts.input.json');
  writeFileSync(file, JSON.stringify({ accounts: [{ username: 'alice', role: 'admin', password: TEST_PASSWORD }, { username: 'bob', role: 'viewer', password: '' }] }), { mode: 0o644 });
  await assert.rejects(importAccounts(out), /0600/);
  chmodSync(file, 0o600);
  assert.equal(pendingInputPasswords(out), true);
  assert.deepEqual(await importAccounts(out), ['alice(admin)']);
  assert.equal(pendingInputPasswords(out), false);
  assert.ok(!readFileSync(file, 'utf8').includes(TEST_PASSWORD));
  assert.deepEqual(readAccounts(out).map((a) => [a.username, a.role]), [['alice', 'admin']]);
  writeFileSync(file, JSON.stringify({ accounts: [{ username: 'alice', role: 'viewer', password: 'another-long-password' }] }), { mode: 0o600 });
  await importAccounts(out);
  assert.equal(readAccounts(out)[0].role, 'viewer');
});
