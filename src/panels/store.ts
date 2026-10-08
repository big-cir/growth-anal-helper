// 저장 패널 파일(panels/<id>.json) 읽기·쓰기.
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

/** 계산 방식: 저장 시 미리보기, 새 스냅샷 자동, 규칙 변경 후 수동 */
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
  /** 패널 SQL이 참조한 표. 이전 기록에는 없을 수 있음 */
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
  /** 저장한 사용자. 이전 파일에는 없음(admin만 관리) */
  created_by?: string;
  /** 지표 사전 이전 형식(metric 없음). 다시 계산하지 않고 재생성만 */
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
    if (!ID_RE.test(id)) throw new Error(`패널 ID 형식 오류: ${id}`);
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

  /** 만든 순서대로 */
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
    // 이전 형식이면 metric 키 없이 써서 표시를 유지한다
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

