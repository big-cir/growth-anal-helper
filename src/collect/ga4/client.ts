// GA4 Data API calls: gets a token with a service account JWT, then calls runReport and checkCompatibility.
// Endpoints are fixed. Errors carry only the kind and status code (no URL, headers, body, key or token).
import { createSign, randomInt } from 'node:crypto';
import type { Ga4Credentials } from './config.ts';

export const TOKEN_URL = 'https://oauth2.googleapis.com/token';
export const API_BASE = 'https://analyticsdata.googleapis.com/v1beta';
export const SCOPE = 'https://www.googleapis.com/auth/analytics.readonly';

export const CLIENT_LIMITS = { timeoutMs: 60_000, bodyBytes: 50 * 1024 * 1024, retries: 3, maxCalls: 200, quotaMinShare: 0.1, maxRetryWaitMs: 60_000 };

export type Ga4ErrorKind = 'auth' | 'quota' | 'http_4xx' | 'http_5xx' | 'timeout' | 'invalid_response' | 'limit' | 'incompatible';

const MESSAGES: Record<Ga4ErrorKind, string> = {
  auth: 'GA4 authentication failed',
  quota: 'GA4 quota exhausted',
  http_4xx: 'GA4 request rejected',
  http_5xx: 'GA4 server error',
  timeout: 'GA4 response timed out',
  invalid_response: 'unexpected GA4 response format',
  limit: 'GA4 call limit reached',
  incompatible: 'incompatible GA4 dimensions and metrics',
};

export class Ga4Error extends Error {
  readonly kind: Ga4ErrorKind;
  readonly status: number | null;
  constructor(kind: Ga4ErrorKind, status: number | null = null, where = '') {
    super(`${MESSAGES[kind]}${status !== null ? ` (HTTP ${status})` : ''}${where ? ` — ${where}` : ''}`);
    this.kind = kind;
    this.status = status;
  }
}

export type HttpRequest = { url: string; method: 'POST'; headers: Record<string, string>; body: string };
export type HttpResponse = { status: number; headers: Record<string, string>; body: string };
export type Transport = (req: HttpRequest) => Promise<HttpResponse>;

/** Production transport: no redirects, timeout, body size limit. Exceptions keep only the kind */
export const httpsTransport: Transport = async (req) => {
  let res: Response;
  try {
    res = await fetch(req.url, { method: req.method, headers: req.headers, body: req.body, redirect: 'error', signal: AbortSignal.timeout(CLIENT_LIMITS.timeoutMs) });
  } catch (e) {
    throw new Ga4Error((e as Error).name === 'TimeoutError' ? 'timeout' : 'http_5xx');
  }
  const reader = res.body?.getReader();
  const chunks: Uint8Array[] = [];
  let n = 0;
  if (reader) {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      n += value.length;
      if (n > CLIENT_LIMITS.bodyBytes) {
        await reader.cancel();
        throw new Ga4Error('invalid_response', res.status, 'response too large');
      }
      chunks.push(value);
    }
  }
  const headers: Record<string, string> = {};
  res.headers.forEach((v, k) => { headers[k.toLowerCase()] = v; });
  return { status: res.status, headers, body: Buffer.concat(chunks).toString('utf8') };
};

const b64url = (s: string | Buffer) => Buffer.from(s).toString('base64url');

/** Service account JWT (RS256) with fixed claims */
export function signedAssertion(cred: Ga4Credentials, nowSec: number): string {
  const header = b64url(JSON.stringify({ alg: 'RS256', typ: 'JWT' }));
  const claims = b64url(JSON.stringify({ iss: cred.clientEmail, scope: SCOPE, aud: TOKEN_URL, iat: nowSec, exp: nowSec + 3600 }));
  const sign = createSign('RSA-SHA256');
  sign.update(`${header}.${claims}`);
  return `${header}.${claims}.${b64url(sign.sign(cred.key))}`;
}

export type ClientOptions = { transport?: Transport; sleep?: (ms: number) => Promise<void>; now?: () => number };

export class Ga4Client {
  private readonly cred: Ga4Credentials;
  private readonly propertyId: string;
  private readonly transport: Transport;
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly now: () => number;
  private token: { value: string; until: number } | null = null;
  calls = 0;

  constructor(cred: Ga4Credentials, propertyId: string, o: ClientOptions = {}) {
    if (!/^\d{1,20}$/.test(propertyId)) throw new Ga4Error('invalid_response', null, 'property_id format');
    this.cred = cred;
    this.propertyId = propertyId;
    this.transport = o.transport ?? httpsTransport;
    this.sleep = o.sleep ?? ((ms) => new Promise((r) => setTimeout(r, ms)));
    this.now = o.now ?? Date.now;
  }

  /** Call with limits and retries (429 and 5xx, exponential backoff with jitter, Retry-After) */
  private async send(req: HttpRequest, where: string): Promise<HttpResponse> {
    for (let attempt = 0; ; attempt++) {
      if (this.calls >= CLIENT_LIMITS.maxCalls) throw new Ga4Error('limit', null, where);
      this.calls++;
      let res: HttpResponse;
      try {
        res = await this.transport(req);
      } catch (e) {
        const err = e instanceof Ga4Error ? e : new Ga4Error('http_5xx', null, where);
        if (err.kind !== 'timeout' && err.kind !== 'http_5xx') throw new Ga4Error(err.kind, err.status, where);
        if (attempt >= CLIENT_LIMITS.retries) throw new Ga4Error(err.kind, err.status, where);
        await this.sleep(1000 * 2 ** attempt + randomInt(250));
        continue;
      }
      if (res.status === 429 || res.status >= 500) {
        if (attempt >= CLIENT_LIMITS.retries) throw new Ga4Error(res.status === 429 ? 'quota' : 'http_5xx', res.status, where);
        const ra = Number(res.headers['retry-after']);
        const wait = Number.isFinite(ra) && ra > 0 ? Math.min(ra * 1000, CLIENT_LIMITS.maxRetryWaitMs) : 1000 * 2 ** attempt + randomInt(250);
        await this.sleep(wait);
        continue;
      }
      if (res.status === 401 || res.status === 403) throw new Ga4Error('auth', res.status, where);
      if (res.status >= 400) throw new Ga4Error('http_4xx', res.status, where);
      return res;
    }
  }

  private async accessToken(): Promise<string> {
    if (this.token && this.token.until > this.now() + 60_000) return this.token.value;
    const assertion = signedAssertion(this.cred, Math.floor(this.now() / 1000));
    const body = new URLSearchParams({ grant_type: 'urn:ietf:params:oauth:grant-type:jwt-bearer', assertion }).toString();
    const res = await this.send({ url: TOKEN_URL, method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body }, 'token');
    let j: Record<string, unknown>;
    try {
      j = JSON.parse(res.body);
    } catch {
      throw new Ga4Error('auth', res.status, 'token response');
    }
    if (j.token_type !== 'Bearer' || typeof j.access_token !== 'string' || j.access_token === '' || typeof j.expires_in !== 'number' || !Number.isSafeInteger(j.expires_in) || j.expires_in <= 0) throw new Ga4Error('auth', res.status, 'token response');
    this.token = { value: j.access_token, until: this.now() + j.expires_in * 1000 };
    return this.token.value;
  }

  private async post(method: 'runReport' | 'checkCompatibility', payload: unknown, where: string): Promise<Record<string, unknown>> {
    const token = await this.accessToken();
    const res = await this.send({
      url: `${API_BASE}/properties/${this.propertyId}:${method}`,
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }, where);
    try {
      const j = JSON.parse(res.body);
      if (!j || typeof j !== 'object' || Array.isArray(j)) throw new Error();
      return j as Record<string, unknown>;
    } catch {
      throw new Ga4Error('invalid_response', res.status, where);
    }
  }

  runReport(payload: unknown, where: string) {
    return this.post('runReport', payload, where);
  }

  checkCompatibility(payload: unknown, where: string) {
    return this.post('checkCompatibility', payload, where);
  }
}
