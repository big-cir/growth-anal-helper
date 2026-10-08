// 시스템 프롬프트 조립: 엔진 지침, 지표 사전, 스냅샷 스키마, 워크스페이스 설명서, 시드 패널, 현재 상태.
import { DatabaseSync } from 'node:sqlite';
import { readFileSync } from 'node:fs';
import { displayToJson, type PanelSpec } from '../panels/spec.ts';

export const CONTEXT_LIMIT = 60_000;
export const ENGINE_GUIDE = readFileSync(new URL('./engine-guide.md', import.meta.url), 'utf8');

export class ContextError extends Error {}

/** guide.md의 `표.칸` 설명(표 행 또는 목록) */
export function columnDescriptions(guide: string): Map<string, string> {
  const m = new Map<string, string>();
  for (const line of guide.split('\n')) {
    const t = /^\|\s*`([A-Za-z_][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*)`\s*\|\s*(.+?)\s*\|?\s*$/.exec(line);
    const l = /^\s*[-*]\s*`([A-Za-z_][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*)`\s*[:：—-]\s*(.+)$/.exec(line);
    const hit = t ?? l;
    if (hit) m.set(hit[1], hit[2].replace(/\s*\|\s*$/, ''));
  }
  return m;
}

/** 패널용 표와 탐색 전용 표로 나눈 스키마. tables는 읽을 수 있는 실제 표 이름 전부 */
export function snapshotSchema(agentPath: string, readablePrefixes: string[], panelPrefixes: string[], guide: string): { text: string; tables: string[] } {
  const desc = columnDescriptions(guide);
  const db = new DatabaseSync(agentPath, { readOnly: true });
  try {
    const tables = (db.prepare("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name").all() as { name: string }[])
      .map((r) => r.name)
      .filter((n) => readablePrefixes.some((p) => n.startsWith(p)));
    const describe = (t: string) => {
      const n = (db.prepare(`SELECT count(*) n FROM "${t}"`).get() as { n: number }).n;
      const out = [`### ${t} (${n}행)`];
      for (const c of db.prepare(`PRAGMA table_info("${t}")`).all() as { name: string; type: string }[]) {
        const d = desc.get(`${t}.${c.name}`);
        out.push(`- ${c.name}${c.type ? ` ${c.type}` : ''}${d ? ` — ${d}` : ''}`);
      }
      return out.join('\n');
    };
    const forPanel = tables.filter((t) => panelPrefixes.some((p) => t.startsWith(p)));
    const probeOnly = tables.filter((t) => !forPanel.includes(t));
    const text = [
      `## Tables for panels (panel SQL reads only these)\n\n${forPanel.map(describe).join('\n') || '(none)'}`,
      `## Probe-only tables (for probe checks only; panel SQL reading them is rejected)\n\n${probeOnly.map(describe).join('\n') || '(none)'}`,
    ].join('\n\n');
    return { text, tables };
  } finally {
    db.close();
  }
}

export type SeedPanel = { id: string; spec: PanelSpec };

export type ContextInput = {
  schema: string;
  /** 지표 사전 본문 */
  metrics: string;
  guide: string;
  seedPanels: SeedPanel[];
  state: { asOf: string; today: string; calendarStart: string | null; params: Record<string, string | number | string[]> };
};

function seedText(panels: SeedPanel[], withSql: boolean[]): string {
  return panels.map((p, i) => {
    const s = p.spec;
    const lines = [`### ${p.id}: ${s.title}`, `metric: ${s.metric ?? 'null (off-dictionary)'}`, `question: ${s.question}`, `pattern: ${JSON.stringify(displayToJson(s.display))}`, ...s.definition.map(([k, v]) => `- ${k}: ${v}`)];
    if (withSql[i]) lines.push('```sql', s.sql, '```');
    return lines.join('\n');
  }).join('\n\n');
}

/** 상한을 넘으면 시드 패널 SQL, 그다음 시드 패널을 뒤에서부터 뺀다 */
export function buildContext(o: ContextInput): string {
  const scalarParams = Object.fromEntries(Object.entries(o.state.params).filter(([, v]) => !Array.isArray(v)));
  const state = [
    '# Current state',
    `- Snapshot cutoff (:as_of): ${o.state.asOf}`,
    `- Today: ${o.state.today}`,
    ...(o.state.calendarStart ? [`- Data start (calendar_start): ${o.state.calendarStart}`] : []),
    `- Available parameters: :as_of${Object.keys(scalarParams).map((k) => `, :${k}`).join('')}`,
  ].join('\n');
  const assemble = (panels: SeedPanel[], withSql: boolean[]) =>
    [ENGINE_GUIDE.trim(), `# Metric dictionary\n\n${o.metrics}`, `# Snapshot schema (ID values are pseudonyms)\n\n${o.schema}`, `# Workspace guide\n\n${o.guide.trim()}`,
      panels.length ? `# Example panels (SQL that applies the interpretation rules correctly)\n\n${seedText(panels, withSql)}` : '', state]
      .filter(Boolean).join('\n\n---\n\n');

  let panels = [...o.seedPanels];
  const withSql = panels.map(() => true);
  let text = assemble(panels, withSql);
  for (let i = panels.length - 1; i >= 0 && text.length > CONTEXT_LIMIT; i--) {
    withSql[i] = false;
    text = assemble(panels, withSql);
  }
  while (panels.length > 0 && text.length > CONTEXT_LIMIT) {
    panels = panels.slice(0, -1);
    text = assemble(panels, withSql.slice(0, panels.length));
  }
  if (text.length > CONTEXT_LIMIT) throw new ContextError(`컨텍스트가 ${CONTEXT_LIMIT}자를 넘음(${text.length}자): 설명서(guide.md)를 줄여 주세요`);
  return text;
}
