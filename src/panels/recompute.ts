// Decides what to do with saved panels when a new snapshot arrives.
import type { PanelVersions, SavedPanel } from './store.ts';

/** Versions that send a panel to review instead of recomputing automatically when they change */
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
