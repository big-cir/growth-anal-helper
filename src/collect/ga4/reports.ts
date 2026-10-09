// GA4 report kinds known to the engine: building requests and turning responses into table rows.
// No free-text dimensions. Category values come from known lists; events are stored only as mapped keys.

/** Bump when report definitions, allow-lists, column names or small-value rules change (part of the snapshot ID) */
export const REPORT_DEFS_VERSION = 2;

type DimKind = 'date' | 'category' | 'pattern' | 'event' | 'week' | 'month' | 'cohort' | 'nth';
/** fine: fine-grained dimension (one per report, not with activity metrics) */
type Dim = { api: string; col: string; kind: DimKind; values?: readonly string[]; pattern?: RegExp; fine?: boolean };
/** user: user count, group: user count that sets group size, max: upper bound */
type Metric = { api: string; col: string; int: boolean; user: boolean; group: boolean; max?: number };
/** key: report key in the collect log, breakdown: table split by category or event */
export type ReportDef = { key: string; table: string; range: 'daily' | 'weekly' | 'monthly' | 'cohort'; dims: Dim[]; metrics: Metric[]; custom: boolean };

const CHANNELS = ['Direct', 'Organic Search', 'Paid Search', 'Organic Social', 'Paid Social', 'Email', 'Affiliates', 'Referral', 'Paid Shopping', 'Organic Shopping', 'Display', 'Paid Video', 'Organic Video', 'Audio', 'SMS', 'Mobile Push Notifications', 'Cross-network', 'Paid Other', 'Unassigned'];

/** Category values defined by Google */
export const CATEGORY_VALUES: Record<string, readonly string[]> = {
  firstUserDefaultChannelGroup: CHANNELS,
  sessionDefaultChannelGroup: CHANNELS,
  platform: ['Android', 'iOS', 'web'],
  deviceCategory: ['desktop', 'mobile', 'tablet', 'smart tv'],
  newVsReturning: ['new', 'returning'],
  operatingSystem: ['Android', 'iOS', 'Windows', 'Macintosh', 'Linux', 'Chrome OS'],
};

const cat = (api: string, col: string): Dim => ({ api, col, kind: 'category', values: CATEGORY_VALUES[api] });
/** Dimensions available to custom reports */
export const CUSTOM_DIMENSIONS: Record<string, Dim> = {
  firstUserDefaultChannelGroup: cat('firstUserDefaultChannelGroup', 'first_user_channel_group'),
  sessionDefaultChannelGroup: cat('sessionDefaultChannelGroup', 'session_channel_group'),
  platform: cat('platform', 'platform'),
  deviceCategory: cat('deviceCategory', 'device_category'),
  newVsReturning: cat('newVsReturning', 'new_vs_returning'),
  operatingSystem: cat('operatingSystem', 'operating_system'),
  dayOfWeek: { api: 'dayOfWeek', col: 'day_of_week', kind: 'pattern', pattern: /^[0-6]$/ },
  countryId: { api: 'countryId', col: 'country_id', kind: 'pattern', pattern: /^[A-Z]{2}$/, fine: true },
  languageCode: { api: 'languageCode', col: 'language_code', kind: 'pattern', pattern: /^[a-z]{2,3}$/, fine: true },
  hour: { api: 'hour', col: 'hour', kind: 'pattern', pattern: /^([01]\d|2[0-3])$/, fine: true },
  eventName: { api: 'eventName', col: 'event_key', kind: 'event', fine: true },
};

const mk = (api: string, col: string, o: { int?: boolean; user?: boolean; group?: boolean; max?: number } = {}): Metric =>
  ({ api, col, int: o.int ?? true, user: o.user ?? false, group: o.group ?? false, ...(o.max !== undefined ? { max: o.max } : {}) });
/** Metrics known to the engine. custom: available to custom reports */
export const METRICS: Record<string, Metric & { custom: boolean }> = {
  activeUsers: { ...mk('activeUsers', 'active_users', { user: true, group: true }), custom: true },
  totalUsers: { ...mk('totalUsers', 'total_users', { user: true, group: true }), custom: true },
  newUsers: { ...mk('newUsers', 'new_users', { user: true }), custom: true },
  sessions: { ...mk('sessions', 'sessions'), custom: true },
  engagedSessions: { ...mk('engagedSessions', 'engaged_sessions'), custom: true },
  eventCount: { ...mk('eventCount', 'event_count'), custom: true },
  screenPageViews: { ...mk('screenPageViews', 'screen_page_views'), custom: true },
  userEngagementDuration: { ...mk('userEngagementDuration', 'engagement_seconds', { int: false }), custom: true },
  averageSessionDuration: { ...mk('averageSessionDuration', 'average_session_seconds', { int: false }), custom: true },
  engagementRate: { ...mk('engagementRate', 'engagement_rate', { int: false, max: 1 }), custom: true },
  cohortActiveUsers: { ...mk('cohortActiveUsers', 'cohort_active_users', { user: true }), custom: false },
  cohortTotalUsers: { ...mk('cohortTotalUsers', 'cohort_total_users', { user: true, group: true }), custom: false },
};
const M = (api: string): Metric => {
  const { custom: _, ...x } = METRICS[api];
  return x;
};
export const USER_METRICS = Object.keys(METRICS).filter((k) => METRICS[k].user && METRICS[k].custom);

const DATE: Dim = { api: 'date', col: 'date', kind: 'date' };
const WEEK: Dim = { api: 'isoYearIsoWeek', col: 'iso_year_week', kind: 'week' };
const MONTH: Dim = { api: 'yearMonth', col: 'year_month', kind: 'month' };
const TIME = { daily: DATE, weekly: WEEK, monthly: MONTH } as const;

const builtin = (key: string, range: ReportDef['range'], dims: Dim[], metrics: string[]): ReportDef =>
  ({ key, table: `r_ga4_${key}`, range, dims, metrics: metrics.map(M), custom: false });

export const REPORT_KINDS = {
  daily_overview: builtin('daily_overview', 'daily', [DATE], ['activeUsers', 'newUsers', 'totalUsers', 'sessions', 'engagedSessions', 'userEngagementDuration']),
  weekly_users: builtin('weekly_users', 'weekly', [WEEK], ['activeUsers', 'newUsers']),
  monthly_users: builtin('monthly_users', 'monthly', [MONTH], ['activeUsers', 'newUsers']),
  daily_events: builtin('daily_events', 'daily', [DATE, CUSTOM_DIMENSIONS.eventName], ['eventCount', 'totalUsers']),
  daily_channel: builtin('daily_channel', 'daily', [DATE, { ...CUSTOM_DIMENSIONS.firstUserDefaultChannelGroup, col: 'channel_group' }], ['newUsers', 'activeUsers', 'engagedSessions']),
  daily_platform: builtin('daily_platform', 'daily', [DATE, CUSTOM_DIMENSIONS.platform, CUSTOM_DIMENSIONS.deviceCategory], ['activeUsers', 'newUsers']),
  daily_new_returning: builtin('daily_new_returning', 'daily', [DATE, CUSTOM_DIMENSIONS.newVsReturning], ['activeUsers', 'engagedSessions']),
  weekly_cohort: builtin('weekly_cohort', 'cohort', [{ api: 'cohort', col: 'cohort', kind: 'cohort' }, { api: 'cohortNthWeek', col: 'cohort_nth_week', kind: 'nth' }], ['cohortActiveUsers', 'cohortTotalUsers']),
} satisfies Record<string, ReportDef>;

export type ReportKind = keyof typeof REPORT_KINDS;

export type CustomReport = { id: string; range: 'daily' | 'weekly' | 'monthly'; dimensions: string[]; metrics: string[] };

/** Custom report definition. Called after config.ts has validated it */
export function customDef(c: CustomReport): ReportDef {
  return { key: `custom:${c.id}`, table: `r_ga4_x_${c.id}`, range: c.range, dims: [TIME[c.range], ...c.dimensions.map((d) => CUSTOM_DIMENSIONS[d])], metrics: c.metrics.map(M), custom: true };
}

/** Whether the table is split by category or event (time dimensions alone do not count) */
export const isBreakdown = (def: ReportDef) => def.range !== 'cohort' && def.dims.some((d) => d.kind === 'category' || d.kind === 'pattern' || d.kind === 'event');

/** Internal codes that never clash with real values */
export const CODES = { unknown: '_unknown', ga4Other: '_ga4_other' } as const;
/** Metric types read as real numbers */
const REAL_TYPES = new Set(['TYPE_FLOAT', 'TYPE_SECONDS', 'TYPE_MILLISECONDS', 'TYPE_MINUTES', 'TYPE_HOURS', 'TYPE_STANDARD', 'TYPE_CURRENCY', 'TYPE_FEET', 'TYPE_MILES', 'TYPE_METERS', 'TYPE_KILOMETERS']);
const realDate = (d: string) => /^\d{4}-\d{2}-\d{2}$/.test(d) && new Date(`${d}T00:00:00Z`).toISOString().slice(0, 10) === d;

// ── Dates ───────────────────────────────────────────────

const ymd = (d: Date) => d.toISOString().slice(0, 10);
export const addDays = (date: string, n: number) => ymd(new Date(Date.parse(`${date}T00:00:00Z`) + n * 86_400_000));
const dow = (date: string) => new Date(`${date}T00:00:00Z`).getUTCDay();

/** "Today minus 2 days" in the property time zone */
export function dataThrough(nowMs: number, timeZone: string): string {
  const today = new Intl.DateTimeFormat('en-CA', { timeZone, year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date(nowMs));
  return addDays(today, -2);
}

/** Range trimmed to complete ISO weeks (Mon-Sun) or months. Null if none */
export function completeRange(range: 'weekly' | 'monthly', start: string, through: string): { start: string; end: string } | null {
  let s: string;
  let e: string;
  if (range === 'weekly') {
    s = addDays(start, (8 - dow(start)) % 7);
    e = addDays(through, -dow(through));
  } else {
    s = start.endsWith('-01') ? start : addDays(`${start.slice(0, 7)}-01`, 32).slice(0, 7) + '-01';
    const nextDay = addDays(through, 1);
    e = nextDay.endsWith('-01') ? through : addDays(`${through.slice(0, 7)}-01`, -1);
  }
  return s <= e ? { start: s, end: e } : null;
}

/** Last 12 complete acquisition weeks (Sun-Sat) */
export function cohortWeeks(through: string, n = 12): string[] {
  const lastSat = addDays(through, -((dow(through) + 1) % 7));
  return Array.from({ length: n }, (_, i) => addDays(lastSat, -6 - 7 * (n - 1 - i)));
}

/** Monday of an ISO week (YYYYWW) */
function isoWeekMonday(year: number, week: number): string {
  const jan4 = Date.UTC(year, 0, 4);
  const mondayW1 = jan4 - ((new Date(jan4).getUTCDay() + 6) % 7) * 86_400_000;
  return ymd(new Date(mondayW1 + (week - 1) * 7 * 86_400_000));
}

// ── Requests ────────────────────────────────────────────

export function buildRequest(def: ReportDef, o: { start: string; through: string; events: Record<string, string> }, offset: number): Record<string, unknown> | null {
  const base = {
    dimensions: def.dims.map((d) => ({ name: d.api })),
    metrics: def.metrics.map((x) => ({ name: x.api })),
    orderBys: def.dims.map((d) => ({ dimension: { dimensionName: d.api } })),
    limit: 250_000,
    offset,
    returnPropertyQuota: true,
  };
  if (def.range === 'cohort') {
    const weeks = cohortWeeks(o.through).filter((w) => w >= o.start);
    if (!weeks.length) return null;
    // Cohort requests have no top-level dateRanges
    return {
      ...base,
      cohortSpec: {
        cohorts: weeks.map((w) => ({ name: w, dimension: 'firstSessionDate', dateRange: { startDate: w, endDate: addDays(w, 6) } })),
        cohortsRange: { granularity: 'WEEKLY', startOffset: 0, endOffset: 11 },
      },
    };
  }
  const r = def.range === 'daily' ? (o.start <= o.through ? { start: o.start, end: o.through } : null) : completeRange(def.range, o.start, o.through);
  if (!r) return null;
  const req: Record<string, unknown> = { ...base, dateRanges: [{ startDate: r.start, endDate: r.end }] };
  if (def.dims.some((d) => d.kind === 'event')) {
    req.dimensionFilter = { filter: { fieldName: 'eventName', inListFilter: { values: Object.keys(o.events), caseSensitive: true } } };
  }
  return req;
}

// ── Response → rows ─────────────────────────────────────

export class ReportShapeError extends Error {}

export type Ga4Row = (string | number | null)[];
export type TransformStats = { droppedSmall: number; droppedUnobserved: number; droppedUnmapped: number; droppedCollision: number; suppressedCells: number };

/** Null for events not in the map (the row is not stored) */
function dimValue(d: Dim, v: string, events: Record<string, string>): (string | number)[] | null {
  switch (d.kind) {
    case 'date': {
      const date = `${v.slice(0, 4)}-${v.slice(4, 6)}-${v.slice(6, 8)}`;
      if (!/^\d{8}$/.test(v) || !realDate(date)) throw new ReportShapeError(`${d.api} value format`);
      return [date];
    }
    case 'week': {
      const week = Number(v.slice(4));
      if (!/^\d{6}$/.test(v) || week < 1 || week > 53) throw new ReportShapeError(`${d.api} value format`);
      const mon = isoWeekMonday(Number(v.slice(0, 4)), week);
      return [v, mon, addDays(mon, 6)];
    }
    case 'month': {
      const first = `${v.slice(0, 4)}-${v.slice(4)}-01`;
      if (!/^\d{6}$/.test(v) || !realDate(first)) throw new ReportShapeError(`${d.api} value format`);
      return [v, first, addDays(addDays(first, 32).slice(0, 7) + '-01', -1)];
    }
    case 'nth': {
      if (!/^\d{1,4}$/.test(v)) throw new ReportShapeError(`${d.api} value format`);
      return [Number(v)];
    }
    case 'cohort': {
      if (!realDate(v)) throw new ReportShapeError(`${d.api} value format`);
      return [v];
    }
    case 'event':
      return Object.hasOwn(events, v) ? [events[v]] : null;
    case 'category':
      if (v === '(other)') return [CODES.ga4Other];
      return [d.values?.includes(v) ? v : CODES.unknown];
    case 'pattern':
      if (v === '(other)') return [CODES.ga4Other];
      return [d.pattern?.test(v) ? v : CODES.unknown];
  }
}

export type Column = { name: string; type: 'TEXT' | 'INTEGER' | 'REAL'; key: boolean; nullable: boolean };

/** Table columns (weeks and months add period start and end). Integer metrics in breakdown tables and cohort active users allow NULL, since small values are blanked */
export function tableColumns(def: ReportDef): Column[] {
  const cols: Column[] = [];
  for (const d of def.dims) {
    cols.push({ name: d.col, type: d.kind === 'nth' ? 'INTEGER' : 'TEXT', key: true, nullable: false });
    if (d.kind === 'week' || d.kind === 'month') cols.push({ name: 'period_start', type: 'TEXT', key: false, nullable: false }, { name: 'period_end', type: 'TEXT', key: false, nullable: false });
  }
  const breakdown = isBreakdown(def);
  for (const x of def.metrics) {
    const nullable = (breakdown && x.int) || (def.range === 'cohort' && x.api === 'cohortActiveUsers');
    cols.push({ name: x.col, type: x.int ? 'INTEGER' : 'REAL', key: false, nullable });
  }
  return cols;
}

export function transformRows(def: ReportDef, raw: { dims: string[]; metrics: string[]; types: string[] }[], o: { events: Record<string, string>; minUsers: number; through: string }): { rows: Ga4Row[]; stats: TransformStats } {
  const stats: TransformStats = { droppedSmall: 0, droppedUnobserved: 0, droppedUnmapped: 0, droppedCollision: 0, suppressedCells: 0 };
  const cols = tableColumns(def);
  const keyCols = cols.map((c, i) => (c.key ? i : -1)).filter((i) => i >= 0);
  const mOff = cols.length - def.metrics.length;
  // 1) Raw duplicates are a response error. Rows that collide after normalization (_unknown, _ga4_other) fail built-in reports; custom reports drop all rows with that key
  const rawSeen = new Set<string>();
  const groups = new Map<string, Ga4Row[]>();
  for (const r of raw) {
    const rawKey = JSON.stringify(r.dims);
    if (rawSeen.has(rawKey)) throw new ReportShapeError(`more than one row with the same raw key (${def.table})`);
    rawSeen.add(rawKey);
    const parts = def.dims.map((d, i) => dimValue(d, r.dims[i], o.events));
    if (parts.some((p) => p === null)) {
      stats.droppedUnmapped++;
      continue;
    }
    const dims = (parts as (string | number)[][]).flat();
    const metrics = def.metrics.map((x, i) => {
      const type = r.types[i];
      const n = Number(r.metrics[i]);
      if (r.metrics[i] === '' || !Number.isFinite(n)) throw new ReportShapeError(`${x.api}: not a number`);
      if (type === 'TYPE_INTEGER') {
        if (!Number.isSafeInteger(n)) throw new ReportShapeError(`${x.api}: not a safe integer`);
      } else if (!REAL_TYPES.has(type) || x.int) throw new ReportShapeError(`${x.api}: metric type ${type}`);
      if (n < 0 || (x.max !== undefined && n > x.max)) throw new ReportShapeError(`${x.api}: value out of range`);
      return n;
    });
    const row = [...dims, ...metrics];
    const key = JSON.stringify(keyCols.map((i) => row[i]));
    const g = groups.get(key);
    if (g) {
      if (!def.custom) throw new ReportShapeError(`more than one row with the same key (${def.table})`);
      g.push(row);
    } else groups.set(key, [row]);
  }
  const all: Ga4Row[] = [];
  for (const g of groups.values()) {
    if (g.length > 1) stats.droppedCollision += g.length;
    else all.push(g[0]);
  }
  const at = (api: string) => mOff + def.metrics.findIndex((x) => x.api === api);
  // 2) Cohorts: drop cells still being observed and small cohorts; blank small active counts
  if (def.range === 'cohort') {
    const totalIdx = at('cohortTotalUsers');
    const activeIdx = at('cohortActiveUsers');
    const small = new Set(all.filter((r) => (r[totalIdx] as number) < o.minUsers).map((r) => r[0] as string));
    const rows = all.filter((r) => {
      if (addDays(r[0] as string, 7 * ((r[1] as number) + 1) - 1) > o.through) {
        stats.droppedUnobserved++;
        return false;
      }
      if (small.has(r[0] as string)) {
        stats.droppedSmall++;
        return false;
      }
      return true;
    }).map((r) => {
      if ((r[activeIdx] as number) < o.minUsers) {
        stats.suppressedCells++;
        return r.map((v, i) => (i === activeIdx ? null : v));
      }
      return r;
    });
    return { rows, stats };
  }
  // 3) Breakdown tables: drop rows whose group (max of active and total users) is small; blank small integer values in the rest
  if (!isBreakdown(def)) return { rows: all, stats };
  const groupIdx = def.metrics.map((x, i) => (x.group ? mOff + i : -1)).filter((i) => i >= 0);
  const intIdx = def.metrics.map((x, i) => (x.int ? mOff + i : -1)).filter((i) => i >= 0);
  const rows: Ga4Row[] = [];
  for (const r of all) {
    if (Math.max(...groupIdx.map((i) => r[i] as number)) < o.minUsers) {
      stats.droppedSmall++;
      continue;
    }
    rows.push(r.map((v, i) => {
      if (intIdx.includes(i) && (v as number) < o.minUsers) {
        stats.suppressedCells++;
        return null;
      }
      return v;
    }));
  }
  return { rows, stats };
}
