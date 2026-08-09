package runtime

import (
	"context"
	"io"

	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/wire"
)

// agent.go is the ONE sub-agent engine. Every "spawn a sub-agent" path in memcode routes through
// spawnAgent: the read-only research scouts (explore), the plan-mode research + review sessions,
// and the first-class `agent` tool. Before this they were three bespoke bodies; now they differ
// only by the AgentSpec they pass. The engine is assembled from the existing primitives —
// New(...) + runner.Fork() (shared ledger, auto spend attribution), the readOnly tool gate, the
// iterCap bound, and the forceEscalate routing hook (route.go) that pins a request to Anthropic.

// AgentTier selects the model lane a sub-agent runs on.
type AgentTier int

const (
	TierFast   AgentTier = iota // the cheap scout lane — routine read/summarize work
	TierStrong                  // the strong vendor tier — quality-sensitive / hard / non-code work
)

// AgentSpec is everything that configures a sub-agent. Defaults (zero value) are a fast,
// mutating, MainLoop-ish agent; callers set what they need.
type AgentSpec struct {
	Task     string      // the self-contained instruction the sub-agent runs to completion
	Tier     AgentTier   // Fast (scout/cheap) | Strong (the strong vendor)
	ReadOnly bool        // read-only (no edits/mutating bash) vs a full mutating agent
	Scope    string      // optional subsystem/path tag (telemetry + scout focus)
	IterCap  int         // 0 = mode default
	Purpose  llm.Purpose // ledger attribution; also picks the run shape (Explore → scout prompt)
	Effort   wire.Effort // pin the sub-agent's thinking effort ("" = its own per-turn heuristic)
}

// AgentResult is a sub-agent's report-back: its final text plus the telemetry callers surface
// (the tool-count + served-by on the marker line).
type AgentResult struct {
	Text      string
	ToolCalls int
	ServedBy  string // which model/backend actually ran it (cheap lane vs Anthropic)
}

// spawnAgent runs a sub-agent synchronously and returns its report-back. It is the in-process
// Task primitive: the caller (the LLM via a tool, or plan mode) blocks until the sub-agent
// finishes, then acts on the result.
func (s *Session) spawnAgent(ctx context.Context, spec AgentSpec) (AgentResult, error) {
	model := s.scoutModel
	if model == "" {
		model = s.model
	}
	switch {
	case spec.Tier == TierStrong:
		// Strong runs on the strong tier via the agent_strong risk hint; the model
		// id is irrelevant (the ladder resolves the tier from the hint), so keep
		// the session model.
		model = s.model
	case spec.Purpose == llm.Explore && s.planCtl.Planning() && s.planCtl.ResearchModel() != "":
		model = s.planCtl.ResearchModel() // plan-mode research override (fast lane)
	}

	sub := New(s.store, s.runner.Fork(), s.root, model, s.effectiveMode(), io.Discard)
	if spec.Purpose != "" {
		sub.purpose = spec.Purpose
	} else {
		sub.purpose = llm.Agent
	}
	sub.forceEscalate = spec.Tier == TierStrong
	if spec.IterCap > 0 {
		sub.iterCap = spec.IterCap
	}
	if spec.Effort != "" { // a reasoning delegate pins its thinking depth for the whole run
		sub.effortOverride, sub.hasEffortOverride = spec.Effort, true
	}
	// Share the parent's HITL channel so a MUTATING sub-agent's approvals surface in the same
	// TUI cards (the parent turn is blocked here, so there's no contention). Read-only agents
	// never prompt, so this is harmless for them.
	sub.approve, sub.ask = s.approve, s.ask

	// Explore is the read-only RESEARCH shape: scout prompt, evidence-style answer. Answer sets
	// readOnly itself. Everything else is a general task agent on the normal exec prompt, read-only
	// or mutating per the spec, so it can GENERATE/REASON (strong tier) or actually do the work.
	if spec.Purpose == llm.Explore {
		ans, err := sub.Answer(ctx, spec.Scope, spec.Task)
		if err != nil {
			return AgentResult{}, err
		}
		return AgentResult{Text: ans, ToolCalls: sub.metrics.toolCalls, ServedBy: sub.ServedBy()}, nil
	}
	sub.readOnly = spec.ReadOnly
	sub.scope = spec.Scope
	if _, err := sub.Run(ctx, spec.Task); err != nil {
		return AgentResult{}, err
	}
	return AgentResult{Text: sub.lastText, ToolCalls: sub.metrics.toolCalls, ServedBy: sub.ServedBy()}, nil
}
