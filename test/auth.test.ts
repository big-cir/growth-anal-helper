// 인증·인가: 계정 파일, 로그인·세션, 역할, 소유권, 외부 운영 모드, 감사 기록.
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
  const common = { description: 'd', summary: '', summary_snapshot_id: null, prompt: '원래 요청 문구', spec: seed, generated_model: 'm', created_at: '2024-01-01T00:00:00Z', status: 'ok' as const, last_error: null,
    versions: { snapshot_id: 'snap', schema_version: 'a', policy_version: 'b', docs_version: 'c', prompt_version: 'd', pattern_contract_version: 1, renderer_version: 2 },
    last_result: makeResult(seed, snap, 'preview', [{ name: 'n', table: null, column: null }], [[1]], ['d_member_first_week']) };
  store.write({ ...common, id: 'pnleditor1aa', title: 'e1의 패널', preview_hash: 'a'.repeat(64), created_by: 'editor1' });
  store.write({ ...common, id: 'pnllegacyaaa', title: '소유자 없음', preview_hash: 'b'.repeat(64) });
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

test('계정: scrypt 해시·비교, 비밀번호 길이, 중복', async () => {
  const hash = await hashPassword(TEST_PASSWORD);
  assert.match(hash, /^scrypt\$32768\$8\$1\$/);
  assert.ok(await verifyPassword(TEST_PASSWORD, hash));
  assert.ok(!(await verifyPassword('wrong-password-xx', hash)));
  assert.ok(!(await verifyPassword('x'.repeat(300), hash)));
  await assert.rejects(hashPassword('short'), /12자 이상/);
  const out = mkdtempSync(join(tmpdir(), 'gl-acc-'));
  await addAccount(out, 'someone', 'viewer', TEST_PASSWORD);
  await assert.rejects(addAccount(out, 'someone', 'viewer', TEST_PASSWORD), /이미 있는 계정/);
  await assert.rejects(addAccount(out, 'Bad Name', 'viewer', TEST_PASSWORD), AccountError);
  assert.equal((readFileSync(accountsFile(out)).toString().match(/correct-horse/g) ?? []).length, 0, '비밀번호 원문 없음');
});

test('계정 파일: 권한·심볼릭 링크·소유 디렉터리 검사, 죽은 잠금은 정리', async () => {
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
  assert.throws(() => readAccounts(out2), /심볼릭 링크/);

  writeFileSync(join(out, 'auth', '.lock'), '999999 2024-01-01');
  updateAccounts(out, (l) => l);
  writeFileSync(join(out, 'auth', '.lock'), `${process.pid} now`);
  assert.throws(() => updateAccounts(out, (l) => l), /다른 계정 변경이 진행 중/);
});

test('계정이 없으면 서버가 시작하지 않는다', () => {
  const ws = { ...demo.ws, config: { ...demo.ws.config, outDir: mkdtempSync(join(tmpdir(), 'gl-noacc-')) } };
  assert.throws(() => startServer(new App(ws), 0), /계정이 없어요/);
});

test('로그인 전: 정적 화면만, 모든 API는 경로와 무관하게 401', async () => {
  assert.equal((await fetch(`${base}/`)).status, 200);
  for (const p of ['/api/state', '/api/panels', '/api/nope', '/api/quality', '/api/conversations/aaaaaaaaaaaa']) assert.equal((await call('GET', p)).status, 401, p);
  assert.equal((await call('POST', '/api/conversations')).status, 401);
  assert.equal((await call('GET', '/api/me')).status, 401);
  assert.equal((await call('GET', '/api/state', 'viewer1')).status, 200);
});

test('로그인: 실패는 같은 응답, 쿠키 속성, 로그인도 Origin·헤더 검사, 로그아웃', async () => {
  const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
  const bad = await fetch(`${base}/api/login`, { method: 'POST', headers: H(), body: JSON.stringify({ username: 'editor1', password: 'wrong-password-12' }) });
  assert.equal(bad.status, 401);
  const soon = await fetch(`${base}/api/login`, { method: 'POST', headers: H(), body: JSON.stringify({ username: 'editor1', password: TEST_PASSWORD }) });
  assert.equal(soon.status, 429, '실패 직후에는 맞는 비밀번호도 기다려야 함');
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

test('로그인 시도 제한: 연속 실패마다 대기가 늘고, 전역 상한', () => {
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
  assert.equal(l.check(['u:y']), 1000, '분당·동시 상한');
});

test('세션: 유휴·절대 만료, 사용자 단위 폐기', () => {
  let t = 0;
  const revoked: string[] = [];
  const s = new SessionStore((k) => revoked.push(k), () => t);
  const id = s.create({ username: 'a', role: 'viewer' });
  t += 7 * 3600_000;
  assert.ok(s.get(id));
  t += 9 * 3600_000;
  assert.equal(s.get(id), null, '유휴 8시간');
  const id2 = s.create({ username: 'a', role: 'viewer' });
  assert.equal(s.revokeUser('a'), 1);
  assert.equal(s.get(id2), null);
  assert.equal(revoked.length, 2);
});

test('역할: viewer는 보기만, editor는 대화·저장, 품질은 admin만', async () => {
  assert.equal((await call('GET', '/api/panels', 'viewer1')).status, 200);
  assert.equal((await call('POST', '/api/conversations', 'viewer1')).status, 403);
  assert.equal((await call('GET', '/api/quality', 'viewer1')).status, 403);
  assert.equal((await call('GET', '/api/quality', 'editor1')).status, 403);
  assert.equal((await call('GET', '/api/quality', 'admin1')).status, 200);
  const st = (await call('GET', '/api/state', 'viewer1')).body;
  assert.equal(st.agent, null);
  assert.deepEqual(st.suggestions, []);
});

test('소유권: 남의 대화는 404, 남의 패널은 403, viewer에게는 SQL·처음 요청을 보내지 않음', async () => {
  const conv = (await call('POST', '/api/conversations', 'editor1')).body.conversation_id;
  assert.equal((await call('GET', `/api/conversations/${conv}`, 'editor1')).status, 200);
  assert.equal((await call('GET', `/api/conversations/${conv}`, 'editor2')).status, 404);
  assert.equal((await call('GET', `/api/conversations/${conv}`, 'admin1')).status, 404, 'admin도 남의 대화는 못 봄');
  assert.equal((await call('POST', `/api/conversations/${conv}/messages`, 'editor2', { text: 'x' })).status, 404);

  const asViewer = (await call('GET', '/api/panels/pnleditor1aa', 'viewer1')).body.panel;
  assert.equal(asViewer.full, false);
  for (const k of ['sql', 'prompt', 'tables', 'answers', 'last_error', 'versions']) assert.ok(!(k in asViewer), k);
  assert.ok(!JSON.stringify(asViewer).includes('원래 요청 문구'));
  assert.ok(!('table' in asViewer.last_result.columns[0]));
  const asOwner = (await call('GET', '/api/panels/pnleditor1aa', 'editor1')).body.panel;
  assert.equal(asOwner.full, true);
  assert.equal(asOwner.prompt, '원래 요청 문구');
  assert.equal((await call('GET', '/api/panels/pnleditor1aa', 'editor2')).body.panel.full, false);

  for (const [m, p] of [['DELETE', '/api/panels/pnleditor1aa'], ['POST', '/api/panels/pnleditor1aa/regenerate'], ['POST', '/api/panels/pnleditor1aa/resummarize'], ['POST', '/api/panels/pnllegacyaaa/regenerate']]) {
    assert.equal((await call(m, p, 'editor2')).status, 403, `${m} ${p}`);
  }
  assert.equal((await call('DELETE', '/api/panels/pnllegacyaaa', 'admin1')).status, 200, '소유자 없는 이전 패널은 admin');
});

test('계정을 끄면 세션과 열린 SSE가 바로 끊긴다', async () => {
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
  assert.ok(done, 'SSE가 닫힘');
  assert.equal((await fetch(`${base}/api/me`, { headers: { Cookie: c } })).status, 401);
  modifyAccount(demo.ws.config.outDir, 'editor2', { disabled: false });
  h.auth.reload();
  cookies.editor2 = await login(base, 'editor2');
});

test('감사 기록: 로그인·삭제가 남고 비밀번호·세션 값은 없음', () => {
  const dir = join(demo.ws.config.outDir, 'logs', 'audit');
  const text = readdirSync(dir).filter((f) => f.startsWith('audit-')).map((f) => readFileSync(join(dir, f), 'utf8')).join('');
  assert.match(text, /"event":"login_ok","user":"editor1"/);
  assert.match(text, /"event":"login_fail"/);
  assert.match(text, /"event":"panel_deleted","user":"admin1"/);
  assert.match(text, /"event":"session_revoked","user":"editor2"/);
  assert.ok(!text.includes(TEST_PASSWORD));
  assert.ok(!text.includes(cookies.editor1.split('=')[1]));
});

test('라우트 표 검사: 권한 누락·중복·public 남용은 시작 오류', () => {
  const h0 = () => {};
  assert.throws(() => checkRoutes([{ method: 'GET', pattern: /^\/api\/x$/, access: 'public', handler: h0 }] as never), /public 라우트는/);
  assert.throws(() => checkRoutes([{ method: 'GET', pattern: /^\/api\/x$/, handler: h0 }] as never), /권한 누락/);
  assert.throws(() => checkRoutes([{ method: 'GET', pattern: /^\/api\/x$/, access: 'viewer', handler: h0 }, { method: 'GET', pattern: /^\/api\/x$/, access: 'admin', handler: h0 }] as never), /중복/);
});

test('클라이언트 IP: 외부 모드에서만 X-Forwarded-For 오른쪽부터, IPv4-mapped 정규화', () => {
  assert.equal(clientIp('::ffff:192.0.2.10', '192.0.2.11', null), '192.0.2.10');
  assert.equal(clientIp('127.0.0.1', 'spoof, 198.51.100.9', 1), '198.51.100.9');
  assert.equal(clientIp('127.0.0.1', 'a, 203.0.113.8, 198.51.100.9', 2), '203.0.113.8');
});

test('외부 운영 모드: publicOrigin 호스트·출처만, Secure 쿠키, HSTS', async () => {
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
    assert.equal((await go({ Host: `127.0.0.1:${hx.port}` })).status, 403, '루프백 Host 거부');
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
    assert.match(String(sseHeaders['strict-transport-security']), /max-age/, 'SSE에도 HSTS');
  } finally {
    await hx.close();
  }
});

test('DTO: viewer·남의 패널은 정해진 필드만, 소유자는 전체 필드', async () => {
  const short = ['created_at', 'created_by', 'definition', 'description', 'display', 'full', 'id', 'job_status', 'last_result', 'legacy', 'metric', 'status', 'summary', 'summary_snapshot_id', 'title'];
  const asViewer = (await call('GET', '/api/panels/pnleditor1aa', 'viewer1')).body.panel;
  assert.deepEqual(Object.keys(asViewer).sort(), short);
  assert.deepEqual(Object.keys(asViewer.last_result).sort(), ['as_of', 'caveats', 'columns', 'computed_at', 'headline', 'mode', 'rows', 'snapshot_id']);
  const asOwner = (await call('GET', '/api/panels/pnleditor1aa', 'editor1')).body.panel;
  assert.deepEqual(Object.keys(asOwner).sort(), [...short, 'answers', 'changed_rules', 'generated_model', 'last_error', 'prompt', 'sql', 'tables', 'versions'].sort());
});

test('패널 이벤트: 최소 상태만, 품질 이벤트는 admin에게만', async () => {
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

test('로그아웃은 본문 없이도 되고, 감사 기록을 못 쓰면 변경을 거부한다', async () => {
  const c = await login(base, 'viewer1');
  const out = await fetch(`${base}/api/logout`, { method: 'POST', headers: { Origin: base, 'X-Growth-Lab': '1', Cookie: c } });
  assert.equal(out.status, 200);

  const auditDir = join(demo.ws.config.outDir, 'logs', 'audit');
  chmodSync(auditDir, 0o755);
  try {
    const conv = await call('POST', '/api/conversations', 'editor1');
    const r = await call('POST', `/api/conversations/${conv.body.conversation_id}/messages`, 'editor1', { text: '감사 기록 실패 시험' });
    assert.equal(r.status, 500);
    assert.match(r.body.error, /감사 기록/);
    const st = await call('GET', `/api/conversations/${conv.body.conversation_id}`, 'editor1');
    assert.equal(st.body.request, null, '요청이 시작되지 않음');
    const li = await fetch(`${base}/api/login`, { method: 'POST', headers: H(), body: JSON.stringify({ username: 'viewer1', password: TEST_PASSWORD }) });
    assert.equal(li.status, 500, '로그인 성공 기록을 못 쓰면 로그인도 거부');
    assert.equal(li.headers.get('set-cookie'), null);
    const lo = await fetch(`${base}/api/logout`, { method: 'POST', headers: { Origin: base, 'X-Growth-Lab': '1', Cookie: cookies.viewer1 } });
    assert.equal(lo.status, 500, '로그아웃 기록을 못 쓰면 세션 유지');
    assert.equal(lo.headers.get('set-cookie'), null);
    assert.equal((await call('GET', '/api/me', 'viewer1')).status, 200);
  } finally {
    chmodSync(auditDir, 0o700);
  }
});

test('계정 입력 파일: 적은 계정을 반영하고 비밀번호 칸을 비운다, 권한이 넓으면 거부', async () => {
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
