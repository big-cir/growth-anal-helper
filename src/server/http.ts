// Local HTTP server: static files, JSON API, SSE.
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { loadSeedPanels, type App } from './app.ts';
import { ConversationHub, ID_RE, type Conversation, type ServerEvent } from './conversations.ts';
import { Conflict, NotFound, PanelService, makeResult, type PanelEvent } from './panels.ts';
import { AuthService, atLeast } from '../auth/service.ts';
import { AuditLog, type AuditRecord } from '../auth/audit.ts';
import { clearCookie, sessionCookie, type Session } from '../auth/sessions.ts';
import type { Role } from '../auth/accounts.ts';
import type { SavedPanel } from '../panels/store.ts';
import { secretAssignment, secretShape, sensitiveTopic } from '../agent/sensitive.ts';
import type { PanelSpec } from '../panels/spec.ts';
import { PATTERN_CONTRACT_VERSION } from '../panels/contract.ts';
import { RENDERER_VERSION } from '../panels/hash.ts';
import { tr } from '../i18n.ts';

const WEB_DIR = new URL('../../web/', import.meta.url).pathname;
/** Screen files served without sign-in (this list only) */
const STATIC: Record<string, { file: string; type: string }> = {
  '/': { file: 'index.html', type: 'text/html; charset=utf-8' },
  '/index.html': { file: 'index.html', type: 'text/html; charset=utf-8' },
  '/app.js': { file: 'app.js', type: 'text/javascript; charset=utf-8' },
  '/charts.js': { file: 'charts.js', type: 'text/javascript; charset=utf-8' },
  '/i18n.js': { file: 'i18n.js', type: 'text/javascript; charset=utf-8' },
  '/styles.css': { file: 'styles.css', type: 'text/css; charset=utf-8' },
};
export const LIMITS = { bodyBytes: 64 * 1024, text: 2000, title: 60, description: 300, summary: 2000 };
const SECURITY_HEADERS = {
  'Content-Security-Policy': "default-src 'self'; object-src 'none'; manifest-src 'none'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
  'X-Content-Type-Options': 'nosniff',
  'Referrer-Policy': 'no-referrer',
  'Cache-Control': 'no-store',
};

class HttpError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

function staticMap(): Map<string, { body: Buffer; type: string }> {
  return new Map(Object.entries(STATIC).map(([path, f]) => [path, { body: readFileSync(join(WEB_DIR, f.file)), type: f.type }]));
}

async function readJson(req: IncomingMessage): Promise<Record<string, unknown>> {
  const ct = (req.headers['content-type'] ?? '').split(';')[0].trim().toLowerCase();
  if (ct !== 'application/json') throw new HttpError(415, tr('Content-Type must be application/json', 'Content-Type은 application/json이어야 함'));
  const chunks: Buffer[] = [];
  let n = 0;
  for await (const c of req) {
    n += (c as Buffer).length;
    if (n > LIMITS.bodyBytes) throw new HttpError(413, tr('Body is larger than 64KB', '본문이 64KB를 넘음'));
    chunks.push(c as Buffer);
  }
  let v: unknown;
  try {
    v = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
  } catch {
    throw new HttpError(400, tr('Invalid JSON', 'JSON 형식 오류'));
  }
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw new HttpError(400, tr('Body must be a JSON object', '본문은 JSON 객체여야 함'));
  return v as Record<string, unknown>;
}

function str(v: unknown, name: string, max: number): string {
  if (typeof v !== 'string' || v.trim() === '') throw new HttpError(400, tr(`${name}: must be a non-empty string`, `${name}: 비어 있지 않은 문자열`));
  if ([...v].length > max) throw new HttpError(400, tr(`${name}: ${max} characters or fewer`, `${name}: ${max}자 이하`));
  return v;
}

function optStr(v: unknown, name: string, max: number): string {
  if (v === undefined || v === null) return '';
  if (typeof v !== 'string') throw new HttpError(400, tr(`${name}: must be a string`, `${name}: 문자열`));
  if ([...v].length > max) throw new HttpError(400, tr(`${name}: ${max} characters or fewer`, `${name}: ${max}자 이하`));
  return v.trim();
}

function idField(v: unknown, name: string): string {
  if (typeof v !== 'string' || !ID_RE.test(v)) throw new HttpError(400, tr(`${name}: invalid format`, `${name} 형식 오류`));
  return v;
}

function hashField(v: unknown): string {
  if (typeof v !== 'string' || !/^[0-9a-f]{64}$/.test(v)) throw new HttpError(400, tr('preview_hash: invalid format', 'preview_hash 형식 오류'));
  return v;
}

/** Whether text shown on screen or sent back to the agent looks like a secret */
const secretLike = (text: string) => secretShape(text) !== null || secretAssignment(text);
const specSecretLike = (spec: PanelSpec) => secretLike(JSON.stringify(spec));

function sseHead(res: ServerResponse, headers: Record<string, string>): void {
  res.writeHead(200, { ...headers, 'Content-Type': 'text/event-stream; charset=utf-8', Connection: 'keep-alive' });
  res.write('retry: 2000\n\n');
}

type Ctx = { req: IncomingMessage; res: ServerResponse; params: string[]; body: () => Promise<Record<string, unknown>>; user: Session; ip: string };
type PublicCtx = Omit<Ctx, 'user' | 'params'>;
type Route =
  | { method: string; pattern: RegExp; access: Role; handler: (ctx: Ctx) => Promise<void> | void }
  | { method: string; pattern: RegExp; access: 'public'; handler: (ctx: PublicCtx) => Promise<void> | void };

/** Startup check: access is required, no duplicate method+pattern, public only for login/logout/me */
export function checkRoutes(routes: Route[]): void {
  const seen = new Set<string>();
  for (const r of routes) {
    if (!['public', 'viewer', 'editor', 'admin'].includes(r.access)) throw new Error(`Route has no access level: ${r.method} ${r.pattern}`);
    const k = `${r.method} ${r.pattern.source}`;
    if (seen.has(k)) throw new Error(`Duplicate route: ${k}`);
    seen.add(k);
    if (r.access === 'public' && !/^\^\\\/api\\\/(login|logout|me)\$$/.test(r.pattern.source)) throw new Error(`Only login/logout/me may be public: ${r.pattern}`);
  }
}

const LOCAL_USER: Session = { username: 'local', role: 'admin', key: 'local', created: 0, lastSeen: 0 };

export type ServerHandle = { server: Server; port: number; panels: PanelService; auth: AuthService; close(): Promise<void> };


export function startServer(app: App, port: number): Promise<ServerHandle> {
  const cfg = app.ws.config;
  const files = staticMap();
  for (const p of ['/', '/index.html']) {
    const f = files.get(p);
    if (f) f.body = Buffer.from(f.body.toString('utf8').replace('<html lang="ko">', `<html lang="${cfg.language}">`));
  }
  const audit = new AuditLog(cfg.outDir);
  audit.prune(cfg.server.auditRetentionDays);
  app.audit = audit;
  const auth = new AuthService({ outDir: cfg.outDir, audit, proxyHops: cfg.server.publicOrigin ? cfg.server.proxyHops : null, requireAccounts: cfg.server.auth });
  const hub = new ConversationHub(app);
  const panels = new PanelService(app);
  const external = cfg.server.publicOrigin;
  // With auth off, every request is the admin `local`
  const sessionOf = (req: IncomingMessage): Session | null => (cfg.server.auth ? auth.session(req) : LOCAL_USER);
  const allowedHosts = new Set<string>();
  const allowedOrigins = new Set<string>();
  const allowLoopback = (p: number) => {
    allowedHosts.add(`127.0.0.1:${p}`).add(`localhost:${p}`);
    allowedOrigins.add(`http://127.0.0.1:${p}`).add(`http://localhost:${p}`);
  };
  if (external) {
    allowedHosts.add(new URL(external).host);
    allowedOrigins.add(external);
  } else {
    allowLoopback(port);
  }
  const headers: Record<string, string> = external ? { ...SECURITY_HEADERS, 'Strict-Transport-Security': 'max-age=31536000' } : SECURITY_HEADERS;
  const sendJson = (res: ServerResponse, status: number, body: unknown, extra: Record<string, string> = {}) => {
    const text = JSON.stringify(body);
    res.writeHead(status, { ...headers, ...extra, 'Content-Type': 'application/json; charset=utf-8', 'Content-Length': Buffer.byteLength(text) });
    res.end(text);
  };

  const metricNames = () => {
    try {
      return app.metrics().dict.metrics.map((m) => ({ id: m.id, name: m.name }));
    } catch {
      return [];
    }
  };
  /** Own conversations only (ownerless legacy ones: admin). Otherwise 404 even if it exists */
  const ownedConversation = (user: Session, id: string): Conversation => {
    const c = hub.get(id);
    if (!c || !(c.owner === user.username || (c.owner === null && user.role === 'admin'))) throw new HttpError(404, tr('No such conversation', '없는 대화'));
    return c;
  };
  const canWritePanel = (user: Session, p: SavedPanel) => user.role === 'admin' || (user.role === 'editor' && p.created_by === user.username);
  const ownedPanelForWrite = (user: Session, id: string): SavedPanel => {
    const p = panels.mustGet(id);
    if (!canWritePanel(user, p)) throw new HttpError(403, tr('You can only change panels you saved', '자기가 저장한 패널만 바꿀 수 있어요'));
    return p;
  };
  const panelView = (user: Session, p: SavedPanel) => panels.view(p, app.snapshot(), canWritePanel(user, p));
  /** Write the audit record before the change; if that fails, do not change */
  const audited = async <T>(r: AuditRecord, fn: () => Promise<T> | T): Promise<T> => {
    try {
      audit.write(r);
    } catch (e) {
      console.error(`Audit log write failed: ${(e as Error).message}`);
      throw new HttpError(500, tr('Request refused: the audit log could not be written', '감사 기록을 남기지 못해 요청을 거부했어요'));
    }
    try {
      return await fn();
    } catch (e) {
      audit.tryWrite({ ...r, event: `${r.event}_failed` as AuditRecord['event'] });
      throw e;
    }
  };
  /** SSE: bound to the session; session and role are rechecked on every heartbeat */
  const stream = (req: IncomingMessage, res: ServerResponse, user: Session, still: () => boolean, cleanup: () => void) => {
    sseHead(res, headers);
    let closed = false;
    const end = () => {
      if (closed) return;
      closed = true;
      clearInterval(hb);
      unregister();
      cleanup();
      res.end();
    };
    const unregister = auth.registerStream(user.key, end);
    const hb = setInterval(() => {
      if ((cfg.server.auth && !auth.stillValid(user.key)) || !still()) return end();
      res.write(': heartbeat\n\n');
    }, 15_000);
    req.on('close', end);
  };

  const routes: Route[] = [
    { method: 'POST', pattern: /^\/api\/login$/, access: 'public', handler: async ({ req, res, body }) => {
      if (!cfg.server.auth) throw new HttpError(400, tr('Sign-in is turned off', '인증이 꺼져 있어요'));
      const b = await body();
      const username = typeof b.username === 'string' ? b.username : '';
      const password = typeof b.password === 'string' ? b.password : '';
      let r: Awaited<ReturnType<AuthService['login']>>;
      try {
        r = await auth.login(req, username, password);
      } catch (e) {
        console.error(`Sign-in failed: ${(e as Error).message}`);
        throw new HttpError(500, tr('Sign-in refused: the audit log could not be written', '감사 기록을 남기지 못해 로그인을 거부했어요'));
      }
      if (!r.ok) {
        if (r.status === 429) return sendJson(res, 429, { error: tr('Please try again in a moment', '잠시 뒤 다시 시도해 주세요') }, { 'Retry-After': String(Math.ceil((r.retryAfterMs ?? 1000) / 1000)) });
        return sendJson(res, 401, { error: tr('Wrong username or password', '아이디 또는 비밀번호가 맞지 않아요') });
      }
      sendJson(res, 200, { username: r.user.username, role: r.user.role }, { 'Set-Cookie': sessionCookie(r.sessionId, !!external) });
    } },
    { method: 'POST', pattern: /^\/api\/logout$/, access: 'public', handler: ({ req, res }) => {
      try {
        auth.logout(req);
      } catch (e) {
        console.error(`Audit log write failed: ${(e as Error).message}`);
        throw new HttpError(500, tr('Request refused: the audit log could not be written', '감사 기록을 남기지 못해 요청을 거부했어요'));
      }
      sendJson(res, 200, { ok: true }, { 'Set-Cookie': clearCookie(!!external) });
    } },
    { method: 'GET', pattern: /^\/api\/me$/, access: 'public', handler: ({ req, res }) => {
      const s = sessionOf(req);
      if (!s) return sendJson(res, 401, { error: tr('Sign-in required', '로그인이 필요해요') });
      sendJson(res, 200, { username: s.username, role: s.role, auth: cfg.server.auth });
    } },
    { method: 'GET', pattern: /^\/api\/state$/, access: 'viewer', handler: ({ res, user }) => {
      const snap = app.snapshot();
      const canAgent = atLeast(user, 'editor');
      sendJson(res, 200, {
        snapshot: snap ? { snapshot_id: snap.id, as_of: snap.asOf } : null,
        agent: canAgent ? app.agent : null,
        data_mode: cfg.agent.dataMode,
        workspace: cfg.name,
        dashboard_count: panels.store.list().length,
        suggestions: canAgent ? loadSeedPanels(app.ws.dir).panels.slice(0, 3).map((p) => p.spec.question) : [],
        metrics: metricNames(),
      });
    } },
    { method: 'POST', pattern: /^\/api\/conversations$/, access: 'editor', handler: async ({ res, body, user }) => {
      await body();
      sendJson(res, 201, { conversation_id: hub.create(user.username).id });
    } },
    // My latest conversation to reopen after sign-in (null if none)
    { method: 'GET', pattern: /^\/api\/conversations$/, access: 'editor', handler: ({ res, user }) => sendJson(res, 200, { conversation_id: hub.latestFor(user) }) },
    { method: 'GET', pattern: /^\/api\/conversations\/([^/]+)$/, access: 'editor', handler: ({ res, params, user }) => sendJson(res, 200, ownedConversation(user, params[0]).state()) },
    { method: 'POST', pattern: /^\/api\/conversations\/([^/]+)\/messages$/, access: 'editor', handler: async ({ res, params, body, user, ip }) => {
      const c = ownedConversation(user, params[0]);
      const b = await body();
      const text = str(b.text, 'text', LIMITS.text);
      const id = await audited({ event: 'agent_request', user: user.username, ip, target: c.id }, () => c.submit(text));
      sendJson(res, 202, { request_id: id });
    } },
    { method: 'POST', pattern: /^\/api\/conversations\/([^/]+)\/stop$/, access: 'editor', handler: async ({ res, params, body, user }) => {
      const c = ownedConversation(user, params[0]);
      await body();
      sendJson(res, 200, { stopped: await c.stop() });
    } },
    { method: 'POST', pattern: /^\/api\/conversations\/([^/]+)\/answers$/, access: 'editor', handler: async ({ res, params, body, user }) => {
      const c = ownedConversation(user, params[0]);
      const b = await body();
      const requestId = idField(b.request_id, 'request_id');
      if (!Number.isInteger(b.turn_no)) throw new HttpError(400, tr('turn_no must be an integer', 'turn_no는 정수'));
      const raw = b.answers;
      if (!raw || typeof raw !== 'object' || Array.isArray(raw)) throw new HttpError(400, tr('answers must be an object', 'answers는 객체'));
      const answers: Record<string, string> = {};
      for (const [k, v] of Object.entries(raw)) {
        if (!/^[a-z_]{1,32}$/.test(k)) throw new HttpError(400, tr(`Invalid answers key: ${k}`, `answers 키 형식 오류: ${k}`));
        if (v === null || v === undefined || v === '') continue;
        answers[k] = str(v, `answers.${k}`, 300);
      }
      if (!c.answer(requestId, b.turn_no as number, answers)) throw new HttpError(409, tr('Not expecting an answer now (already answered or another request)', '지금 받을 수 있는 답이 아님(이미 답했거나 다른 요청)'));
      sendJson(res, 202, { ok: true });
    } },
    { method: 'POST', pattern: /^\/api\/conversations\/([^/]+)\/offdict$/, access: 'editor', handler: async ({ res, params, body, user, ip }) => {
      const c = ownedConversation(user, params[0]);
      const b = await body();
      if (!Number.isInteger(b.turn_no)) throw new HttpError(400, tr('turn_no must be an integer', 'turn_no는 정수'));
      if (typeof b.approve !== 'boolean') throw new HttpError(400, tr('approve must be true or false', 'approve는 true/false'));
      const requestId = idField(b.request_id, 'request_id');
      const run = () => {
        if (!c.offdict(requestId, b.turn_no as number, b.approve as boolean)) throw new HttpError(409, tr('Not expecting an approval now (already answered or another request)', '지금 받을 수 있는 승인이 아님(이미 답했거나 다른 요청)'));
      };
      if (b.approve) await audited({ event: 'offdict_approved', user: user.username, ip, target: c.id }, run);
      else run();
      sendJson(res, 202, { ok: true });
    } },
    { method: 'GET', pattern: /^\/api\/conversations\/([^/]+)\/events$/, access: 'editor', handler: ({ req, res, params, user }) => {
      const c = ownedConversation(user, params[0]);
      // Resend events after Last-Event-ID or ?since=N
      const header = req.headers['last-event-id'];
      const since = new URL(req.url ?? '', 'http://x').searchParams.get('since');
      const last = Number(header ?? since ?? NaN);
      const write = (e: ServerEvent) => res.write(`id: ${e.event_id}\nevent: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`);
      const sub = c.subscribe(write, Number.isInteger(last) && last >= 0 ? last : null);
      stream(req, res, user, () => true, () => sub.unsubscribe());
      if (sub.replay === 'resync') res.write(`event: resync\ndata: {}\n\n`);
      else for (const e of sub.replay) write(e);
    } },
    { method: 'POST', pattern: /^\/api\/conversations\/([^/]+)\/save-draft$/, access: 'editor', handler: async ({ res, params, body, user }) => {
      const c = ownedConversation(user, params[0]);
      const b = await body();
      const hit = c.completedPreview(idField(b.request_id, 'request_id'), hashField(b.preview_hash));
      if (!hit) throw new HttpError(409, tr('This is not the current preview (a newer request finished or changed it)', '현재 미리보기가 아니에요(새 요청이 끝났거나 바뀜)'));
      const s = hit.preview.spec;
      if (specSecretLike(s)) throw new HttpError(409, tr('This tool cannot handle this data', '이 도구가 다룰 수 없는 데이터예요'));
      const base = { title: s.title, description: [...s.question].slice(0, LIMITS.description).join('') };
      if (cfg.agent.dataMode === 'schema_only') return sendJson(res, 200, { ...base, summary: '', summary_status: 'disabled' });
      if (!hit.agentRows) return sendJson(res, 200, { ...base, summary: '', summary_status: 'failed', message: tr('No pseudonymized result, so no description can be written', '가명 결과가 없어 설명을 만들 수 없어요') });
      if (c.drafting) throw new HttpError(409, tr('The description is being written', '설명을 만드는 중이에요'));
      c.drafting = true;
      try {
        const r = await app.summarize(s, hit.preview.columns, hit.agentRows);
        sendJson(res, 200, r.status === 'ok' ? { ...base, summary: r.text, summary_status: 'ok' } : { ...base, summary: '', summary_status: 'failed', message: r.message });
      } finally {
        c.drafting = false;
      }
    } },
    { method: 'GET', pattern: /^\/api\/panels\/events$/, access: 'viewer', handler: ({ req, res, user }) => {
      const admin = user.role === 'admin';
      const write = (e: PanelEvent) => {
        if (e.type === 'quality_status' && !admin) return;
        res.write(`event: ${e.type}\ndata: ${JSON.stringify(e.data)}\n\n`);
      };
      const unsubscribe = panels.subscribe(write);
      stream(req, res, user, () => true, unsubscribe);
      write({ type: 'panel_status_all', data: { panels: panels.allStatus() } });
    } },
    { method: 'GET', pattern: /^\/api\/panels$/, access: 'viewer', handler: ({ res, user }) => {
      sendJson(res, 200, { panels: panels.store.list().map((p) => panelView(user, p)) });
    } },
    { method: 'POST', pattern: /^\/api\/panels$/, access: 'editor', handler: async ({ res, body, user, ip }) => {
      const b = await body();
      const c = ownedConversation(user, idField(b.conversation_id, 'conversation_id'));
      const hit = c.completedPreview(idField(b.request_id, 'request_id'), hashField(b.preview_hash));
      if (!hit) throw new HttpError(409, tr('This is not the current preview (a newer request finished or changed it)', '현재 미리보기가 아니에요(새 요청이 끝났거나 바뀜)'));
      const p = hit.preview;
      const summary = optStr(b.summary, 'summary', LIMITS.summary);
      const title = str(b.title, 'title', LIMITS.title).trim();
      const description = optStr(b.description, 'description', LIMITS.description);
      if (secretLike(`${title}\n${description}\n${summary}`) || specSecretLike(p.spec)) throw new HttpError(400, tr('Not saved: something looks like a secret', '비밀처럼 보이는 값이 있어 저장하지 않았어요'));
      const { panel, created } = await audited({ event: 'panel_saved', user: user.username, ip, target: c.id }, () => panels.create({
        title, description, summary,
        summary_snapshot_id: summary ? p.snapshot_id : null,
        prompt: c.firstPrompt(),
        spec: p.spec,
        versions: { ...p.versions, pattern_contract_version: PATTERN_CONTRACT_VERSION, renderer_version: RENDERER_VERSION },
        generated_model: cfg.agent.model ?? 'default',
        preview_hash: p.preview_hash,
        last_result: makeResult(p.spec, { id: p.snapshot_id, asOf: p.as_of }, 'preview', p.columns, p.rows, p.tables ?? []),
        created_by: user.username,
      }));
      const old = c.origin ? panels.store.get(c.origin.panel_id) : null;
      const replaces = old && old.id !== panel.id && canWritePanel(user, old) ? old.id : null;
      sendJson(res, created ? 201 : 200, { panel: panelView(user, panel), created, replaces });
    } },
    { method: 'GET', pattern: /^\/api\/panels\/([^/]+)$/, access: 'viewer', handler: ({ res, params, user }) => sendJson(res, 200, { panel: panelView(user, panels.mustGet(params[0])) }) },
    { method: 'DELETE', pattern: /^\/api\/panels\/([^/]+)$/, access: 'editor', handler: async ({ res, params, user, ip }) => {
      const p = ownedPanelForWrite(user, params[0]);
      await audited({ event: 'panel_deleted', user: user.username, ip, target: p.id }, () => panels.delete(p.id));
      sendJson(res, 200, { deleted: true });
    } },
    { method: 'POST', pattern: /^\/api\/panels\/([^/]+)\/recompute$/, access: 'editor', handler: async ({ res, params, body, user, ip }) => {
      await body();
      const p = ownedPanelForWrite(user, params[0]);
      sendJson(res, 202, { job_status: await audited({ event: 'recompute', user: user.username, ip, target: p.id }, () => panels.recompute(p.id)) });
    } },
    { method: 'POST', pattern: /^\/api\/panels\/([^/]+)\/recompute\/cancel$/, access: 'editor', handler: async ({ res, params, body, user }) => {
      await body();
      const p = ownedPanelForWrite(user, params[0]);
      sendJson(res, 200, { job_status: await panels.cancel(p.id) });
    } },
    { method: 'POST', pattern: /^\/api\/panels\/([^/]+)\/regenerate$/, access: 'editor', handler: async ({ res, params, body, user, ip }) => {
      await body();
      const p = ownedPanelForWrite(user, params[0]);
      const answersText = p.spec.answers.map((a) => `${a.question} ${a.answer}`).join(' ');
      if (sensitiveTopic(`${p.prompt} ${answersText}`) !== null || secretLike(`${p.prompt}\n${answersText}`)) {
        audit.tryWrite({ event: 'sensitive_blocked', user: user.username, ip, target: p.id });
        throw new HttpError(409, tr('This tool does not handle credentials, tokens, connection details, settings or personal contacts', '이 도구는 인증 정보·토큰·연결 정보·설정·개인 연락처를 다루지 않아요'));
      }
      const id = await audited({ event: 'regenerate', user: user.username, ip, target: p.id }, () => {
        const c = hub.create(user.username);
        c.setOrigin(p.id, p.prompt);
        const defs = p.spec.answers.map((a) => `- ${a.question}: ${a.answer}`).join('\n');
        const text = `${p.prompt}\n\n${tr('Rebuild it using the previously chosen definitions as defaults.', '이전에 정한 정의를 기본값으로 다시 만들어 주세요.')}${defs ? `\n${defs}` : ''}`;
        c.submit([...text].slice(0, LIMITS.text).join(''));
        return c.id;
      });
      sendJson(res, 201, { conversation_id: id });
    } },
    { method: 'POST', pattern: /^\/api\/panels\/([^/]+)\/resummarize$/, access: 'editor', handler: async ({ res, params, body, user, ip }) => {
      await body();
      const p0 = ownedPanelForWrite(user, params[0]);
      if (specSecretLike(p0.spec)) throw new HttpError(409, tr('This tool cannot handle this data', '이 도구가 다룰 수 없는 데이터예요'));
      if (cfg.agent.dataMode === 'schema_only') throw new HttpError(409, tr('No description in this mode: result values are not sent', '결과 값을 보내지 않는 모드라 설명을 만들지 않아요'));
      const out = await audited({ event: 'resummarize', user: user.username, ip, target: p0.id }, () => panels.exclusive(p0.id, async (p) => {
        const v = await panels.verifyLastResult(p);
        const r = await app.summarize(p.spec, v.columns, v.agentRows);
        if (r.status !== 'ok') return { summary_status: 'failed', message: r.message };
        const next = panels.updateSummary(p.id, r.text, p.last_result.snapshot_id);
        return { summary_status: 'ok', panel: panelView(user, next) };
      }));
      sendJson(res, 200, out);
    } },
    { method: 'GET', pattern: /^\/api\/quality$/, access: 'admin', handler: ({ res }) => sendJson(res, 200, panels.quality) },
  ];
  checkRoutes(routes);
  const publicRoutes = routes.filter((r) => r.access === 'public');
  const privateRoutes = routes.filter((r) => r.access !== 'public') as Extract<Route, { access: Role }>[];

  const server = createServer(async (req, res) => {
    try {
      if (!allowedHosts.has(req.headers.host ?? '')) throw new HttpError(403, tr('Host not allowed', 'Host 거부'));
      const method = req.method ?? 'GET';
      if (method !== 'GET' && method !== 'HEAD') {
        if (!allowedOrigins.has(req.headers.origin ?? '') || req.headers['x-growth-lab'] !== '1') throw new HttpError(403, tr('Not a same-origin request', '같은 출처 요청이 아님'));
      }
      let path: string;
      try {
        path = new URL(req.url ?? '/', 'http://127.0.0.1').pathname;
      } catch {
        throw new HttpError(400, tr('Bad path', '잘못된 경로'));
      }
      if (!path.startsWith('/api/')) {
        const f = files.get(path);
        if (!f) throw new HttpError(404, tr('No such path', '없는 경로'));
        if (method !== 'GET') throw new HttpError(405, tr('Method not allowed', '허용되지 않는 메서드'));
        res.writeHead(200, { ...headers, 'Content-Type': f.type, 'Content-Length': f.body.length });
        res.end(f.body);
        return;
      }
      let parsed: Record<string, unknown> | null = null;
      const body = async () => (parsed ??= await readJson(req));
      const ip = auth.ip(req);
      const pub = publicRoutes.find((r) => r.method === method && r.pattern.test(path));
      if (pub) {
        await (pub.handler as (c: PublicCtx) => Promise<void> | void)({ req, res, body, ip });
        return;
      }
      // Signed-in users only from here; whether a path exists is revealed only after sign-in
      const user = sessionOf(req);
      if (!user) throw new HttpError(401, tr('Sign-in required', '로그인이 필요해요'));
      const matches = privateRoutes.map((r) => ({ r, m: r.pattern.exec(path) })).filter((x) => x.m);
      if (matches.length === 0) throw new HttpError(404, tr('No such path', '없는 경로'));
      const hit = matches.find((x) => x.r.method === method);
      if (!hit) throw new HttpError(405, tr('Method not allowed', '허용되지 않는 메서드'));
      if (!atLeast(user, hit.r.access)) throw new HttpError(403, tr('Permission denied', '권한이 없어요'));
      let params: string[];
      try {
        params = hit.m!.slice(1).map(decodeURIComponent);
      } catch {
        throw new HttpError(400, tr('Bad path encoding', '잘못된 경로 인코딩'));
      }
      for (const p of params) if (!ID_RE.test(p)) throw new HttpError(400, tr('Invalid ID', 'ID 형식 오류'));
      await hit.r.handler({ req, res, params, body, user, ip });
    } catch (e) {
      if (res.headersSent) return res.end();
      if (e instanceof HttpError) sendJson(res, e.status, { error: e.message });
      else if (e instanceof Conflict) sendJson(res, 409, { error: e.message });
      else if (e instanceof NotFound) sendJson(res, 404, { error: e.message });
      else {
        console.error(e);
        sendJson(res, 500, { error: tr('Server error', '서버 오류') });
      }
    }
  });

  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, '127.0.0.1', () => {
      panels.start();
      auth.start();
      const actual = (server.address() as { port: number }).port;
      if (actual !== port && !external) allowLoopback(actual);
      resolve({
        server,
        port: actual,
        panels,
        auth,
        close: async () => {
          auth.stop();
          await Promise.all([hub.stopAll(), panels.stop()]);
          server.closeAllConnections();
          await new Promise<void>((r) => server.close(() => r()));
        },
      });
    });
  });
}
