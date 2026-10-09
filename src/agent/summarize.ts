// Writes the long description of a panel being saved: a new session, retried once if the check fails.
import { readFileSync } from 'node:fs';
import type { ResultColumn } from '../query/worker.ts';
import type { Row } from '../panels/contract.ts';
import type { PanelSpec } from '../panels/spec.ts';
import { checkSummary, claimUnits, parseSummary, summaryResult, SummaryError } from '../panels/summary-check.ts';
import { LANGUAGE_NAMES } from './context.ts';
import type { CallResult } from './runner.ts';
import type { Outbound } from './outbound.ts';
import { secretAssignment, secretShape } from './sensitive.ts';
import { language, tr } from '../i18n.ts';

export const SUMMARY_SCHEMA_ARG = JSON.stringify(JSON.parse(readFileSync(new URL('./summary.schema.json', import.meta.url), 'utf8')));
export const SUMMARY_GUIDE = readFileSync(new URL('./summary-guide.md', import.meta.url), 'utf8');

/** Summary guide with the configured language and units filled in */
export function summaryGuide(): string {
  const u = claimUnits();
  return SUMMARY_GUIDE.replaceAll('{{LANGUAGE}}', LANGUAGE_NAMES[language()]).replaceAll('{{DIFF_UNIT}}', u.diff).replaceAll('{{RATIO_UNIT}}', u.ratio);
}

export type SummaryOutcome = { status: 'ok'; text: string } | { status: 'failed'; message: string };

export type SummarizeInput = {
  spec: PanelSpec;
  columns: ResultColumn[];
  /** Result from the pseudonymized copy */
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
        if (secretShape(check.text) || secretAssignment(check.text)) return { status: 'failed', message: tr('The description looked like it contained a secret, so it was discarded', '설명에 비밀처럼 보이는 값이 있어 버렸어요') };
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
  return { status: 'failed', message: tr(`Description check failed: ${last}`, `설명 검사 실패: ${last}`) };
}
