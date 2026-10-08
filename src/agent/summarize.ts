// 저장할 패널의 긴 설명 만들기: 새 세션으로 호출하고, 검사에 실패하면 한 번 더 요청한다.
import { readFileSync } from 'node:fs';
import type { ResultColumn } from '../query/worker.ts';
import type { Row } from '../panels/contract.ts';
import type { PanelSpec } from '../panels/spec.ts';
import { checkSummary, parseSummary, summaryResult, SummaryError } from '../panels/summary-check.ts';
import type { CallResult } from './claude.ts';
import type { Outbound } from './outbound.ts';
import { secretAssignment, secretShape } from './sensitive.ts';

export const SUMMARY_SCHEMA_ARG = JSON.stringify(JSON.parse(readFileSync(new URL('./summary.schema.json', import.meta.url), 'utf8')));
export const SUMMARY_GUIDE = readFileSync(new URL('./summary-guide.md', import.meta.url), 'utf8');

export type SummaryOutcome = { status: 'ok'; text: string } | { status: 'failed'; message: string };

export type SummarizeInput = {
  spec: PanelSpec;
  columns: ResultColumn[];
  /** 가명 사본 결과 */
  agentRows: Row[];
  outbound: Outbound;
  call(o: { input: string; sessionId: string | null }): Promise<CallResult>;
};

export async function summarize(o: SummarizeInput): Promise<SummaryOutcome> {
  let input = o.outbound.summarize(o.spec, summaryResult(o.columns, o.agentRows));
  let sessionId: string | null = null;
  let last = '';
  for (let attempt = 0; attempt < 2; attempt++) {
    const res = await o.call({ input, sessionId });
    if (!res.ok) return { status: 'failed', message: res.message };
    sessionId = res.sessionId;
    let problems: string[];
    try {
      const check = checkSummary(parseSummary(res.structured), o.spec, o.columns, o.agentRows);
      if (check.ok) {
        if (secretShape(check.text) || secretAssignment(check.text)) return { status: 'failed', message: '설명에 비밀처럼 보이는 값이 있어 버렸어요' };
        return { status: 'ok', text: check.text };
      }
      problems = check.problems;
    } catch (e) {
      if (!(e instanceof SummaryError)) throw e;
      problems = [e.message];
    }
    last = problems.join('; ');
    input = o.outbound.summaryRetry(problems);
  }
  return { status: 'failed', message: `설명 검사 실패: ${last}` };
}
