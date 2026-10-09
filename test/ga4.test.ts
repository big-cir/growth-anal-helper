// GA4 import: config and file checks, service account JWT, fixed endpoints, report transforms, paging, errors, snapshot integration.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, cpSync, mkdtempSync, readFileSync, symlinkSync, writeFileSync } from 'node:fs';
import { createVerify, generateKeyPairSync } from 'node:crypto';
import { DatabaseSync } from 'node:sqlite';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { API_BASE, Ga4Error, SCOPE, TOKEN_URL, type HttpRequest, type HttpResponse, type Transport } from '../src/collect/ga4/client.ts';
import { ga4SpecHash, loadCredentials, loadReports, Ga4ConfigError, type Ga4Connection } from '../src/collect/ga4/config.ts';
import { completeRange, cohortWeeks, customDef, dataThrough, REPORT_KINDS, transformRows } from '../src/collect/ga4/reports.ts';
import { rawHash } from '../src/collect/finalize.ts';
import { parseSpec } from '../src/collect/spec.ts';
import { importGa4, loadGa4Plan } from '../src/collect/ga4/importer.ts';
import { ENGINE_TABLES_SQL } from '../src/snapshot/raw-ddl.ts';
import { runCollect, runDerive } from '../src/snapshot/pipeline.ts';
import { readCurrent } from '../src/snapshot/store.ts';
import { loadWorkspace } from '../src/workspace.ts';
import { seedDemo } from '../examples/demo/seed.ts';
import { DEMO_DIR } from './helpers/demo-snapshot.ts';

const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
const PEM = privateKey.export({ type: 'pkcs8', format: 'pem' }).toString();
const NOW = Date.parse('2024-06-12T20:00:00Z'); // June 12 in Los Angeles → data through June 10
const TZ = 'America/Los_Angeles';

/** Workspace folder with a key file and ga4-reports.json */
function ga4Files(reports: unknown = { start: '2024-03-01', reports: ['daily_overview', 'weekly_users', 'monthly_users', 'daily_events', 'daily_channel', 'daily_platform', 'daily_new_returning', 'weekly_cohort'], events: { POST_WRITE: 'post_write', REPLY_WRITE: 'reply_write' }, min_users: 10 }) {
  const dir = mkdtempSync(join(tmpdir(), 'gl-ga4-'));
  const key = join(dir, 'sa.json');
  writeFileSync(key, JSON.stringify({ type: 'service_account', client_email: 'reader@example.iam.gserviceaccount.com', private_key: PEM, token_uri: 'https://attacker.example/token' }), { mode: 0o600 });
  writeFileSync(join(dir, 'ga4-reports.json'), JSON.stringify(reports));
  return dir;
}

const conn = (dir: string): Ga4Connection => ({ propertyId: '123456', timeZone: TZ, keyFile: join(dir, 'sa.json') });

type Seen = { reqs: HttpRequest[] };
/** Fake Google: verifies the token signature with the public key and returns fixed responses per report */
function fakeGoogle(o: { timeZone?: string | null; quotaRemaining?: number; status?: number[]; pageSize?: number; repeatPage?: boolean; compat?: 'incompatible' | 'missing' | 'duplicate'; token?: Record<string, unknown>; badRow?: 'metrics' | 'dims' | 'number'; noQuota?: boolean; noRowCount?: boolean; empty?: boolean } = {}): { transport: Transport; seen: Seen } {
  const seen: Seen = { reqs: [] };
  const statuses = [...(o.status ?? [])];
  const transport: Transport = async (req) => {
    seen.reqs.push(req);
    const s = statuses.shift();
    if (s) return { status: s, headers: { 'retry-after': '1' }, body: '{"error":"secret detail token=abc"}' };
    if (req.url === TOKEN_URL) {
      const assertion = new URLSearchParams(req.body).get('assertion')!;
      const [h, c, sig] = assertion.split('.');
      const v = createVerify('RSA-SHA256');
      v.update(`${h}.${c}`);
      assert.ok(v.verify(publicKey, Buffer.from(sig, 'base64url')), 'JWT signature');
      return { status: 200, headers: {}, body: JSON.stringify(o.token ?? { access_token: 'ya29.fake', token_type: 'Bearer', expires_in: 3600 }) };
    }
    assert.ok(req.url.startsWith(`${API_BASE}/properties/123456:`), req.url);
    assert.equal(req.headers.Authorization, 'Bearer ya29.fake');
    const body = JSON.parse(req.body);
    if (req.url.endsWith(':checkCompatibility')) {
      // Like the real API, also returns items that were not requested
      const dims: { name: string }[] = [...body.dimensions, { name: 'country' }, { name: 'city' }];
      const metrics: { name: string }[] = [...body.metrics, { name: 'screenPageViews' }];
      if (o.compat === 'missing') dims.shift();
      if (o.compat === 'duplicate') metrics.push(body.metrics[0]);
      return json({
        dimensionCompatibilities: dims.map((d, i) => ({ dimensionMetadata: { apiName: d.name }, compatibility: o.compat === 'incompatible' && i === 0 ? 'INCOMPATIBLE' : 'COMPATIBLE' })),
        metricCompatibilities: metrics.map((m) => ({ metricMetadata: { apiName: m.name }, compatibility: 'COMPATIBLE' })),
      });
    }
    const dims: string[] = body.dimensions.map((d: { name: string }) => d.name);
    const metrics: string[] = body.metrics.map((m: { name: string }) => m.name);
    const all = o.empty ? [] : rowsFor(dims, metrics);
    const size = o.pageSize ?? all.length;
    const page = o.repeatPage ? all.slice(0, size) : all.slice(body.offset === 0 ? 0 : size, body.offset === 0 ? size : undefined);
    return json({
      dimensionHeaders: dims.map((name) => ({ name })),
      metricHeaders: metrics.map((name) => ({ name, type: name === 'userEngagementDuration' ? 'TYPE_SECONDS' : 'TYPE_INTEGER' })),
      rows: page.map((r) => ({
        dimensionValues: (o.badRow === 'dims' ? [...r.d, 'x'] : r.d).map((value) => ({ value })),
        metricValues: (o.badRow === 'metrics' ? r.m.slice(1) : r.m).map((value) => ({ value: o.badRow === 'number' ? value : String(value) })),
      })),
      ...(o.noRowCount ? {} : { rowCount: all.length }),
      metadata: { ...(o.timeZone === null ? {} : { timeZone: o.timeZone ?? TZ }), subjectToThresholding: true, dataLossFromOtherRow: dims.includes('firstUserDefaultChannelGroup') },
      ...(o.noQuota ? {} : { propertyQuota: { tokensPerDay: { consumed: 100, remaining: o.quotaRemaining ?? 24900 }, tokensPerHour: { consumed: 10, remaining: 4990 } } }),
    });
  };
  return { transport, seen };
}
const json = (v: unknown): HttpResponse => ({ status: 200, headers: {}, body: JSON.stringify(v) });

function rowsFor(dims: string[], metrics: string[]): { d: string[]; m: number[] }[] {
  const k = dims.join(',');
  const ms = (...v: number[]) => metrics.map((_, i) => v[i] ?? 50);
  if (k === 'date') return [{ d: ['20240609'], m: ms(120, 30, 150, 200, 140, 3600.5) }, { d: ['20240610'], m: ms(110, 25, 140, 190, 130, 3000) }];
  if (k === 'isoYearIsoWeek') return [{ d: ['202423'], m: ms(400, 90) }];
  if (k === 'yearMonth') return [{ d: ['202405'], m: ms(900, 200) }];
  if (k === 'date,eventName') return [{ d: ['20240610', 'POST_WRITE'], m: ms(80, 40) }, { d: ['20240610', 'REPLY_WRITE'], m: ms(9, 3) }, { d: ['20240610', 'UNLISTED_EVENT'], m: ms(70, 30) }];
  if (k === 'date,firstUserDefaultChannelGroup') return [{ d: ['20240610', 'Organic Search'], m: ms(40, 60, 30) }, { d: ['20240610', 'Some Free Text <script>'], m: ms(20, 25, 10) }, { d: ['20240610', '(other)'], m: ms(15, 15, 5) }, { d: ['20240610', 'Direct'], m: ms(4, 4, 1) }];
  if (k === 'date,platform,deviceCategory') return [{ d: ['20240610', 'Android', 'mobile'], m: ms(70, 12) }, { d: ['20240610', 'iOS', 'tablet'], m: ms(3, 1) }];
  if (k === 'date,newVsReturning') return [{ d: ['20240610', 'new'], m: ms(25, 20) }, { d: ['20240610', 'returning'], m: ms(85, 70) }];
  if (k === 'date,countryId') return [
    { d: ['20240610', 'KR'], m: ms(50, 8) }, { d: ['20240610', 'US'], m: ms(40, 12) }, { d: ['20240610', 'JP'], m: ms(5, 1) },
    { d: ['20240610', 'ZZZ'], m: ms(30, 10) }, { d: ['20240610', 'xx'], m: ms(20, 10) }, { d: ['20240610', '(other)'], m: ms(25, 11) },
  ];
  if (k === 'cohort,cohortNthWeek') {
    const weeks = cohortWeeks('2024-06-10');
    return weeks.flatMap((w, i) => [0, 1, 2].map((n) => ({ d: [w, String(n).padStart(4, '0')], m: ms(i === 0 ? 5 - n : 50 - n * 10, i === 0 ? 5 : 50) })));
  }
  throw new Error(`unknown request ${k}`);
}

function tmpDb(): DatabaseSync {
  const db = new DatabaseSync(':memory:');
  db.exec(readFileSync(ENGINE_TABLES_SQL, 'utf8'));
  return db;
}

test('dates: two days ago in the property time zone, complete weeks and months, last 12 complete cohort weeks', () => {
  assert.equal(dataThrough(NOW, TZ), '2024-06-10');
  assert.equal(dataThrough(Date.parse('2024-06-12T05:00:00Z'), TZ), '2024-06-09', 'still June 11 in LA');
  assert.deepEqual(completeRange('weekly', '2024-03-01', '2024-06-10'), { start: '2024-03-04', end: '2024-06-09' });
  assert.deepEqual(completeRange('monthly', '2024-03-01', '2024-06-10'), { start: '2024-03-01', end: '2024-05-31' });
  assert.deepEqual(completeRange('monthly', '2024-03-02', '2024-03-30'), null);
  const weeks = cohortWeeks('2024-06-10');
  assert.equal(weeks.length, 12);
  assert.equal(weeks.at(-1), '2024-06-02', 'last complete week: Sun 6/2 to Sat 6/8');
  assert.ok(weeks.every((w) => new Date(`${w}T00:00:00Z`).getUTCDay() === 0));
});

test('config: key file permissions and symlinks, one-to-one event map, unknown reports', () => {
  const dir = ga4Files();
  const key = join(dir, 'sa.json');
  assert.equal(loadCredentials(key).clientEmail, 'reader@example.iam.gserviceaccount.com');
  chmodSync(key, 0o644);
  assert.throws(() => loadCredentials(key), /0600/);
  chmodSync(key, 0o600);
  symlinkSync(key, join(dir, 'link.json'));
  assert.throws(() => loadCredentials(join(dir, 'link.json')), /symlinks are not allowed/);
  const dup = ga4Files({ start: '2024-03-01', reports: ['daily_events'], events: { A_EVT: 'x', B_EVT: 'x' } });
  assert.throws(() => loadReports(dup), /one-to-one/);
  assert.throws(() => loadReports(ga4Files({ start: '2024-03-01', reports: ['page_paths'] })), /unknown report/);
  assert.throws(() => loadReports(ga4Files({ start: '2024-03-01', reports: ['daily_events'] })), Ga4ConfigError);
});

test('auth: fixed endpoints only, JWT claims and signature, token_uri in the key file is ignored', async () => {
  const dir = ga4Files({ start: '2024-03-01', reports: ['daily_overview'] });
  const g = fakeGoogle();
  await importGa4(tmpDb(), loadGa4Plan(dir, conn(dir)), { transport: g.transport, now: () => NOW, sleep: async () => {} });
  const tok = g.seen.reqs.find((r) => r.url === TOKEN_URL)!;
  assert.ok(tok);
  assert.ok(!g.seen.reqs.some((r) => r.url.includes('attacker')));
  const params = new URLSearchParams(tok.body);
  assert.equal(params.get('grant_type'), 'urn:ietf:params:oauth:grant-type:jwt-bearer');
  const [h, c] = params.get('assertion')!.split('.');
  assert.deepEqual(JSON.parse(Buffer.from(h, 'base64url').toString()), { alg: 'RS256', typ: 'JWT' });
  const claims = JSON.parse(Buffer.from(c, 'base64url').toString());
  assert.deepEqual({ ...claims, iat: 0, exp: claims.exp - claims.iat }, { iss: 'reader@example.iam.gserviceaccount.com', scope: SCOPE, aud: TOKEN_URL, iat: 0, exp: 3600 });
  assert.equal(g.seen.reqs.filter((r) => r.url === TOKEN_URL).length, 1, 'token fetched once');
});

test('report transform: unknown categories, (other), event map, small groups dropped, unobserved and small cohorts dropped, complete weeks and months, collect log', async () => {
  const dir = ga4Files();
  const db = tmpDb();
  const g = fakeGoogle();
  await importGa4(db, loadGa4Plan(dir, conn(dir)), { transport: g.transport, now: () => NOW, sleep: async () => {} });
  const all = (sql: string) => db.prepare(sql).all().map((r) => ({ ...r }));
  assert.deepEqual(all('SELECT date, active_users, engagement_seconds FROM r_ga4_daily_overview ORDER BY date'), [
    { date: '2024-06-09', active_users: 120, engagement_seconds: 3600.5 }, { date: '2024-06-10', active_users: 110, engagement_seconds: 3000 },
  ]);
  assert.deepEqual(all('SELECT channel_group, new_users FROM r_ga4_daily_channel ORDER BY channel_group'), [
    { channel_group: 'Organic Search', new_users: 40 }, { channel_group: '_ga4_other', new_users: 15 }, { channel_group: '_unknown', new_users: 20 },
  ], 'Direct (4 users) is a small group and is dropped; free text is not stored');
  assert.deepEqual(all("SELECT channel_group, active_users, engaged_sessions FROM r_ga4_daily_channel WHERE channel_group = '_ga4_other'"), [{ channel_group: '_ga4_other', active_users: 15, engaged_sessions: null }], 'the group (15 users) stays; only the small value (5 engaged sessions) is blanked');
  assert.equal((db.prepare("SELECT suppressed_cells n FROM r_ga4_collect_log WHERE report = 'daily_channel'").get() as { n: number }).n, 1);
  const notnull = (t: string) => Object.fromEntries((db.prepare(`PRAGMA table_info(${t})`).all() as { name: string; notnull: number }[]).map((c) => [c.name, c.notnull]));
  assert.deepEqual(notnull('r_ga4_daily_channel'), { date: 1, channel_group: 1, new_users: 0, active_users: 0, engaged_sessions: 0 }, 'only integer metrics in breakdown tables allow NULL');
  assert.equal(notnull('r_ga4_daily_overview').new_users, 1, 'totals tables are not blanked');
  assert.deepEqual(all('SELECT event_key, event_count FROM r_ga4_daily_events'), [{ event_key: 'post_write', event_count: 80 }], 'event rows with 3 users and unmapped events are dropped');
  assert.equal((db.prepare("SELECT dropped_unmapped n FROM r_ga4_collect_log WHERE report = 'daily_events'").get() as { n: number }).n, 1);
  assert.deepEqual(all('SELECT platform, device_category FROM r_ga4_daily_platform'), [{ platform: 'Android', device_category: 'mobile' }]);
  assert.deepEqual(all('SELECT iso_year_week, period_start, period_end FROM r_ga4_weekly_users'), [{ iso_year_week: '202423', period_start: '2024-06-03', period_end: '2024-06-09' }]);
  assert.deepEqual(all('SELECT year_month, period_start, period_end FROM r_ga4_monthly_users'), [{ year_month: '202405', period_start: '2024-05-01', period_end: '2024-05-31' }]);
  const cohorts = all('SELECT cohort, cohort_nth_week FROM r_ga4_weekly_cohort');
  assert.ok(!cohorts.some((r) => r.cohort === cohortWeeks('2024-06-10')[0]), 'a 5-user cohort is dropped entirely');
  assert.ok(!cohorts.some((r) => r.cohort === '2024-06-02' && (r.cohort_nth_week as number) > 0), 'the 6/2 cohort has only week 0 fully observed');
  const log = all("SELECT report, rows, dropped_small, dropped_unobserved, subject_to_thresholding, data_loss_from_other_row, data_through FROM r_ga4_collect_log WHERE report IN ('daily_channel', 'weekly_cohort') ORDER BY report");
  assert.deepEqual(log[0], { report: 'daily_channel', rows: 3, dropped_small: 1, dropped_unobserved: 0, subject_to_thresholding: 1, data_loss_from_other_row: 1, data_through: '2024-06-10' });
  assert.equal(log[1].dropped_unobserved, 3);
  // request format
  const reqs = g.seen.reqs.filter((r) => r.url.endsWith(':runReport')).map((r) => JSON.parse(r.body));
  const weekly = reqs.find((b) => b.dimensions[0].name === 'isoYearIsoWeek');
  assert.deepEqual(weekly.dateRanges, [{ startDate: '2024-03-04', endDate: '2024-06-09' }]);
  const cohort = reqs.find((b) => b.cohortSpec);
  assert.equal(cohort.dateRanges, undefined, 'cohort requests have no top-level dateRanges');
  assert.deepEqual(cohort.cohortSpec.cohortsRange, { granularity: 'WEEKLY', startOffset: 0, endOffset: 11 });
  const events = reqs.find((b) => b.dimensions.some((d: { name: string }) => d.name === 'eventName'));
  assert.deepEqual(events.dimensionFilter.filter.inListFilter.values, ['POST_WRITE', 'REPLY_WRITE']);
  assert.ok(reqs.every((b) => b.limit === 250000 && b.returnPropertyQuota === true && b.orderBys.length === b.dimensions.length));
});

test('paging: reads up to rowCount; a repeated page fails', async () => {
  const dir = ga4Files({ start: '2024-03-01', reports: ['daily_overview'] });
  const g = fakeGoogle({ pageSize: 1 });
  const db = tmpDb();
  await importGa4(db, loadGa4Plan(dir, conn(dir)), { transport: g.transport, now: () => NOW, sleep: async () => {} });
  assert.equal((db.prepare('SELECT count(*) n FROM r_ga4_daily_overview').get() as { n: number }).n, 2);
  const offsets = g.seen.reqs.filter((r) => r.url.endsWith(':runReport')).map((r) => JSON.parse(r.body).offset);
  assert.deepEqual(offsets, [0, 250000]);
  await assert.rejects(importGa4(tmpDb(), loadGa4Plan(dir, conn(dir)), { transport: fakeGoogle({ pageSize: 1, repeatPage: true }).transport, now: () => NOW, sleep: async () => {} }), /paging/);
});

test('errors: time zone mismatch, incompatible combination, low quota, failure after retries; messages have no token, URL or body', async () => {
  const dir = ga4Files({ start: '2024-03-01', reports: ['daily_overview'] });
  const plan = loadGa4Plan(dir, conn(dir));
  const run = (o: Parameters<typeof fakeGoogle>[0]) => importGa4(tmpDb(), plan, { transport: fakeGoogle(o).transport, now: () => NOW, sleep: async () => {} });
  await assert.rejects(run({ timeZone: 'Asia/Seoul' }), /time zone differs from the config/);
  for (const compat of ['incompatible', 'missing', 'duplicate'] as const) await assert.rejects(run({ compat }), (e) => e instanceof Ga4Error && e.kind === 'incompatible', compat);
  await assert.rejects(run({ quotaRemaining: 5 }), (e) => e instanceof Ga4Error && e.kind === 'quota');
  const waits: number[] = [];
  const g = fakeGoogle({ status: [503, 503, 503, 503] });
  const err = await importGa4(tmpDb(), plan, { transport: g.transport, now: () => NOW, sleep: async (ms) => { waits.push(ms); } }).catch((e) => e);
  assert.ok(err instanceof Ga4Error && err.kind === 'http_5xx');
  assert.deepEqual(waits, [1000, 1000, 1000], 'follows Retry-After');
  for (const e of [err]) {
    assert.doesNotMatch(e.message, /ya29|token=|https?:\/\/|BEGIN|reader@/);
  }
  const auth = await importGa4(tmpDb(), plan, { transport: fakeGoogle({ status: [401] }).transport, now: () => NOW, sleep: async () => {} }).catch((e) => e);
  assert.ok(auth instanceof Ga4Error && auth.kind === 'auth');
  assert.doesNotMatch(auth.message, /secret detail/);
  for (const token of [{ access_token: 'x', token_type: 'mac', expires_in: 3600 }, { access_token: '', token_type: 'Bearer', expires_in: 3600 }, { access_token: 'x', token_type: 'Bearer', expires_in: 0 }, { access_token: 'x', token_type: 'Bearer', expires_in: 1.5 }]) {
    await assert.rejects(run({ token }), (e) => e instanceof Ga4Error && e.kind === 'auth', JSON.stringify(token));
  }
  for (const badRow of ['metrics', 'dims', 'number'] as const) await assert.rejects(run({ badRow }), /row format/, badRow);
  await assert.rejects(run({ noRowCount: true }), /rowCount/);
  const emptyDb = tmpDb();
  await importGa4(emptyDb, plan, { transport: fakeGoogle({ empty: true, noRowCount: true }).transport, now: () => NOW, sleep: async () => {} });
  assert.equal((emptyDb.prepare('SELECT count(*) n FROM r_ga4_daily_overview').get() as { n: number }).n, 0, 'a missing rowCount is allowed with 0 rows');
  await assert.rejects(run({ timeZone: null }), /time zone differs from the config/);
  await assert.rejects(run({ noQuota: true }), /missing quota info/);
});

/** Example workspace + GA4 */
function demoWithGa4(): string {
  const dir = ga4Files({ start: '2024-03-01', reports: ['daily_overview', 'daily_channel'], min_users: 10 });
  for (const f of ['tables.json', 'derived.sql', 'derived-columns.json']) cpSync(join(DEMO_DIR, f), join(dir, f));
  const ws = JSON.parse(readFileSync(join(DEMO_DIR, 'workspace.json'), 'utf8'));
  writeFileSync(join(dir, 'workspace.json'), JSON.stringify({ ...ws, ga4: { property_id: '123456', time_zone: TZ, key_file: 'sa.json' } }));
  seedDemo(join(dir, '.out', 'source.sqlite'), { anchor: '2024-06-03 12:00:00' });
  return dir;
}

test('snapshot integration: GA4 tables go into the real and agent copies, the collect log is left out of the agent copy; config changes change the ID and derive is rejected', async () => {
  const dir = demoWithGa4();
  const g = fakeGoogle();
  const r = await runCollect(loadWorkspace(dir), { ga4: { transport: g.transport, now: () => NOW, sleep: async () => {} } });
  const tables = (p: string) => (new DatabaseSync(p, { readOnly: true }).prepare("SELECT name FROM sqlite_master WHERE type = 'table'").all() as { name: string }[]).map((x) => x.name);
  assert.ok(tables(r.files.real).includes('r_ga4_daily_overview'));
  assert.ok(tables(r.files.agent).includes('r_ga4_daily_channel'));
  assert.ok(tables(r.files.real).includes('r_ga4_collect_log'));
  assert.ok(!tables(r.files.agent).includes('r_ga4_collect_log'));
  const meta = new DatabaseSync(r.files.real, { readOnly: true }).prepare('SELECT ga4_spec_hash FROM snapshot_meta').get() as { ga4_spec_hash: string };
  assert.match(meta.ga4_spec_hash, /^[0-9a-f]{64}$/);

  writeFileSync(join(dir, 'ga4-reports.json'), JSON.stringify({ start: '2024-03-01', reports: ['daily_overview'], min_users: 10 }));
  assert.throws(() => runDerive(loadWorkspace(dir)), /GA4 settings.*collect/);
  const r2 = await runCollect(loadWorkspace(dir), { ga4: { transport: fakeGoogle().transport, now: () => NOW, sleep: async () => {} } });
  assert.notEqual(r2.snapshotId.split('-').at(-1), r.snapshotId.split('-').at(-1), 'changing GA4 settings changes the snapshot ID');
});

test('a GA4 failure fails the whole collect, keeping the previous snapshot and cleaning up the lock', async () => {
  const dir = demoWithGa4();
  const ok = await runCollect(loadWorkspace(dir), { ga4: { transport: fakeGoogle().transport, now: () => NOW, sleep: async () => {} } });
  await assert.rejects(runCollect(loadWorkspace(dir), { ga4: { transport: fakeGoogle({ status: [401] }).transport, now: () => NOW, sleep: async () => {} } }), /GA4 authentication failed/);
  const snaps = join(dir, '.out', 'snapshots');
  assert.equal(readCurrent(snaps)!.snapshot_id, ok.snapshotId);
  const { readdirSync } = await import('node:fs');
  assert.deepEqual(readdirSync(snaps).filter((f) => f.startsWith('tmp-') || f === '.lock'), []);
});

test('collect fails when a public DB text column holds a secret-looking value', async () => {
  const dir = demoWithGa4();
  const db = new DatabaseSync(join(dir, '.out', 'source.sqlite'));
  db.exec("UPDATE member SET country = 'a@b.co' WHERE id = (SELECT min(id) FROM member)");
  db.close();
  await assert.rejects(runCollect(loadWorkspace(dir), { ga4: { transport: fakeGoogle().transport, now: () => NOW, sleep: async () => {} } }), /r_member\.country: has a secret-looking value \(email\)/);
});

test('call limit: more than 200 calls in one collect fails', async () => {
  const dir = ga4Files({ start: '2024-03-01', reports: ['daily_overview'] });
  const g = fakeGoogle({ status: Array(400).fill(503) });
  const plan = loadGa4Plan(dir, conn(dir));
  const { Ga4Client, CLIENT_LIMITS } = await import('../src/collect/ga4/client.ts');
  const { loadCredentials } = await import('../src/collect/ga4/config.ts');
  const c = new Ga4Client(loadCredentials(plan.conn.keyFile), '123456', { transport: g.transport, sleep: async () => {} });
  let err: unknown;
  for (let i = 0; i < 100 && !err; i++) err = await c.runReport({}, 'x').then(() => null, (e) => (e instanceof Ga4Error && e.kind === 'limit' ? e : null));
  assert.ok(err, 'limit error');
  assert.equal(c.calls, CLIENT_LIMITS.maxCalls);
});

test('config error messages contain no file paths', () => {
  const dir = ga4Files();
  chmodSync(join(dir, 'sa.json'), 0o644);
  const plan = loadGa4Plan(dir, conn(dir));
  return import('../src/collect/ga4/config.ts').then(({ loadCredentials }) => {
    assert.throws(() => loadCredentials(plan.conn.keyFile), (e: Error) => /0600/.test(e.message) && !e.message.includes(dir));
  });
});

test('custom report config: allow-list, counts, duplicates, fine dimensions, events, group-size metrics', () => {
  const base = { start: '2024-03-01', reports: [], events: { POST_WRITE: 'post_write' } };
  const load = (custom: unknown[], extra: Record<string, unknown> = {}) => loadReports(ga4Files({ ...base, ...extra, custom }));
  const ok = load([{ id: 'daily_country', range: 'daily', dimensions: ['countryId'], metrics: ['activeUsers', 'newUsers'] }, { id: 'weekly_os', range: 'weekly', dimensions: ['operatingSystem', 'deviceCategory'], metrics: ['activeUsers', 'sessions', 'engagementRate'] }]);
  assert.equal(ok.custom.length, 2);
  const bad: [unknown, RegExp][] = [
    [{ id: 'a', range: 'daily', dimensions: ['appVersion'], metrics: ['activeUsers'] }, /not allowed: appVersion/],
    [{ id: 'a', range: 'daily', dimensions: ['pagePath'], metrics: ['activeUsers'] }, /not allowed: pagePath/],
    [{ id: 'a', range: 'daily', dimensions: ['countryId', 'hour'], metrics: ['activeUsers'] }, /one fine dimension/],
    [{ id: 'a', range: 'daily', dimensions: ['countryId'], metrics: ['activeUsers', 'sessions'] }, /user-count metrics only/],
    [{ id: 'a', range: 'daily', dimensions: ['platform', 'deviceCategory', 'operatingSystem'], metrics: ['activeUsers'] }, /0-2 strings/],
    [{ id: 'a', range: 'daily', dimensions: ['platform', 'platform'], metrics: ['activeUsers'] }, /duplicate/],
    [{ id: 'a', range: 'daily', dimensions: ['platform'], metrics: ['newUsers'] }, /activeUsers or totalUsers/],
    [{ id: 'a', range: 'daily', dimensions: ['eventName'], metrics: ['activeUsers', 'eventCount'] }, /needs totalUsers/],
    [{ id: 'a', range: 'daily', dimensions: ['eventName'], metrics: ['totalUsers', 'sessions'] }, /metrics not allowed with eventName/],
    [{ id: 'a', range: 'daily', dimensions: [], metrics: ['cohortActiveUsers'] }, /not allowed:/],
    [{ id: 'daily_overview', range: 'daily', dimensions: [], metrics: ['activeUsers'] }, /clashes with another report/],
    [{ id: 'a', range: 'cohort', dimensions: [], metrics: ['activeUsers'] }, /range/],
    [{ id: 'A b', range: 'daily', dimensions: [], metrics: ['activeUsers'] }, /lowercase identifier/],
    [{ id: 'a', range: 'daily', dimensions: [], metrics: ['activeUsers'], filter: 'x' }, /unknown key/],
  ];
  for (const [c, re] of bad) assert.throws(() => load([c]), re, JSON.stringify(c));
  assert.throws(() => load([{ id: 'a', range: 'daily', dimensions: [], metrics: ['activeUsers'] }, { id: 'a', range: 'daily', dimensions: [], metrics: ['newUsers'] }]), /clashes with another report/);
  assert.throws(() => loadReports(ga4Files({ start: '2024-03-01', reports: [], custom: [{ id: 'e', range: 'daily', dimensions: ['eventName'], metrics: ['totalUsers'] }] })), /events map/);
  assert.throws(() => loadReports(ga4Files({ start: '2024-03-01', reports: [] })), /at least one report/);
});

test('custom report collect: table and log keys, malformed values become _unknown, only normalization collisions dropped, small values blanked', async () => {
  const dir = ga4Files({ start: '2024-03-01', reports: [], custom: [{ id: 'daily_country', range: 'daily', dimensions: ['countryId'], metrics: ['activeUsers', 'newUsers'] }], min_users: 10 });
  const db = tmpDb();
  await importGa4(db, loadGa4Plan(dir, conn(dir)), { transport: fakeGoogle().transport, now: () => NOW, sleep: async () => {} });
  const rows = db.prepare('SELECT country_id, active_users, new_users FROM r_ga4_x_daily_country ORDER BY country_id').all().map((r) => ({ ...r }));
  assert.deepEqual(rows, [
    { country_id: 'KR', active_users: 50, new_users: null },
    { country_id: 'US', active_users: 40, new_users: 12 },
    { country_id: '_ga4_other', active_users: 25, new_users: 11 },
  ], 'JP (5 users) is dropped; ZZZ and xx both become _unknown and are dropped as a collision');
  const log = { ...db.prepare("SELECT report, table_name, rows, dropped_small, dropped_collision, suppressed_cells FROM r_ga4_collect_log").get() as object };
  assert.deepEqual(log, { report: 'custom:daily_country', table_name: 'r_ga4_x_daily_country', rows: 3, dropped_small: 1, dropped_collision: 2, suppressed_cells: 1 });
});

test('transform: raw duplicates fail, normalization collisions fail built-in reports, out-of-range values fail', () => {
  const o = { events: {}, minUsers: 10, through: '2024-06-10' };
  const types = ['TYPE_INTEGER', 'TYPE_INTEGER', 'TYPE_INTEGER'];
  const ch = REPORT_KINDS.daily_channel;
  assert.throws(() => transformRows(ch, [{ dims: ['20240610', 'Direct'], metrics: ['20', '20', '20'], types }, { dims: ['20240610', 'Direct'], metrics: ['30', '30', '30'], types }], o), /same raw key/);
  assert.throws(() => transformRows(ch, [{ dims: ['20240610', 'free a'], metrics: ['20', '20', '20'], types }, { dims: ['20240610', 'free b'], metrics: ['30', '30', '30'], types }], o), /same key/);
  assert.throws(() => transformRows(ch, [{ dims: ['20240610', 'Direct'], metrics: ['-1', '20', '20'], types }], o), /out of range/);
  const rate = customDef({ id: 'r', range: 'daily', dimensions: [], metrics: ['activeUsers', 'engagementRate'] });
  assert.throws(() => transformRows(rate, [{ dims: ['20240610'], metrics: ['20', '1.5'], types: ['TYPE_INTEGER', 'TYPE_FLOAT'] }], o), /out of range/);
  assert.equal(transformRows(rate, [{ dims: ['20240610'], metrics: ['3', '0.5'], types: ['TYPE_INTEGER', 'TYPE_FLOAT'] }], o).rows[0][1], 3, 'tables without a breakdown keep small values');
});

test('hash: custom report order does not matter, dimension and metric order does; the raw hash accepts only the planned GA4 tables', () => {
  const conn = { propertyId: '1', timeZone: TZ, keyFile: '/k' };
  const a = { id: 'a', range: 'daily' as const, dimensions: ['platform', 'deviceCategory'], metrics: ['activeUsers'] };
  const b = { id: 'b', range: 'weekly' as const, dimensions: [], metrics: ['activeUsers', 'newUsers'] };
  const rep = (custom: typeof a[]) => ({ start: '2024-03-01', reports: [], events: {}, minUsers: 10, custom });
  assert.equal(ga4SpecHash(conn, rep([a, b])), ga4SpecHash(conn, rep([b, a])));
  assert.notEqual(ga4SpecHash(conn, rep([a, b])), ga4SpecHash(conn, rep([{ ...a, dimensions: ['deviceCategory', 'platform'] }, b])));
  const db = tmpDb();
  db.exec('CREATE TABLE r_ga4_daily_overview (date TEXT NOT NULL PRIMARY KEY); CREATE TABLE r_ga4_x_extra (date TEXT NOT NULL PRIMARY KEY)');
  assert.throws(() => rawHash(db, [], ['r_ga4_daily_overview']), /differ from the plan/);
  const h1 = rawHash(db, [], ['r_ga4_daily_overview', 'r_ga4_x_extra']);
  db.exec('DROP TABLE r_ga4_x_extra; CREATE TABLE r_ga4_x_extra (date TEXT PRIMARY KEY)');
  assert.notEqual(rawHash(db, [], ['r_ga4_daily_overview', 'r_ga4_x_extra']), h1, 'changing the schema (NULL allowed) changes the hash');
});

test('collection spec: table names starting with r_ga4_ are reserved for GA4', () => {
  assert.throws(() => parseSpec([{ source: 's', target: 'r_ga4_x', key: ['id'], cutoffColumn: 'c', columns: [{ expr: 'id', as: 'id', kind: 'int', role: 'ordinary' }, { expr: 'c', as: 'c', kind: 'ts', role: 'ordinary' }] }]), /GA4/);
});
