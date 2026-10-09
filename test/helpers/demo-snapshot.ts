// Builds a fixed-time snapshot from the demo workspace.
import { cpSync, mkdirSync, mkdtempSync, readdirSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { seedDemo } from '../../examples/demo/seed.ts';
import { collect } from '../../src/collect/collector.ts';
import { allRoles, finalize } from '../../src/collect/finalize.ts';
import { SqliteSource } from '../../src/collect/sources/sqlite.ts';
import { loadBuildInputs } from '../../src/snapshot/pipeline.ts';
import { loadWorkspace, type Workspace } from '../../src/workspace.ts';
import { parsePanelSpec, type PanelSpec } from '../../src/panels/spec.ts';
import type { Role } from '../../src/collect/spec.ts';

export const DEMO_DIR = join(import.meta.dirname, '..', '..', 'examples', 'demo');
export const DEMO_ANCHOR = '2024-06-03 12:00:00';
export const DEMO_CUTOFF = '2024-06-03 12:00:00.000000';

class FixedNow extends SqliteSource {
  private readonly fixed: string;
  constructor(path: string, fixed: string) {
    super(path);
    this.fixed = fixed;
  }
  override async now() { return this.fixed; }
}

export type DemoSnapshot = { ws: Workspace; real: string; agent: string; asOf: string; roles: Map<string, Role> };

/** mutate: edits workspace files before collecting */
export async function buildDemoSnapshot(o: { mutate?: (dir: string) => void } = {}): Promise<DemoSnapshot> {
  const dir = mkdtempSync(join(tmpdir(), 'gl-demo-'));
  for (const f of ['workspace.json', 'tables.json', 'derived.sql', 'derived-columns.json', 'metrics.json', 'guide.md', 'seed-panels']) cpSync(join(DEMO_DIR, f), join(dir, f), { recursive: true });
  o.mutate?.(dir);
  seedDemo(join(dir, '.out', 'source.sqlite'), { anchor: DEMO_ANCHOR });
  const ws = loadWorkspace(dir);
  // Server tests also check sign-in and roles
  ws.config.server.auth = true;
  const inputs = loadBuildInputs(ws);
  const snaps = join(ws.config.outDir, 'snapshots');
  mkdirSync(snaps, { recursive: true });
  const c = await collect({ specs: inputs.specs, source: new FixedNow(join(dir, '.out', 'source.sqlite'), DEMO_ANCHOR), snapshotsDir: snaps });
  const r = finalize({ tmpPath: c.tmpPath, snapshotsDir: snaps, inputs, meta: { cutoff: c.cutoff, startedAt: c.startedAt, finishedAt: c.finishedAt }, applyCutoffFirst: true });
  return { ws, real: r.files.real, agent: r.files.agent, asOf: c.cutoff, roles: allRoles(inputs.specs, inputs.derivedRoles) };
}

export function demoSeedPanels(): { id: string; spec: PanelSpec }[] {
  const dir = join(DEMO_DIR, 'seed-panels');
  return readdirSync(dir).filter((f) => f.endsWith('.json')).sort().map((f) => {
    const raw = JSON.parse(readFileSync(join(dir, f), 'utf8'));
    return { id: raw.id as string, spec: parsePanelSpec(raw) };
  });
}
