// 새 스냅샷이 들어왔을 때 저장 패널을 어떻게 할지 정한다.
import type { PanelVersions, SavedPanel } from './store.ts';

/** 바뀌면 자동 재계산하지 않고 재검토로 돌리는 버전 */
export const RULE_KEYS = ['schema_version', 'policy_version', 'docs_version', 'prompt_version', 'pattern_contract_version'] as const;

export type RecomputeDecision = 'none' | 'auto' | 'review';

export function changedRules(saved: PanelVersions, current: PanelVersions): string[] {
  return RULE_KEYS.filter((k) => saved[k] !== current[k]);
}

export function decide(p: SavedPanel, current: PanelVersions): RecomputeDecision {
  if (changedRules(p.versions, current).length) return p.status === 'review' ? 'none' : 'review';
  if (p.status === 'review') return 'none';
  if (p.last_result.snapshot_id !== current.snapshot_id) return 'auto';
  return 'none';
}
