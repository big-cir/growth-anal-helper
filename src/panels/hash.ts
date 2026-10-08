// 해시와 버전.
import { createHash } from 'node:crypto';
import { ALLOWED_FUNCTIONS } from '../query/authorizer.ts';
import { LINT_RULES_VERSION } from '../query/sql-lint.ts';
import { PATTERN_CONTRACT_VERSION } from './contract.ts';
import { displayToJson, type PanelSpec } from './spec.ts';

/** 화면 렌더러가 바뀌면 올린다 */
export const RENDERER_VERSION = 2;

const sha = (s: string) => createHash('sha256').update(s).digest('hex');

export function normalizeSql(sql: string): string {
  return sql.split(/\r?\n/).map((l) => l.trimEnd()).join('\n').replace(/\n{3,}/g, '\n\n').trim();
}

/** 같은 해시면 같은 패널 */
export function semanticHash(spec: PanelSpec, paramsHash: string): string {
  return sha(JSON.stringify([normalizeSql(spec.sql), paramsHash, displayToJson(spec.display), spec.definition, spec.answers, spec.metric]));
}

export const schemaVersion = (derivedSql: string, rolesHash: string) => sha(`${derivedSql}\n${rolesHash}`);

export const policyVersion = (readablePrefixes: string[], panelReadablePrefixes: string[]) =>
  sha(JSON.stringify([[...readablePrefixes].sort(), [...panelReadablePrefixes].sort(), [...ALLOWED_FUNCTIONS].sort(), LINT_RULES_VERSION, PATTERN_CONTRACT_VERSION]));

export const docsVersion = (guide: string, seedPanels: { name: string; text: string }[], metrics: string) =>
  sha(JSON.stringify([guide, [...seedPanels].sort((a, b) => (a.name < b.name ? -1 : 1)).map((p) => [p.name, p.text]), metrics]));

export const promptVersion = (engineGuide: string, actionSchema: string) => sha(JSON.stringify([engineGuide, actionSchema]));

export type ContextVersion = { snapshot_id: string; schema_version: string; policy_version: string; docs_version: string; prompt_version: string };

export const contextVersionKey = (v: ContextVersion) =>
  [v.snapshot_id, v.schema_version, v.policy_version, v.docs_version, v.prompt_version].join(':');

export const resultCacheKey = (semantic: string, ctx: ContextVersion) =>
  sha(JSON.stringify([semantic, contextVersionKey(ctx), PATTERN_CONTRACT_VERSION, RENDERER_VERSION]));
