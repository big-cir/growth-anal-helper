// GA4 settings: reads and validates the connection (workspace.json ga4), the service account key and report definitions (ga4-reports.json).
import { closeSync, constants, fstatSync, openSync, readFileSync } from 'node:fs';
import { createHash, createPrivateKey, type KeyObject } from 'node:crypto';
import { join } from 'node:path';
import { CUSTOM_DIMENSIONS, customDef, METRICS, REPORT_KINDS, REPORT_DEFS_VERSION, tableColumns, USER_METRICS, type CustomReport, type ReportKind } from './reports.ts';

export class Ga4ConfigError extends Error {}

export type Ga4Connection = { propertyId: string; timeZone: string; keyFile: string };
export type Ga4Reports = { start: string; reports: ReportKind[]; events: Record<string, string>; minUsers: number; custom: CustomReport[] };
export type Ga4Credentials = { clientEmail: string; key: KeyObject };

/** Owned by me, no group/other permissions, regular file, size limit. Symlinks are not opened */
/** label: name of the file's role in errors (paths are never put in errors) */
export function readOwnerOnly(path: string, maxBytes: number, label: string): string {
  let fd: number;
  try {
    fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  } catch (e) {
    const code = (e as NodeJS.ErrnoException).code;
    throw new Ga4ConfigError(code === 'ENOENT' ? `${label} not found` : `cannot open ${label} (symlinks are not allowed)`);
  }
  try {
    const st = fstatSync(fd);
    const me = typeof process.getuid === 'function' ? process.getuid() : -1;
    if (!st.isFile()) throw new Ga4ConfigError(`${label} is not a regular file`);
    if (me >= 0 && st.uid !== me) throw new Ga4ConfigError(`${label} is not owned by the current user`);
    if ((st.mode & 0o777) !== 0o600) throw new Ga4ConfigError(`${label} permissions must be 0600`);
    if (st.size > maxBytes) throw new Ga4ConfigError(`${label} is too large (max ${maxBytes} bytes)`);
    return readFileSync(fd, 'utf8');
  } finally {
    closeSync(fd);
  }
}

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => !!v && typeof v === 'object' && !Array.isArray(v);
const DATE = /^\d{4}-\d{2}-\d{2}$/;

function validTimeZone(tz: string): boolean {
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: tz });
    return true;
  } catch {
    return false;
  }
}

/** The ga4 key of workspace.json. resolvePath: makes key_file absolute */
export function parseConnection(raw: unknown, resolvePath: (p: string) => string): Ga4Connection {
  if (!isObj(raw)) throw new Ga4ConfigError('must be an object');
  for (const k of Object.keys(raw)) if (!['property_id', 'time_zone', 'key_file'].includes(k)) throw new Ga4ConfigError(`unknown key ${k}`);
  if (typeof raw.property_id !== 'string' || !/^\d{1,20}$/.test(raw.property_id)) throw new Ga4ConfigError('property_id must be a numeric string');
  if (typeof raw.time_zone !== 'string' || !validTimeZone(raw.time_zone)) throw new Ga4ConfigError('time_zone must be an IANA time zone (e.g. America/Los_Angeles)');
  if (typeof raw.key_file !== 'string' || raw.key_file === '') throw new Ga4ConfigError('key_file must be the path of the service account key file');
  return { propertyId: raw.property_id, timeZone: raw.time_zone, keyFile: resolvePath(raw.key_file) };
}

/** Reads only the two values used for signing (token_uri and the rest are ignored) */
export function loadCredentials(keyFile: string): Ga4Credentials {
  let raw: unknown;
  try {
    raw = JSON.parse(readOwnerOnly(keyFile, 64 * 1024, 'service account key file'));
  } catch (e) {
    if (e instanceof Ga4ConfigError) throw e;
    throw new Ga4ConfigError('invalid service account key file');
  }
  if (!isObj(raw) || raw.type !== 'service_account' || typeof raw.client_email !== 'string' || !/^[^\s@]+@[^\s@]+$/.test(raw.client_email) || typeof raw.private_key !== 'string') {
    throw new Ga4ConfigError('service account key file is missing required values');
  }
  let key: KeyObject;
  try {
    key = createPrivateKey(raw.private_key);
  } catch {
    throw new Ga4ConfigError('cannot read the service account private key');
  }
  if (key.asymmetricKeyType !== 'rsa') throw new Ga4ConfigError('service account private key is not RSA');
  return { clientEmail: raw.client_email, key };
}

export function loadReports(wsDir: string): Ga4Reports {
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(join(wsDir, 'ga4-reports.json'), 'utf8'));
  } catch {
    throw new Ga4ConfigError('cannot read ga4-reports.json');
  }
  if (!isObj(raw)) throw new Ga4ConfigError('ga4-reports.json: must be an object');
  for (const k of Object.keys(raw)) if (!['start', 'reports', 'events', 'min_users', 'custom'].includes(k)) throw new Ga4ConfigError(`ga4-reports.json: unknown key ${k}`);
  if (typeof raw.start !== 'string' || !DATE.test(raw.start) || new Date(`${raw.start}T00:00:00Z`).toISOString().slice(0, 10) !== raw.start) throw new Ga4ConfigError('ga4-reports.json: start must be a real date YYYY-MM-DD');
  if (!Array.isArray(raw.reports)) throw new Ga4ConfigError('ga4-reports.json: reports must be an array');
  const reports = raw.reports.map((r) => {
    if (typeof r !== 'string' || !(r in REPORT_KINDS)) throw new Ga4ConfigError(`ga4-reports.json: unknown report ${String(r)} (${Object.keys(REPORT_KINDS).join(', ')})`);
    return r as ReportKind;
  });
  if (new Set(reports).size !== reports.length) throw new Ga4ConfigError('ga4-reports.json: duplicate reports');
  const events: Record<string, string> = {};
  if (raw.events !== undefined) {
    if (!isObj(raw.events)) throw new Ga4ConfigError('ga4-reports.json: events must be { "GA4 event name": "key" }');
    const seen = new Set<string>();
    for (const [name, key] of Object.entries(raw.events)) {
      if (!/^[A-Za-z][A-Za-z0-9_]{0,39}$/.test(name)) throw new Ga4ConfigError(`ga4-reports.json: invalid event name ${name}`);
      if (typeof key !== 'string' || !/^[a-z][a-z0-9_]{0,39}$/.test(key)) throw new Ga4ConfigError(`ga4-reports.json: event keys must be lowercase identifiers (${name})`);
      if (seen.has(key)) throw new Ga4ConfigError(`ga4-reports.json: event key ${key} is used twice (one-to-one only)`);
      seen.add(key);
      events[name] = key;
    }
  }
  if (reports.includes('daily_events') && Object.keys(events).length === 0) throw new Ga4ConfigError('ga4-reports.json: daily_events needs an events map');
  const minUsers = raw.min_users === undefined ? 10 : raw.min_users;
  if (typeof minUsers !== 'number' || !Number.isInteger(minUsers) || minUsers < 1 || minUsers > 1000) throw new Ga4ConfigError('ga4-reports.json: min_users must be an integer 1-1000');
  const custom = loadCustom(raw.custom, events);
  if (reports.length === 0 && custom.length === 0) throw new Ga4ConfigError('ga4-reports.json: reports or custom must have at least one report');
  return { start: raw.start, reports, events, minUsers, custom };
}

const CUSTOM_MAX = 10;
const RANGES = ['daily', 'weekly', 'monthly'];

/** Custom reports: allow-listed dimensions (0-2) and metrics (1-8) only; at most one fine dimension, which allows user-count metrics only */
function loadCustom(v: unknown, events: Record<string, string>): CustomReport[] {
  if (v === undefined) return [];
  if (!Array.isArray(v) || v.length > CUSTOM_MAX) throw new Ga4ConfigError(`ga4-reports.json: custom must be an array of at most ${CUSTOM_MAX}`);
  const ids = new Set<string>();
  return v.map((c, i) => {
    const at = `ga4-reports.json: custom[${i}]`;
    if (!isObj(c)) throw new Ga4ConfigError(`${at} must be an object`);
    for (const k of Object.keys(c)) if (!['id', 'range', 'dimensions', 'metrics'].includes(k)) throw new Ga4ConfigError(`${at}: unknown key ${k}`);
    if (typeof c.id !== 'string' || !/^[a-z][a-z0-9_]{0,30}$/.test(c.id)) throw new Ga4ConfigError(`${at}.id must be a lowercase identifier (31 characters or fewer)`);
    if (ids.has(c.id) || Object.hasOwn(REPORT_KINDS, c.id)) throw new Ga4ConfigError(`${at}.id ${c.id} clashes with another report`);
    ids.add(c.id);
    if (typeof c.range !== 'string' || !RANGES.includes(c.range)) throw new Ga4ConfigError(`${at}.range must be ${RANGES.join(' | ')}`);
    const list = (x: unknown, name: string, min: number, max: number, allowed: (n: string) => boolean): string[] => {
      if (!Array.isArray(x) || x.length < min || x.length > max || x.some((n) => typeof n !== 'string')) throw new Ga4ConfigError(`${at}.${name} must be an array of ${min}-${max} strings`);
      for (const n of x as string[]) if (!allowed(n)) throw new Ga4ConfigError(`${at}.${name}: not allowed: ${n}`);
      if (new Set(x).size !== x.length) throw new Ga4ConfigError(`${at}.${name}: duplicate`);
      return x as string[];
    };
    const dimensions = list(c.dimensions, 'dimensions', 0, 2, (n) => Object.hasOwn(CUSTOM_DIMENSIONS, n));
    const metrics = list(c.metrics, 'metrics', 1, 8, (n) => Object.hasOwn(METRICS, n) && METRICS[n].custom);
    const fine = dimensions.filter((d) => CUSTOM_DIMENSIONS[d].fine);
    if (fine.length > 1) throw new Ga4ConfigError(`${at}: only one fine dimension per report (${fine.join(', ')})`);
    if (dimensions.length > 0 && !metrics.some((m) => METRICS[m].group)) throw new Ga4ConfigError(`${at}: breakdown dimensions need activeUsers or totalUsers`);
    if (dimensions.includes('eventName')) {
      if (Object.keys(events).length === 0) throw new Ga4ConfigError(`${at}: eventName needs an events map`);
      if (!metrics.includes('totalUsers')) throw new Ga4ConfigError(`${at}: eventName needs totalUsers`);
      const bad = metrics.filter((m) => !USER_METRICS.includes(m) && m !== 'eventCount');
      if (bad.length) throw new Ga4ConfigError(`${at}: metrics not allowed with eventName: ${bad.join(', ')}`);
    } else if (fine.length) {
      const bad = metrics.filter((m) => !USER_METRICS.includes(m));
      if (bad.length) throw new Ga4ConfigError(`${at}: the fine dimension ${fine[0]} allows user-count metrics only (${bad.join(', ')})`);
    }
    const r: CustomReport = { id: c.id, range: c.range as CustomReport['range'], dimensions, metrics };
    const names = tableColumns(customDef(r)).map((x) => x.name);
    if (new Set(names).size !== names.length) throw new Ga4ConfigError(`${at}: duplicate column names`);
    return r;
  });
}

/** GA4 config hash used in the snapshot ID, reuse check and schema version */
export function ga4SpecHash(conn: Ga4Connection, reports: Ga4Reports): string {
  const events = Object.keys(reports.events).sort().map((k) => [k, reports.events[k]]);
  // Custom reports sorted by id (array order has no meaning); dimension and metric order is the column order, so kept
  const custom = [...reports.custom].sort((a, b) => (a.id < b.id ? -1 : 1)).map((c) => [c.id, c.range, c.dimensions, c.metrics]);
  const norm = { property_id: conn.propertyId, time_zone: conn.timeZone, start: reports.start, reports: [...reports.reports].sort(), custom, events, min_users: reports.minUsers, defs: REPORT_DEFS_VERSION, api: 'v1beta' };
  return createHash('sha256').update(JSON.stringify(norm)).digest('hex');
}
