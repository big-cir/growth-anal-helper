// Common agent call shape and the choice of connection (claude-code CLI, Anthropic API, OpenAI-compatible API).
import { join } from 'node:path';
import type { AgentConfig } from '../workspace.ts';
import { ApiRunner } from './api.ts';
import { ClaudeRunner } from './claude.ts';

export type CallOptions = {
  input: string;
  systemPrompt: string;
  jsonSchema: string;
  budgetUsd: number;
  sessionId?: string | null;
  model?: string | null;
  timeoutMs: number;
  signal?: AbortSignal;
};

export type CallErrorType =
  | 'timeout'
  | 'process'
  | 'rate_limit'
  | 'budget'
  | 'resume'
  | 'isolation'
  | 'cancelled'
  | 'error';

export type CallResult =
  | { ok: true; sessionId: string; structured: unknown; costUsd: number; ms: number }
  | { ok: false; type: CallErrorType; message: string; sessionId: string | null; costUsd: number; ms: number };

export interface AgentRunner {
  call(o: CallOptions): Promise<CallResult>;
}

export function makeRunner(cfg: AgentConfig, outDir: string): AgentRunner {
  const logDir = join(outDir, 'logs');
  if (cfg.provider === 'claude-code') return new ClaudeRunner({ bin: cfg.bin, cwd: join(outDir, '.agent-cwd'), logDir });
  return new ApiRunner({
    provider: cfg.provider, baseUrl: cfg.baseUrl, apiKey: cfg.apiKey, pricing: cfg.pricing,
    maxOutputTokens: cfg.maxOutputTokens, sessionDir: join(outDir, 'agent-sessions'),
  });
}
