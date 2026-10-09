// Builds the system prompt: engine guide, metric dictionary, snapshot schema, workspace guide, seed panels, current state.
import { DatabaseSync } from 'node:sqlite';
import { readFileSync } from 'node:fs';
import { displayToJson, type PanelSpec } from '../panels/spec.ts';
import { language } from '../i18n.ts';

export const CONTEXT_LIMIT = 60_000;
export const ENGINE_GUIDE = readFileSync(new URL('./engine-guide.md', import.meta.url), 'utf8');

export const LANGUAGE_NAMES = { en: 'English', ko: 'Korean' } as const;

/** Engine guide with the configured output language filled in */
export function engineGuide(): string {
  return ENGINE_GUIDE.replaceAll('{{LANGUAGE}}', LANGUAGE_NAMES[language()]);
}

export class ContextError extends Error {}

/** `table.column` descriptions in guide.md (table rows or list items) */
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

/** Schema split into panel tables and probe-only tables. tables lists every readable table */
export function snapshotSchema(agentPath: string, readablePrefixes: string[], panelPrefixes: string[], guide: string): { text: string; tables: string[] } {
  const desc = columnDescriptions(guide);
  const db = new DatabaseSync(agentPath, { readOnly: true });
  try {
    const tables = (db.prepare("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name").all() as { name: string }[])
      .map((r) => r.name)
      .filter((n) => readablePrefixes.some((p) => n.startsWith(p)));
    const describe = (t: string) => {
      const n = (db.prepare(`SELECT count(*) n FROM "${t}"`).get() as { n: number }).n;
      const out = [`### ${t} (${n} rows)`];
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
  /** Metric dictionary text */
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

/** Over the limit: drop seed panel SQL, then seed panels from the end */
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
    [engineGuide().trim(), `# Metric dictionary\n\n${o.metrics}`, `# Snapshot schema (ID values are pseudonyms)\n\n${o.schema}`, `# Workspace guide\n\n${o.guide.trim()}`,
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
  if (text.length > CONTEXT_LIMIT) throw new ContextError(`context is over ${CONTEXT_LIMIT} characters (${text.length}): shorten guide.md`);
  return text;
}
