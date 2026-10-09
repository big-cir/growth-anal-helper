// GA4 import: fetches the configured reports into r_ga4_* tables and r_ga4_collect_log of the temporary snapshot.
import type { DatabaseSync } from 'node:sqlite';
import { secretShape } from '../../agent/sensitive.ts';
import type { Role } from '../spec.ts';
import { Ga4Client, Ga4Error, CLIENT_LIMITS, type Transport } from './client.ts';
import { ga4SpecHash, loadCredentials, loadReports, type Ga4Connection, type Ga4Reports } from './config.ts';
import { buildRequest, customDef, dataThrough, REPORT_KINDS, ReportShapeError, tableColumns, transformRows, type ReportDef } from './reports.ts';

export type Ga4Plan = { conn: Ga4Connection; reports: Ga4Reports; specHash: string };

export function loadGa4Plan(wsDir: string, conn: Ga4Connection): Ga4Plan {
  const reports = loadReports(wsDir);
  return { conn, reports, specHash: ga4SpecHash(conn, reports) };
}

/** Reports in this plan (built-in + custom) */
export function planDefs(plan: Ga4Plan): ReportDef[] {
  return [...plan.reports.reports.map((k) => REPORT_KINDS[k] as ReportDef), ...plan.reports.custom.map(customDef)];
}

/** GA4 table names in this plan (sorted) */
export function ga4Tables(plan: Ga4Plan | null): string[] {
  return plan ? planDefs(plan).map((d) => d.table).sort() : [];
}

/** Column roles of GA4 tables (all ordinary: no user IDs or free text) */
export function ga4Roles(plan: Ga4Plan | null): Map<string, Role> {
  const m = new Map<string, Role>();
  if (!plan) return m;
  for (const def of planDefs(plan)) for (const c of tableColumns(def)) m.set(`${def.table}.${c.name}`, 'ordinary');
  return m;
}

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => !!v && typeof v === 'object' && !Array.isArray(v);
const names = (v: unknown) => (Array.isArray(v) ? v.map((x) => (isObj(x) ? x.name : undefined)) : []);

function checkQuota(j: Obj, where: string): { day: number; hour: number } {
  if (!isObj(j.propertyQuota)) throw new Ga4Error('invalid_response', null, `${where} missing quota info`);
  const q = j.propertyQuota;
  const share = (k: string) => {
    const x = q[k];
    const ok = (v: unknown) => typeof v === 'number' && Number.isFinite(v) && v >= 0;
    if (!isObj(x) || !ok(x.remaining) || !ok(x.consumed)) throw new Ga4Error('invalid_response', null, `${where} quota info format`);
    const remaining = x.remaining as number;
    const total = remaining + (x.consumed as number);
    if (total > 0 && remaining / total < CLIENT_LIMITS.quotaMinShare) throw new Ga4Error('quota', null, where);
    return remaining;
  };
  return { day: share('tokensPerDay'), hour: share('tokensPerHour') };
}

export type Ga4ImportOptions = { transport?: Transport; sleep?: (ms: number) => Promise<void>; now?: () => number; log?: (m: string) => void };

/** Builds the GA4 tables in the temporary snapshot DB. Throws on failure (the caller discards the temporary file) */
export async function importGa4(db: DatabaseSync, plan: Ga4Plan, o: Ga4ImportOptions = {}): Promise<void> {
  const now = o.now ?? Date.now;
  const client = new Ga4Client(loadCredentials(plan.conn.keyFile), plan.conn.propertyId, { transport: o.transport, sleep: o.sleep, now });
  const through = dataThrough(now(), plan.conn.timeZone);
  const insLog = db.prepare(`INSERT INTO r_ga4_collect_log (report, table_name, rows, row_count, dropped_small, dropped_unobserved, dropped_unmapped, dropped_collision, suppressed_cells, subject_to_thresholding, data_loss_from_other_row, sampled, sampling_summary, truncated, schema_restricted, empty_reason, time_zone, data_through, calls, quota_day_remaining, quota_hour_remaining, collected_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`);

  for (const def of planDefs(plan)) {
    const kind = def.key;
    const cols = tableColumns(def);
    db.exec(`CREATE TABLE ${def.table} (${cols.map((c) => `${c.name} ${c.type}${c.nullable ? '' : ' NOT NULL'}`).join(', ')}, PRIMARY KEY (${cols.filter((c) => c.key).map((c) => c.name).join(', ')}))`);
    const callsBefore = client.calls;
    const opt = { start: plan.reports.start, through, events: plan.reports.events };
    const first = buildRequest(def, opt, 0);
    if (!first) {
      insLog.run(kind, def.table, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, '', '', 0, 'no_complete_period', plan.conn.timeZone, through, 0, null, null, new Date(now()).toISOString());
      continue;
    }
    o.log?.(`GA4 ${kind}: fetching`);
    // Compatibility: every requested dimension and metric must be COMPATIBLE.
    // Cohort dimensions (cohort, cohortNthWeek) are not covered by checkCompatibility, so the response headers are checked instead
    if (def.range !== 'cohort') {
      const c = await client.checkCompatibility({ dimensions: first.dimensions, metrics: first.metrics, ...(first.dimensionFilter ? { dimensionFilter: first.dimensionFilter } : {}) }, kind);
      // Each requested name appears exactly once and is COMPATIBLE (the response also lists other items)
      const allCompatible = (list: unknown, meta: string, want: string[]) => {
        if (!Array.isArray(list)) return false;
        return want.every((n) => {
          const hits = list.filter((x) => isObj(x) && isObj(x[meta]) && (x[meta] as Obj).apiName === n);
          return hits.length === 1 && (hits[0] as Obj).compatibility === 'COMPATIBLE';
        });
      };
      const ok = allCompatible(c.dimensionCompatibilities, 'dimensionMetadata', def.dims.map((d) => d.api)) && allCompatible(c.metricCompatibilities, 'metricMetadata', def.metrics.map((x) => x.api));
      if (!ok) throw new Ga4Error('incompatible', null, kind);
    }

    const raw: { dims: string[]; metrics: string[]; types: string[] }[] = [];
    let offset = 0;
    let rowCount = -1;
    const pages = new Set<string>();
    let meta: Obj = {};
    let quota = { day: 0, hour: 0 };
    for (;;) {
      const req = buildRequest(def, opt, offset)!;
      const j = await client.runReport(req, kind);
      quota = checkQuota(j, kind);
      const dimNames = names(j.dimensionHeaders);
      const metricHeaders = Array.isArray(j.metricHeaders) ? j.metricHeaders : [];
      if (JSON.stringify(dimNames) !== JSON.stringify(def.dims.map((d) => d.api)) || JSON.stringify(names(metricHeaders)) !== JSON.stringify(def.metrics.map((x) => x.api))) throw new Ga4Error('invalid_response', null, `${kind} headers`);
      meta = isObj(j.metadata) ? j.metadata : {};
      if (meta.timeZone !== plan.conn.timeZone) throw new Ga4Error('invalid_response', null, `${kind} time zone differs from the config`);
      // rowCount is fixed by the first page and must match on every page.
      // GA4 omits rowCount for empty results, so a missing value is read as 0 only when there are no rows
      const hasRows = Array.isArray(j.rows) && j.rows.length > 0;
      if (j.rowCount === undefined && hasRows) throw new Ga4Error('invalid_response', null, `${kind} rowCount`);
      const rc = j.rowCount === undefined ? 0 : j.rowCount;
      if (typeof rc !== 'number' || !Number.isSafeInteger(rc) || rc < 0) throw new Ga4Error('invalid_response', null, `${kind} rowCount`);
      if (rowCount === -1) rowCount = rc;
      else if (rc !== rowCount) throw new Ga4Error('invalid_response', null, `${kind} rowCount differs between pages`);
      const rows = Array.isArray(j.rows) ? j.rows : [];
      // Fail if any earlier page comes back again
      const fp = JSON.stringify(rows);
      if (offset > 0 && (!rows.length || pages.has(fp))) throw new Ga4Error('invalid_response', null, `${kind} paging`);
      pages.add(fp);
      const types = metricHeaders.map((h) => (isObj(h) ? String(h.type) : ''));
      const strs = (v: unknown, n: number) => Array.isArray(v) && v.length === n && v.every((x) => isObj(x) && typeof x.value === 'string');
      for (const r of rows) {
        if (!isObj(r) || !strs(r.dimensionValues, def.dims.length) || !strs(r.metricValues, def.metrics.length)) throw new Ga4Error('invalid_response', null, `${kind} row format`);
        raw.push({ dims: (r.dimensionValues as Obj[]).map((v) => v.value as string), metrics: (r.metricValues as Obj[]).map((v) => v.value as string), types });
      }
      if (raw.length > rowCount) throw new Ga4Error('invalid_response', null, `${kind} more rows than rowCount`);
      if (raw.length === rowCount) break;
      if (!rows.length) throw new Ga4Error('invalid_response', null, `${kind} paging`);
      offset += 250_000;
    }

    let result;
    try {
      result = transformRows(def, raw, { events: plan.reports.events, minUsers: plan.reports.minUsers, through });
    } catch (e) {
      if (e instanceof ReportShapeError) throw new Ga4Error('invalid_response', null, `${kind}: ${e.message}`);
      throw e;
    }
    const ins = db.prepare(`INSERT INTO ${def.table} VALUES (${cols.map(() => '?').join(', ')})`);
    for (const r of result.rows) {
      for (const v of r) if (typeof v === 'string' && secretShape(v)) throw new Ga4Error('invalid_response', null, `${kind}: secret-looking value`);
      ins.run(...r);
    }
    const samples = Array.isArray(meta.samplingMetadatas) ? meta.samplingMetadatas.filter(isObj) : [];
    const sampled = samples.length > 0 ? 1 : 0;
    // Sampling summary: numbers only (samples read / sampling space size)
    const num = (v: unknown) => (typeof v === 'string' && /^\d{1,20}$/.test(v)) || (typeof v === 'number' && Number.isSafeInteger(v)) ? String(v) : '?';
    const samplingSummary = samples.map((x) => `${num(x.samplesReadCount)}/${num(x.samplingSpaceSize)}`).join(';');
    const truncated = Array.isArray(meta.dataTruncationReasons) ? meta.dataTruncationReasons.map(String).join(',') : '';
    insLog.run(kind, def.table, result.rows.length, rowCount, result.stats.droppedSmall, result.stats.droppedUnobserved, result.stats.droppedUnmapped, result.stats.droppedCollision, result.stats.suppressedCells,
      meta.subjectToThresholding ? 1 : 0, meta.dataLossFromOtherRow ? 1 : 0, sampled, samplingSummary, truncated, meta.schemaRestrictionResponse ? 1 : 0,
      typeof meta.emptyReason === 'string' ? meta.emptyReason : '', plan.conn.timeZone, through, client.calls - callsBefore, quota.day, quota.hour, new Date(now()).toISOString());
  }
}
