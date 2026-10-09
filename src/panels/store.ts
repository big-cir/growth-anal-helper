// Reads and writes saved panel files (panels/<id>.json).
import { existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import type { ResultColumn } from '../query/worker.ts';
import type { Headline, Row } from './contract.ts';
import type { ContextVersion } from './hash.ts';
import { displayToJson, parsePanelSpec } from './spec.ts';
import type { PanelSpec } from './spec.ts';

export type PanelStatus = 'ok' | 'review' | 'recompute_failed' | 'id_dependent';
export type JobStatus = 'idle' | 'queued' | 'running' | 'cancelling' | 'cancelled';

export type PanelVersions = ContextVersion & { pattern_contract_version: number; renderer_version: number };

/** How it was computed: preview at save, automatic on a new snapshot, manual after a rule change */
export type ResultMode = 'preview' | 'auto' | 'manual_rule';

export type LastResult = {
  snapshot_id: string;
  as_of: string;
  computed_at: string;
  mode: ResultMode;
  columns: ResultColumn[];
  rows: Row[];
  caveats: string[];
  headline: Headline;
  /** Tables the panel SQL read. May be missing in older records */
  tables?: string[];
};

export type SavedPanel = {
  id: string;
  title: string;
  description: string;
  summary: string;
  summary_snapshot_id: string | null;
  prompt: string;
  spec: PanelSpec;
  versions: PanelVersions;
  generated_model: string;
  preview_hash: string;
  created_at: string;
  status: PanelStatus;
  last_result: LastResult;
  last_error: { at: string; message: string } | null;
  /** User who saved it. Missing in older files (admin-only) */
  created_by?: string;
  /** Pre-dictionary format (no metric). Never recomputed, only regenerated */
  legacy?: boolean;
};

const ID_RE = /^[a-z0-9]{12}$/;

export class PanelStore {
  readonly dir: string;

  constructor(dir: string) {
    this.dir = dir;
    mkdirSync(dir, { recursive: true });
  }

  private file(id: string): string {
    if (!ID_RE.test(id)) throw new Error(`invalid panel id: ${id}`);
    return join(this.dir, `${id}.json`);
  }

  get(id: string): SavedPanel | null {
    if (!ID_RE.test(id)) return null;
    const p = this.file(id);
    if (!existsSync(p)) return null;
    const raw = JSON.parse(readFileSync(p, 'utf8')) as SavedPanel;
    const legacy = !('metric' in (raw.spec as object));
    return { ...raw, spec: parsePanelSpec(raw.spec), ...(legacy ? { legacy: true } : {}) };
  }

  /** In creation order */
  list(): SavedPanel[] {
    return readdirSync(this.dir)
      .filter((f) => /^[a-z0-9]{12}\.json$/.test(f))
      .map((f) => this.get(f.slice(0, 12)))
      .filter((p): p is SavedPanel => p !== null)
      .sort((a, b) => (a.created_at < b.created_at ? -1 : a.created_at > b.created_at ? 1 : 0));
  }

  write(p: SavedPanel): void {
    const target = this.file(p.id);
    const tmp = `${target}.tmp-${process.pid}`;
    const { legacy, ...rest } = p;
    // Old format: write without the metric key to keep it marked
    const { metric, ...specRest } = p.spec;
    const spec = legacy ? { ...specRest, display: displayToJson(p.spec.display) } : { metric, ...specRest, display: displayToJson(p.spec.display) };
    writeFileSync(tmp, JSON.stringify({ ...rest, spec }, null, 1) + '\n');
    renameSync(tmp, target);
  }

  delete(id: string): boolean {
    const p = this.file(id);
    if (!existsSync(p)) return false;
    rmSync(p);
    return true;
  }
}

