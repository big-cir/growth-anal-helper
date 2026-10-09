// API agent (Anthropic, OpenAI-compatible): no tools, JSON replies only. The engine stores the conversation and resends it each call.
import { randomBytes } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import type { CallErrorType, CallOptions, CallResult } from './runner.ts';
import { secretAssignment, secretShape } from './sensitive.ts';

export type ApiProvider = 'anthropic' | 'openai';
export type Pricing = { inputPerMTok: number; outputPerMTok: number };
type Turn = { role: 'user' | 'assistant'; text: string };
type Usage = { input: number; output: number };
type Reply = { ok: true; structured: unknown; usage: Usage } | { ok: false; type: CallErrorType; message: string; usage: Usage };

export const DEFAULT_BASE_URLS: Record<ApiProvider, string> = {
  anthropic: 'https://api.anthropic.com',
  openai: 'https://api.openai.com/v1',
};
const TOOL = 'respond';
const SESSION_RE = /^api_[0-9a-f]{24}$/;
const ERROR_TEXT_MAX = 300;

export class ApiRunner {
  private readonly o: { provider: ApiProvider; baseUrl: string | null; apiKey: string; pricing: Pricing | null; maxOutputTokens: number; sessionDir: string };

  constructor(o: ApiRunner['o']) {
    this.o = o;
    mkdirSync(o.sessionDir, { recursive: true, mode: 0o700 });
  }

  async call(c: CallOptions): Promise<CallResult> {
    const t0 = performance.now();
    const ms = () => Math.round(performance.now() - t0);
    const fail = (type: CallErrorType, message: string, costUsd = 0, sessionId: string | null = c.sessionId ?? null): CallResult => ({ ok: false, type, message, sessionId, costUsd, ms: ms() });
    if (c.signal?.aborted) return fail('cancelled', 'cancelled');
    if (!c.model) return fail('error', 'agent.model is required');

    let history: Turn[] = [];
    if (c.sessionId) {
      const h = this.load(c.sessionId);
      if (!h) return fail('resume', 'could not resume session', 0, null);
      history = h;
    }
    const messages: Turn[] = [...history, { role: 'user', text: c.input }];
    const timeout = AbortSignal.timeout(c.timeoutMs);
    const signal = c.signal ? AbortSignal.any([c.signal, timeout]) : timeout;

    let reply: Reply;
    try {
      reply = this.o.provider === 'anthropic'
        ? await this.anthropic(c, messages, signal)
        : await this.openai(c, messages, signal);
    } catch (e) {
      if (c.signal?.aborted) return fail('cancelled', 'cancelled');
      if (timeout.aborted) return fail('timeout', `call timed out (${c.timeoutMs}ms)`);
      return fail('process', `API connection failed: ${(e as Error).message}`);
    }
    const costUsd = this.cost(reply.usage);
    if (!reply.ok) return fail(reply.type, reply.message, costUsd);
    if (costUsd > c.budgetUsd) return fail('budget', 'call budget reached', costUsd);

    const id = c.sessionId ?? `api_${randomBytes(12).toString('hex')}`;
    this.save(id, [...messages, { role: 'assistant', text: JSON.stringify(reply.structured) }]);
    return { ok: true, sessionId: id, structured: reply.structured, costUsd, ms: ms() };
  }

  private cost(u: Usage): number {
    const p = this.o.pricing;
    return p ? (u.input * p.inputPerMTok + u.output * p.outputPerMTok) / 1e6 : 0;
  }

  private async anthropic(c: CallOptions, messages: Turn[], signal: AbortSignal): Promise<Reply> {
    const res = await fetch(`${(this.o.baseUrl ?? DEFAULT_BASE_URLS.anthropic).replace(/\/$/, '')}/v1/messages`, {
      method: 'POST',
      signal,
      headers: { 'content-type': 'application/json', 'x-api-key': this.o.apiKey, 'anthropic-version': '2023-06-01' },
      body: JSON.stringify({
        model: c.model,
        max_tokens: this.o.maxOutputTokens,
        system: c.systemPrompt,
        messages: messages.map((m) => ({ role: m.role, content: m.text })),
        tools: [{ name: TOOL, description: 'Give the answer in this format', input_schema: JSON.parse(c.jsonSchema) }],
        tool_choice: { type: 'tool', name: TOOL },
      }),
    });
    const body = await readBody(res);
    const u = (body?.usage ?? {}) as Record<string, unknown>;
    const usage = { input: n(u.input_tokens) + n(u.cache_creation_input_tokens) + n(u.cache_read_input_tokens), output: n(u.output_tokens) };
    if (!res.ok) return httpError(res.status, body, usage);
    if (body?.stop_reason === 'max_tokens') return { ok: false, type: 'error', message: 'response cut off at the output limit (maxOutputTokens)', usage };
    const block = Array.isArray(body?.content) ? (body.content as Record<string, unknown>[]).find((b) => b.type === 'tool_use' && b.name === TOOL) : undefined;
    if (!block) return { ok: false, type: 'error', message: 'response has no structured output', usage };
    return { ok: true, structured: block.input, usage };
  }

  private async openai(c: CallOptions, messages: Turn[], signal: AbortSignal): Promise<Reply> {
    const schema = JSON.parse(c.jsonSchema);
    const res = await fetch(`${(this.o.baseUrl ?? DEFAULT_BASE_URLS.openai).replace(/\/$/, '')}/chat/completions`, {
      method: 'POST',
      signal,
      headers: { 'content-type': 'application/json', ...(this.o.apiKey ? { authorization: `Bearer ${this.o.apiKey}` } : {}) },
      body: JSON.stringify({
        model: c.model,
        // Some servers ignore response_format, so the schema also goes in the instructions
        messages: [
          { role: 'system', content: `${c.systemPrompt}\n\nRespond with one JSON object that matches this JSON Schema, and nothing else:\n${c.jsonSchema}` },
          ...messages.map((m) => ({ role: m.role, content: m.text })),
        ],
        response_format: { type: 'json_schema', json_schema: { name: TOOL, schema, strict: false } },
      }),
    });
    const body = await readBody(res);
    const u = (body?.usage ?? {}) as Record<string, unknown>;
    const usage = { input: n(u.prompt_tokens), output: n(u.completion_tokens) };
    if (!res.ok) return httpError(res.status, body, usage);
    const choice = Array.isArray(body?.choices) ? (body.choices[0] as Record<string, unknown> | undefined) : undefined;
    const msg = (choice?.message ?? {}) as Record<string, unknown>;
    if (choice?.finish_reason === 'length') return { ok: false, type: 'error', message: 'response cut off at the output limit (maxOutputTokens)', usage };
    if (typeof msg.refusal === 'string' && msg.refusal) return { ok: false, type: 'error', message: 'model refused to answer', usage };
    if (typeof msg.content !== 'string') return { ok: false, type: 'error', message: 'response has no content', usage };
    try {
      return { ok: true, structured: JSON.parse(stripFence(msg.content)), usage };
    } catch {
      return { ok: true, structured: { not_json: msg.content.slice(0, 200) }, usage };
    }
  }

  private file(id: string): string {
    return join(this.o.sessionDir, `${id}.json`);
  }

  private load(id: string): Turn[] | null {
    if (!SESSION_RE.test(id) || !existsSync(this.file(id))) return null;
    try {
      const v = JSON.parse(readFileSync(this.file(id), 'utf8')) as { provider?: string; turns?: Turn[] };
      return v.provider === this.o.provider && Array.isArray(v.turns) ? v.turns : null;
    } catch {
      return null;
    }
  }

  private save(id: string, turns: Turn[]): void {
    const tmp = `${this.file(id)}.tmp-${process.pid}`;
    writeFileSync(tmp, JSON.stringify({ provider: this.o.provider, turns }), { mode: 0o600 });
    renameSync(tmp, this.file(id));
  }
}

const n = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

/** Models sometimes wrap JSON in a ```json fence */
function stripFence(s: string): string {
  const m = /^\s*```(?:json)?\s*([\s\S]*?)\s*```\s*$/.exec(s);
  return m ? m[1] : s;
}

async function readBody(res: Response): Promise<Record<string, any> | null> {
  try {
    return (await res.json()) as Record<string, any>;
  } catch {
    return null;
  }
}

function httpError(status: number, body: Record<string, any> | null, usage: Usage): Reply {
  let detail = String(body?.error?.message ?? body?.message ?? '').slice(0, ERROR_TEXT_MAX);
  if (secretShape(detail) || secretAssignment(detail)) detail = '';
  const tail = detail ? `: ${detail}` : '';
  if (status === 429 || status === 529 || status === 503 || body?.error?.type === 'overloaded_error') return { ok: false, type: 'rate_limit', message: `API rate limit or overload (${status})`, usage };
  if (status === 401 || status === 403) return { ok: false, type: 'error', message: `API authentication failed (${status})${tail}`, usage };
  return { ok: false, type: 'error', message: `API error (${status})${tail}`, usage };
}
