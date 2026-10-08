// 에이전트에게 보내는 턴 입력을 만든다. dataMode에 맞지 않는 내용은 보내지 않는다.
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
        if (mode !== 'pseudonymized') throw new OutboundBlocked('schema_only 모드에서 결과 행을 보낼 수 없음');
        if (p.target !== 'agent') throw new OutboundBlocked('가명 사본이 아닌 결과 행은 보낼 수 없음');
        out.push(`결과(가명 ID):\n\`\`\`json\n${JSON.stringify({ columns: p.columns, rows: p.rows.map((r) => r.map((t) => t[1])), more: p.more, truncated_cells: p.truncatedCells })}\n\`\`\``);
        break;
      case 'result':
        if (mode !== 'pseudonymized') throw new OutboundBlocked('schema_only 모드에서 결과 값을 보낼 수 없음');
        if (p.target !== 'agent') throw new OutboundBlocked('가명 사본이 아닌 결과는 보낼 수 없음');
        out.push(`패널 결과(가명 ID):\n\`\`\`json\n${JSON.stringify(p.value)}\n\`\`\``);
        break;
      case 'stats':
        out.push(`결과 통계(값은 보내지 않음):\n\`\`\`json\n${JSON.stringify({ row_count: p.rowCount, more: p.more, columns: p.columns })}\n\`\`\``);
        break;
      case 'violation':
        out.push(`위반(${p.stage}):\n\`\`\`json\n${JSON.stringify(p.items)}\n\`\`\``);
        break;
    }
  }
  return out.join('\n\n');
}

/** schema_only용 칸 통계 */
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
    if (recovery) pieces.push({ kind: 'text', text: `이전 대화 요약(새 세션으로 이어감):\n${recovery}` });
    pieces.push({ kind: 'text', text: `사용자 요청: ${userText}` });
    if (current) pieces.push({ kind: 'json', label: '현재 미리보기 패널 사양(수정 요청이면 이것을 바탕으로 고치세요)', value: specJson(current) });
    return this.render(pieces);
  }

  answers(questions: AskQuestion[], given: Record<string, string | undefined>): string {
    const value = questions.map((q) => {
      const a = given[q.id];
      if (a === undefined || a.trim() === '') return { id: q.id, answer: q.options.find((o) => o.is_default)!.label, defaulted: true };
      return { id: q.id, answer: a, defaulted: false };
    });
    return this.render([{ kind: 'text', text: '질문에 대한 답입니다. 답하지 않은 질문은 기본값으로 정했습니다.' }, { kind: 'json', label: '답', value }]);
  }

  probeResult(r: QueryResult, remainingProbes: number): string {
    const tail: Piece = { kind: 'text', text: `남은 탐색 횟수: ${remainingProbes}회.` };
    if (!r.ok) return this.render([{ kind: 'text', text: `탐색 쿼리 실패 (${r.kind}): ${r.message}` }, tail]);
    const head: Piece = { kind: 'text', text: `탐색 쿼리 결과: ${r.rows.length}행${r.more ? ' (더 있음, 50행까지만)' : ''}, ${r.ms}ms.` };
    if (this.mode === 'schema_only') {
      return this.render([head, { kind: 'stats', columns: columnStats(r.columns, r.rows, this.roles), rowCount: r.rows.length, more: r.more }, tail]);
    }
    return this.render([head, { kind: 'rows', target: 'agent', columns: r.columns.map((c) => c.name), rows: r.rows, more: r.more, truncatedCells: r.truncatedCells }, tail]);
  }

  panelFailure(r: Extract<PanelRunResult, { ok: false }>, remainingFixes: number): string {
    const stageName: Record<string, string> = {
      lint: '정적 검사', exec: '실행', contract: '결과 계약', invariant: '불변식', id_column: 'ID 칸 출력', id_dependent: 'ID 값 의존', metric_tables: '지표 사전의 표 규칙', cancelled: '취소',
    };
    const head = `패널 검사 실패 — 단계: ${stageName[r.stage] ?? r.stage}. 남은 수정 횟수: ${remainingFixes}회.`;
    if (this.mode === 'schema_only' && r.stage === 'invariant') {
      const items = (r.violations ?? []).slice(0, 20).map((v: Violation) => ({ rule: v.rule, index: v.index, column: v.column }));
      return this.render([{ kind: 'text', text: head }, { kind: 'violation', stage: r.stage, items }]);
    }
    return this.render([{ kind: 'text', text: `${head}\n${r.message}` }]);
  }

  offdictRejected(): string {
    return this.render([{ kind: 'text', text: '사용자가 지표 사전에 없는 정의로 만든 패널을 받지 않았습니다. 지표 사전의 지표 중 하나(metric에 그 id)로 다시 만드세요. 해당하는 사전 지표가 없다고 판단되면 refuse로 이유와 가까운 사전 지표를 알려 주세요.' }]);
  }

  schemaMismatch(error: string): string {
    return this.render([{ kind: 'text', text: `직전 응답이 행동 스키마에 맞지 않습니다: ${error}\n행동(ask | probe | panel | refuse) 하나를 스키마에 맞게 다시 내세요.` }]);
  }

  summarize(spec: PanelSpec, result: Record<string, unknown>): string {
    return this.render([
      { kind: 'text', text: '아래 패널의 "이 패널이 말해 주는 것"을 써 주세요.' },
      { kind: 'json', label: '패널 사양', value: specJson(spec) },
      { kind: 'result', target: 'agent', value: result },
    ]);
  }

  summaryRetry(problems: string[]): string {
    return this.render([{ kind: 'text', text: `직전 설명이 검사를 통과하지 못했습니다. 고쳐서 다시 내세요.\n${problems.map((p) => `- ${p}`).join('\n')}` }]);
  }

  /** 새 세션용 이전 대화 요약(8,000자 이내) */
  recoverySummary(o: { inputs: string[]; lastAsk: { questions: AskQuestion[]; answers: Record<string, string | undefined> } | null; current: PanelSpec | null }): string {
    const MAX = 8000;
    let inputs = o.inputs.slice(-5).map((t) => (t.length > 500 ? `${t.slice(0, 500)}…` : t));
    let current = o.current;
    let lastAsk = o.lastAsk;
    const build = () => {
      const pieces: Piece[] = [{ kind: 'json', label: '최근 사용자 입력', value: inputs }];
      if (lastAsk) {
        const cut = (t: string) => (t.length > 300 ? `${t.slice(0, 300)}…` : t);
        pieces.push({ kind: 'json', label: '최근 되묻기와 답', value: lastAsk.questions.map((q) => ({ question: q.text, answer: cut(lastAsk!.answers[q.id] ?? `${q.options.find((x) => x.is_default)!.label} (기본값)`) })) });
      }
      if (current) pieces.push({ kind: 'json', label: '현재 미리보기 패널 사양', value: specJson(current) });
      return this.render(pieces);
    };
    let text = build();
    while (text.length > MAX && inputs.length > 0) {
      inputs = inputs.slice(1);
      text = build();
    }
    // 그래도 넘으면 패널 SQL → 정의 → 패널 사양 순으로 줄인다
    if (text.length > MAX && current) {
      const over = text.length - MAX;
      current = { ...current, sql: `${current.sql.slice(0, Math.max(0, current.sql.length - over - 40))}\n…(길어서 생략)` };
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
