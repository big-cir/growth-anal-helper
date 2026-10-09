package agent

import (
	"context"
	"path/filepath"

	"growth-lab/internal/workspace"
)

// CallOptions is one agent call.
type CallOptions struct {
	Input        string
	SystemPrompt string
	JSONSchema   string
	BudgetUsd    float64
	SessionID    string // "" for a new session
	Model        string // "" for the default
	TimeoutMs    int
	Ctx          context.Context
}

// Usage are the token counts of one call; Input excludes cached tokens.
type Usage struct{ Input, Output, CacheRead, CacheWrite float64 }

// CallResult is a call outcome. Type is set on failure: timeout, process, rate_limit, budget, resume, isolation, cancelled, error.
// Usage and APIMs are present when the provider reports them.
type CallResult struct {
	OK         bool
	Type       string
	Message    string
	SessionID  string // "" when none
	Structured any
	CostUsd    float64
	Ms         float64
	Usage      *Usage
	APIMs      *float64
}

// Runner calls the agent.
type Runner interface {
	Call(o CallOptions) CallResult
}

// MakeRunner picks the connection: the Claude Code CLI or an API.
func MakeRunner(cfg workspace.Agent, outDir string) (Runner, error) {
	logDir := filepath.Join(outDir, "logs")
	if cfg.Provider == "claude-code" {
		r, err := NewClaudeRunner(cfg.Bin, filepath.Join(outDir, ".agent-cwd"), logDir, nil)
		if r != nil {
			r.TotalsDir = filepath.Join(outDir, "agent-sessions")
		}
		return r, err
	}
	var base string
	if cfg.BaseURL != nil {
		base = *cfg.BaseURL
	}
	var pricing *Pricing
	if cfg.Pricing != nil {
		pricing = &Pricing{InputPerMTok: cfg.Pricing.InputPerMTok, OutputPerMTok: cfg.Pricing.OutputPerMTok}
	}
	return NewAPIRunner(APIOptions{Provider: cfg.Provider, BaseURL: base, APIKey: cfg.APIKey, Pricing: pricing, MaxOutputTokens: cfg.MaxOutputTokens, SessionDir: filepath.Join(outDir, "agent-sessions")})
}
