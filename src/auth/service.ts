// Auth: account file watch, sign-in and sign-out, request user lookup, per-session SSE shutdown.
import { statSync } from 'node:fs';
import type { IncomingMessage } from 'node:http';
import { accountsFile, AccountError, hashPassword, readAccounts, verifyPassword, type Account, type Role } from './accounts.ts';
import { clientIp, COOKIE, LoginLimiter, parseCookies, SessionStore, type Session, type User } from './sessions.ts';
import type { AuditLog } from './audit.ts';

export const ROLE_RANK: Record<Role, number> = { viewer: 0, editor: 1, admin: 2 };
export const atLeast = (u: User, r: Role) => ROLE_RANK[u.role] >= ROLE_RANK[r];

export type LoginResult = { ok: true; sessionId: string; user: User } | { ok: false; status: 401 | 429; retryAfterMs?: number };

export class AuthService {
  readonly sessions: SessionStore;
  readonly limiter = new LoginLimiter();
  private readonly outDir: string;
  private readonly audit: AuditLog;
  private accounts = new Map<string, Account>();
  private stamp = '';
  private timer: NodeJS.Timeout | null = null;
  private readonly streams = new Map<string, Set<() => void>>();
  private dummyHash: Promise<string>;
  readonly proxyHops: number | null;

  /** requireAccounts: with auth on, the server needs at least one account to start */
  constructor(o: { outDir: string; audit: AuditLog; proxyHops: number | null; requireAccounts?: boolean }) {
    this.outDir = o.outDir;
    this.audit = o.audit;
    this.proxyHops = o.proxyHops;
    this.sessions = new SessionStore((key) => this.closeStreams(key));
    this.reload();
    if (o.requireAccounts !== false && this.accounts.size === 0) throw new AccountError('No accounts. Create one first with `node src/cli.ts account add <name> --role admin`');
    this.dummyHash = hashPassword(`dummy-${Math.random()}-password`);
  }

  start(pollMs = 2000): void {
    this.timer = setInterval(() => {
      try {
        this.reload();
      } catch (e) {
        console.error(`Could not reload the account file: ${(e as Error).message}`);
      }
    }, pollMs);
    this.timer.unref();
  }

  stop(): void {
    if (this.timer) clearInterval(this.timer);
  }

  /** Reload a changed account file and revoke sessions of changed users (removed, disabled, role or password changed) */
  reload(): void {
    let stamp = 'none';
    try {
      const st = statSync(accountsFile(this.outDir));
      stamp = `${st.ino}:${st.size}:${st.mtimeMs}:${st.ctimeMs}`;
    } catch {
      stamp = 'none';
    }
    if (stamp === this.stamp) return;
    const next = new Map(readAccounts(this.outDir).map((a) => [a.username, a]));
    for (const [name, old] of this.accounts) {
      const now = next.get(name);
      if (!now || now.disabled || now.role !== old.role || now.hash !== old.hash) {
        if (this.sessions.revokeUser(name) > 0) this.audit.tryWrite({ event: 'session_revoked', user: name });
      }
    }
    this.accounts = next;
    this.stamp = stamp;
  }

  ip(req: IncomingMessage): string {
    return clientIp(req.socket.remoteAddress, req.headers['x-forwarded-for'], this.proxyHops);
  }

  cookie(req: IncomingMessage): string | null {
    return parseCookies(req.headers.cookie).get(COOKIE) ?? null;
  }

  /** Check the session together with the current account state (enabled, role) */
  session(req: IncomingMessage): Session | null {
    const s = this.sessions.get(this.cookie(req));
    if (!s) return null;
    const a = this.accounts.get(s.username);
    if (!a || a.disabled || a.role !== s.role) {
      this.sessions.revokeUser(s.username);
      return null;
    }
    return s;
  }

  stillValid(key: string): Session | null {
    const s = this.sessions.validKey(key);
    if (!s) return null;
    const a = this.accounts.get(s.username);
    return a && !a.disabled && a.role === s.role ? s : null;
  }

  async login(req: IncomingMessage, username: string, password: string): Promise<LoginResult> {
    const ip = this.ip(req);
    const name = /^[a-z][a-z0-9_.-]{2,31}$/.test(username) ? username : '(invalid)';
    const keys = [`u:${name}`, `ip:${ip}`];
    const wait = this.limiter.check(keys);
    if (wait !== null) return { ok: false, status: 429, retryAfterMs: wait };
    this.limiter.begin();
    let ok = false;
    try {
      const a = this.accounts.get(username);
      const hash = a && !a.disabled ? a.hash : await this.dummyHash;
      ok = (await verifyPassword(password, hash)) && !!a && !a.disabled;
    } finally {
      this.limiter.end(keys, ok);
    }
    if (!ok) {
      this.audit.tryWrite({ event: 'login_fail', user: name, ip });
      return { ok: false, status: 401 };
    }
    const a = this.accounts.get(username)!;
    this.audit.write({ event: 'login_ok', user: a.username, ip });
    this.sessions.destroy(this.cookie(req));
    const user = { username: a.username, role: a.role };
    return { ok: true, sessionId: this.sessions.create(user), user };
  }

  /** Throws if the audit write fails (the session is kept) */
  logout(req: IncomingMessage): boolean {
    const s = this.sessions.get(this.cookie(req), false);
    if (s) this.audit.write({ event: 'logout', user: s.username, ip: this.ip(req) });
    return this.sessions.destroy(this.cookie(req));
  }

  /** Bind an SSE connection to the session; close is called when the session is revoked */
  registerStream(key: string, close: () => void): () => void {
    const set = this.streams.get(key) ?? new Set();
    set.add(close);
    this.streams.set(key, set);
    return () => {
      set.delete(close);
      if (set.size === 0) this.streams.delete(key);
    };
  }

  private closeStreams(key: string): void {
    const set = this.streams.get(key);
    if (!set) return;
    this.streams.delete(key);
    for (const close of set) close();
  }
}
