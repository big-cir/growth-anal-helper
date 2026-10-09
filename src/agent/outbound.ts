// Builds the turn input sent to the agent. Nothing that the dataMode forbids is sent.
import type { Role } from '../collect/spec.ts';
import type { Violation } from '../panels/contract.ts';
import type { PanelRunResult } from '../panels/run.ts';
import { displayToJson, type PanelSpec } from '../panels/spec.ts';
import type { QueryResult } from '../query/executor.ts';
import type { ResultColumn, Tagged } from '../query/worker.ts';
import type { AskQuestion } from './actions.ts';

export type DataMode = 'pseudonymized' | 'schema_only';

export type Piece =
  | { kind: 'text'; text: string }
  | { kind: 'json'; label: string; value: unknown }
  | { kind: 'rows'; target: 'agent'; columns: string[]; rows: Tagged[][]; more: boolean; truncatedCells: number }
  | { kind: 'stats'; columns: ColumnStats[]; rowCount: number; more: boolean }
  | { kind: 'result'; target: 'agent'; value: Record<string, unknown> }
  | { kind: 'violation'; stage: string; items: { rule: string; index: number | null; column: string | null }[] };

export type ColumnStats = { name: string; nulls: number; min: number | null; max: number | null; identifier: boolean };

export class OutboundBlocked extends Error {}

export function render(mode: DataMode, pieces: Piece[]): string {
  const out: string[] = [];
  for (const p of pieces) {
    switch (p.kind) {
      case 'text':
        out.push(p.text);
        break;
      case 'json':
        out.push(`${p.label}:\n\`\`\`json\n${JSON.stringify(p.value, null, 1)}\n\`\`\``);
        break;
      case 'rows':
        if (mode !== 'pseudonymized') throw new OutboundBlocked('result rows cannot be sent in schema_only mode');
        if (p.target !== 'agent') throw new OutboundBlocked('only rows from the pseudonymized copy can be sent');
        out.push(`Result (pseudonymous IDs):\n\`\`\`json\n${JSON.stringify({ columns: p.columns, rows: p.rows.map((r) => r.map((t) => t[1])), more: p.more, truncated_cells: p.truncatedCells })}\n\`\`\``);
        break;
      case 'result':
        if (mode !== 'pseudonymized') throw new OutboundBlocked('result values cannot be sent in schema_only mode');
        if (p.target !== 'agent') throw new OutboundBlocked('only results from the pseudonymized copy can be sent');
        out.push(`Panel result (pseudonymous IDs):\n\`\`\`json\n${JSON.stringify(p.value)}\n\`\`\``);
        break;
      case 'stats':
        out.push(`Result statistics (values not sent):\n\`\`\`json\n${JSON.stringify({ row_count: p.rowCount, more: p.more, columns: p.columns })}\n\`\`\``);
        break;
      case 'violation':
        out.push(`Violations (${p.stage}):\n\`\`\`json\n${JSON.stringify(p.items)}\n\`\`\``);
        break;
    }
  }
  return out.join('\n\n');
}

/** Column statistics for schema_only */
export function columnStats(columns: ResultColumn[], rows: Tagged[][], roles: Map<string, Role>): ColumnStats[] {
  return columns.map((c, i) => {
    const role = c.table && c.column ? roles.get(`${c.table}.${c.column}`) : undefined;
    const identifier = role !== undefined && role !== 'ordinary';
    let nulls = 0;
    let min: number | null = null;
    let max: number | null = null;
    for (const r of rows) {
      const [tag, v] = r[i];
      if (tag === 'n') nulls++;
      else if ((tag === 'i' || tag === 'f') && !identifier) {
        min = min === null ? (v as number) : Math.min(min, v as number);
        max = max === null ? (v as number) : Math.max(max, v as number);
      }
    }
    return { name: c.name, nulls, min, max, identifier };
  });
}

const specJson = (s: PanelSpec) => ({ ...s, display: displayToJson(s.display) });

export class Outbound {
  readonly mode: DataMode;
  private readonly roles: Map<string, Role>;

  constructor(mode: DataMode, roles: Map<string, Role>) {
    this.mode = mode;
    this.roles = roles;
  }

  private render(pieces: Piece[]): string {
    return render(this.mode, pieces);
  }

  request(userText: string, current: PanelSpec | null, recovery: string | null): string {
    const pieces: Piece[] = [];
    if (recovery) pieces.push({ kind: 'text', text: `Summary of the earlier conversation (continuing in a new session):\n${recovery}` });
    pieces.push({ kind: 'text', text: `User request: ${userText}` });
    if (current) pieces.push({ kind: 'json', label: 'Current preview panel spec (if this is a change request, edit this one)', value: specJson(current) });
    return this.render(pieces);
  }

  answers(questions: AskQuestion[], given: Record<string, string | undefined>): string {
    const value = questions.map((q) => {
      const a = given[q.id];
      if (a === undefined || a.trim() === '') return { id: q.id, answer: q.options.find((o) => o.is_default)!.label, defaulted: true };
      return { id: q.id, answer: a, defaulted: false };
    });
    return this.render([{ kind: 'text', text: 'Answers to your questions. Unanswered questions were set to their defaults.' }, { kind: 'json', label: 'Answers', value }]);
  }

  probeResult(r: QueryResult, remainingProbes: number): string {
    const tail: Piece = { kind: 'text', text: `Probes left: ${remainingProbes}.` };
    if (!r.ok) return this.render([{ kind: 'text', text: `Probe query failed (${r.kind}): ${r.message}` }, tail]);
    const head: Piece = { kind: 'text', text: `Probe query result: ${r.rows.length} rows${r.more ? ' (more exist, first 50 only)' : ''}, ${r.ms}ms.` };
    if (this.mode === 'schema_only') {
      return this.render([head, { kind: 'stats', columns: columnStats(r.columns, r.rows, this.roles), rowCount: r.rows.length, more: r.more }, tail]);
    }
    return this.render([head, { kind: 'rows', target: 'agent', columns: r.columns.map((c) => c.name), rows: r.rows, more: r.more, truncatedCells: r.truncatedCells }, tail]);
  }

  panelFailure(r: Extract<PanelRunResult, { ok: false }>, remainingFixes: number): string {
    const stageName: Record<string, string> = {
      lint: 'static check', exec: 'execution', contract: 'result contract', invariant: 'invariants', id_column: 'ID column in output', id_dependent: 'depends on ID values', metric_tables: 'metric dictionary table rule', cancelled: 'cancelled',
    };
    const head = `Panel check failed — stage: ${stageName[r.stage] ?? r.stage}. Fixes left: ${remainingFixes}.`;
    if (this.mode === 'schema_only' && r.stage === 'invariant') {
      const items = (r.violations ?? []).slice(0, 20).map((v: Violation) => ({ rule: v.rule, index: v.index, column: v.column }));
      return this.render([{ kind: 'text', text: head }, { kind: 'violation', stage: r.stage, items }]);
    }
    return this.render([{ kind: 'text', text: `${head}\n${r.message}` }]);
  }

  zeroResult(): string {
    return this.render([{ kind: 'text', text: 'Every number column in the panel result is 0 (or NULL). Check that the join and filter conditions really match. In particular, comparing a date string (YYYY-MM-DD) with a timestamp string (YYYY-MM-DD HH:MM:SS.ffffff) as-is matches no rows. Use a probe if needed, then send the fixed panel. If 0 is correct, send the same panel again and it will be accepted.' }]);
  }

  offdictRejected(): string {
    return this.render([{ kind: 'text', text: 'The user did not accept a panel built on a definition outside the metric dictionary. Rebuild it with one of the dictionary metrics (its id in metric). If no dictionary metric fits, refuse and give the reason and the closest dictionary metrics.' }]);
  }

  schemaMismatch(error: string): string {
    return this.render([{ kind: 'text', text: `Your last reply does not match the action schema: ${error}\nSend one action (ask | probe | panel | refuse) that matches the schema.` }]);
  }

  summarize(spec: PanelSpec, result: Record<string, unknown>): string {
    return this.render([
      { kind: 'text', text: 'Write "what this panel tells you" for the panel below.' },
      { kind: 'json', label: 'Panel spec', value: specJson(spec) },
      { kind: 'result', target: 'agent', value: result },
    ]);
  }

  summaryRetry(problems: string[]): string {
    return this.render([{ kind: 'text', text: `Your last description failed the check. Fix it and send it again.\n${problems.map((p) => `- ${p}`).join('\n')}` }]);
  }

  /** Summary of the earlier conversation for a new session (8,000 characters or fewer) */
  recoverySummary(o: { inputs: string[]; lastAsk: { questions: AskQuestion[]; answers: Record<string, string | undefined> } | null; current: PanelSpec | null }): string {
    const MAX = 8000;
    let inputs = o.inputs.slice(-5).map((t) => (t.length > 500 ? `${t.slice(0, 500)}…` : t));
    let current = o.current;
    let lastAsk = o.lastAsk;
    const build = () => {
      const pieces: Piece[] = [{ kind: 'json', label: 'Recent user inputs', value: inputs }];
      if (lastAsk) {
        const cut = (t: string) => (t.length > 300 ? `${t.slice(0, 300)}…` : t);
        pieces.push({ kind: 'json', label: 'Latest questions and answers', value: lastAsk.questions.map((q) => ({ question: q.text, answer: cut(lastAsk!.answers[q.id] ?? `${q.options.find((x) => x.is_default)!.label} (default)`) })) });
      }
      if (current) pieces.push({ kind: 'json', label: 'Current preview panel spec', value: specJson(current) });
      return this.render(pieces);
    };
    let text = build();
    while (text.length > MAX && inputs.length > 0) {
      inputs = inputs.slice(1);
      text = build();
    }
    // Still too long: shorten panel SQL, then definition, then the spec
    if (text.length > MAX && current) {
      const over = text.length - MAX;
      current = { ...current, sql: `${current.sql.slice(0, Math.max(0, current.sql.length - over - 40))}\n…(truncated)` };
      text = build();
    }
    if (text.length > MAX && current) {
      current = { ...current, definition: current.definition.slice(0, 2), caveats: [] };
      text = build();
    }
    if (text.length > MAX) {
      current = null;
      text = build();
    }
    if (text.length > MAX) {
      lastAsk = null;
      text = build();
    }
    while (text.length > MAX && inputs.length > 0) {
      inputs = inputs.slice(1);
      text = build();
    }
    return text;
  }
}
