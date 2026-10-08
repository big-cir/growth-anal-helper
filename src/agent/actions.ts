// 에이전트 행동 스키마와 검증.
import { readFileSync } from 'node:fs';
import { parsePanelSpec, PanelSpecError, type PanelSpec } from '../panels/spec.ts';

export const ACTION_SCHEMA_TEXT = readFileSync(new URL('./actions.schema.json', import.meta.url), 'utf8');
export const ACTION_SCHEMA_ARG = JSON.stringify(JSON.parse(ACTION_SCHEMA_TEXT));

export type AskQuestion = { id: string; text: string; options: { label: string; is_default: boolean }[]; allow_free_text: boolean };
export type Action =
  | { action: 'ask'; questions: AskQuestion[] }
  | { action: 'probe'; plan: string; purpose: string; sql: string }
  | { action: 'panel'; plan: string; panel: PanelSpec }
  | { action: 'refuse'; reason: string; alternatives: string[] };

export class ActionError extends Error {}

type Obj = Record<string, unknown>;

function only(o: Obj, keys: string[], path: string): void {
  for (const k of Object.keys(o)) if (!keys.includes(k)) throw new ActionError(`${path}.${k}: 알 수 없는 키`);
  for (const k of keys) if (!(k in o)) throw new ActionError(`${path}.${k}: 필수`);
}
function str(v: unknown, path: string, max: number): string {
  if (typeof v !== 'string' || v.trim() === '') throw new ActionError(`${path}: 비어 있지 않은 문자열`);
  if ([...v].length > max) throw new ActionError(`${path}: ${max}자 이하`);
  return v;
}
function obj(v: unknown, path: string): Obj {
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw new ActionError(`${path}: 객체여야 함`);
  return v as Obj;
}
function list(v: unknown, path: string, min: number, max: number): unknown[] {
  if (!Array.isArray(v) || v.length < min || v.length > max) throw new ActionError(`${path}: ${min}~${max}개 배열`);
  return v;
}

/** structured_output({ step: 행동 })을 행동으로 */
export function parseAction(raw: unknown): Action {
  const top = obj(raw, 'structured_output');
  only(top, ['step'], '');
  const o = obj(top.step, '.step');
  switch (o.action) {
    case 'ask': {
      only(o, ['action', 'questions'], '');
      const ids = new Set<string>();
      const questions = list(o.questions, '.questions', 1, 4).map((q, i) => {
        const p = `.questions[${i}]`;
        const x = obj(q, p);
        only(x, ['id', 'text', 'options', 'allow_free_text'], p);
        if (typeof x.id !== 'string' || !/^[a-z_]{1,32}$/.test(x.id)) throw new ActionError(`${p}.id: ^[a-z_]{1,32}$`);
        if (ids.has(x.id)) throw new ActionError(`${p}.id: 요청 안에서 중복`);
        ids.add(x.id);
        const options = list(x.options, `${p}.options`, 1, 4).map((op, j) => {
          const y = obj(op, `${p}.options[${j}]`);
          only(y, ['label', 'is_default'], `${p}.options[${j}]`);
          if (typeof y.is_default !== 'boolean') throw new ActionError(`${p}.options[${j}].is_default: true/false`);
          return { label: str(y.label, `${p}.options[${j}].label`, 60), is_default: y.is_default };
        });
        if (options.filter((op) => op.is_default).length !== 1) throw new ActionError(`${p}.options: 기본값(is_default: true)은 정확히 하나`);
        if (typeof x.allow_free_text !== 'boolean') throw new ActionError(`${p}.allow_free_text: true/false`);
        return { id: x.id, text: str(x.text, `${p}.text`, 200), options, allow_free_text: x.allow_free_text };
      });
      return { action: 'ask', questions };
    }
    case 'probe':
      only(o, ['action', 'plan', 'purpose', 'sql'], '');
      return { action: 'probe', plan: str(o.plan, '.plan', 200), purpose: str(o.purpose, '.purpose', 100), sql: str(o.sql, '.sql', 8000) };
    case 'panel': {
      only(o, ['action', 'plan', 'panel'], '');
      if (!o.panel || typeof o.panel !== 'object' || !('metric' in (o.panel as Obj))) throw new ActionError('.panel.metric: 필수(지표 사전 id 또는 null)');
      try {
        return { action: 'panel', plan: str(o.plan, '.plan', 300), panel: parsePanelSpec(stripNullRoles(o.panel)) };
      } catch (e) {
        if (e instanceof PanelSpecError) throw new ActionError(e.message);
        throw e;
      }
    }
    case 'refuse':
      only(o, ['action', 'reason', 'alternatives'], '');
      return {
        action: 'refuse',
        reason: str(o.reason, '.reason', 300),
        alternatives: list(o.alternatives, '.alternatives', 0, 3).map((a, i) => str(a, `.alternatives[${i}]`, 100)),
      };
    default:
      throw new ActionError(`action은 ask|probe|panel|refuse 중 하나 (받은 값: ${JSON.stringify(o.action)})`);
  }
}

/** null 역할 칸을 지운다 */
function stripNullRoles(panel: unknown): unknown {
  if (!panel || typeof panel !== 'object') return panel;
  const p = panel as Obj;
  if (!p.display || typeof p.display !== 'object') return panel;
  const d = Object.fromEntries(Object.entries(p.display as Obj).filter(([k, v]) => v !== null || k === 'headline'));
  return { ...p, display: d };
}
