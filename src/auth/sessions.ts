// Sign-in sessions (in memory), cookies, sign-in attempt limits.
import { createHash, randomBytes } from 'node:crypto';
import type { Role } from './accounts.ts';

export type User = { username: string; role: Role };
export type Session = User & { key: string; created: number; lastSeen: number };

export const SESSION_LIMITS = { idleMs: 8 * 3600_000, absoluteMs: 7 * 24 * 3600_000 };
export const COOKIE = 'gl_session';

const keyOf = (id: string) => createHash('sha256').update(id).digest('hex');

export class SessionStore {
  private readonly items = new Map<string, Session>();
  private readonly onRevoke: (key: string) => void;
  private readonly now: () => number;

  constructor(onRevoke: (key: string) => void, now: () => number = Date.now) {
    this.onRevoke = onRevoke;
    this.now = now;
  }

  /** Returns a new session ID (the cookie value) */
  create(user: User): string {
    const id = randomBytes(32).toString('base64url');
    const t = this.now();
    this.items.set(keyOf(id), { ...user, key: keyOf(id), created: t, lastSeen: t });
    return id;
  }

  /** Deletes and returns null when expired; otherwise updates the last-used time */
  get(id: string | null, touch = true): Session | null {
    if (!id) return null;
    const k = keyOf(id);
    const s = this.items.get(k);
    if (!s) return null;
    const t = this.now();
    if (t - s.lastSeen > SESSION_LIMITS.idleMs || t - s.created > SESSION_LIMITS.absoluteMs) {
      this.revokeKey(k);
      return null;
    }
    if (touch) s.lastSeen = t;
    return s;
  }

  validKey(key: string): Session | null {
    const s = this.items.get(key);
    if (!s) return null;
    const t = this.now();
    if (t - s.lastSeen > SESSION_LIMITS.idleMs || t - s.created > SESSION_LIMITS.absoluteMs) {
      this.revokeKey(key);
      return null;
    }
    return s;
  }

  destroy(id: string | null): boolean {
    if (!id) return false;
    return this.revokeKey(keyOf(id));
  }

  revokeUser(username: string): number {
    let n = 0;
    for (const [k, s] of this.items) if (s.username === username && this.revokeKey(k)) n++;
    return n;
  }

  private revokeKey(k: string): boolean {
    if (!this.items.delete(k)) return false;
    this.onRevoke(k);
    return true;
  }
}

export function parseCookies(header: string | undefined): Map<string, string> {
  const m = new Map<string, string>();
  for (const part of (header ?? '').split(';')) {
    const i = part.indexOf('=');
    if (i <= 0) continue;
    const k = part.slice(0, i).trim();
    if (!m.has(k)) m.set(k, part.slice(i + 1).trim());
  }
  return m;
}

export function sessionCookie(id: string, secure: boolean): string {
  return `${COOKIE}=${id}; HttpOnly; SameSite=Strict; Path=/${secure ? '; Secure' : ''}`;
}

export function clearCookie(secure: boolean): string {
  return `${COOKIE}=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0${secure ? '; Secure' : ''}`;
}

export const LOGIN_LIMITS = { maxDelayMs: 30_000, concurrent: 4, perMinute: 60, maxEntries: 10_000, entryTtlMs: 3600_000 };

type Entry = { fails: number; until: number; at: number };

/** No hard lockout; the wait grows with each consecutive failure (1, 2, 4 … 30 s) */
export class LoginLimiter {
  private readonly entries = new Map<string, Entry>();
  private recent: number[] = [];
  private active = 0;
  private readonly now: () => number;

  constructor(now: () => number = Date.now) {
    this.now = now;
  }

  /** null if an attempt is allowed, otherwise the wait in ms */
  check(keys: string[]): number | null {
    const t = this.now();
    this.recent = this.recent.filter((x) => t - x < 60_000);
    if (this.recent.length >= LOGIN_LIMITS.perMinute || this.active >= LOGIN_LIMITS.concurrent) return 1000;
    let wait = 0;
    for (const k of keys) {
      const e = this.entries.get(k);
      if (e && e.until > t) wait = Math.max(wait, e.until - t);
    }
    return wait > 0 ? wait : null;
  }

  begin(): void {
    this.active++;
    this.recent.push(this.now());
  }

  end(keys: string[], ok: boolean): void {
    this.active--;
    const t = this.now();
    for (const k of keys) {
      if (ok) {
        this.entries.delete(k);
        continue;
      }
      const e = this.entries.get(k) ?? { fails: 0, until: 0, at: t };
      e.fails++;
      e.at = t;
      e.until = t + Math.min(LOGIN_LIMITS.maxDelayMs, 1000 * 2 ** (e.fails - 1));
      this.entries.delete(k);
      this.entries.set(k, e);
    }
    this.prune(t);
  }

  private prune(t: number): void {
    for (const [k, e] of this.entries) if (t - e.at > LOGIN_LIMITS.entryTtlMs) this.entries.delete(k);
    while (this.entries.size > LOGIN_LIMITS.maxEntries) this.entries.delete(this.entries.keys().next().value!);
  }
}

/** IPv4-mapped IPv6 to IPv4 */
export function normalizeIp(ip: string | undefined): string {
  const v = (ip ?? '').trim().toLowerCase();
  const m = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/.exec(v);
  return m ? m[1] : v || 'unknown';
}

/** The hops-th address from the right of X-Forwarded-For */
export function clientIp(socketIp: string | undefined, xff: string | string[] | undefined, hops: number | null): string {
  if (hops === null || xff === undefined) return normalizeIp(socketIp);
  const list = (Array.isArray(xff) ? xff.join(',') : xff).split(',').map((x) => x.trim()).filter(Boolean);
  const v = list[list.length - hops];
  return normalizeIp(v ?? socketIp);
}
