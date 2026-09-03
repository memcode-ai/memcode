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
// iterCap bound.
//
// AgentTier (Fast | Strong) is DELETED. It selected which model LANE a sub-agent
// ran on, via a routing hint that escalated it to a stronger tier. A delegated
// worker is user-work inference: it runs on the session's pinned model, the same
// one the user is paying for and watching in the footer.

// AgentSpec is everything that configures a sub-agent. Defaults (zero value) are a
// mutating, MainLoop-ish agent; callers set what they need.
type AgentSpec struct {
	Task     string      // the self-contained instruction the sub-agent runs to completion
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
	ServedBy  string // which model actually ran it
}

// spawnAgent runs a sub-agent synchronously and returns its report-back. It is the in-process
// Task primitive: the caller (the LLM via a tool, or plan mode) blocks until the sub-agent
// finishes, then acts on the result.
func (s *Session) spawnAgent(ctx context.Context, spec AgentSpec) (AgentResult, error) {
	// Every delegated worker — agent-tool tasks, explore scouts, plan-mode
	// research — runs on the DELEGATED pin. Unset means inherit the primary, so
	// the default is "the model you chose runs everything" and a split only
	// happens because someone asked for one.
	//
	// This is a pin, not a decision: nothing here inspects the task to pick a
	// model, and the agent tool has no model parameter to override it with.
	model, runner := s.model, s.runner.Fork()
	if s.delegatedPin != "" {
		model, runner = s.delegatedPin, s.runner.ForkWithModel(s.delegatedPin)
	}

	sub := New(s.store, runner, s.root, model, s.effectiveMode(), io.Discard)
	sub.delegatedPin, sub.delegatedWindow = s.delegatedPin, s.delegatedWindow // a worker's own workers too
	if spec.Purpose != "" {
		sub.purpose = spec.Purpose
	} else {
		sub.purpose = llm.Agent
	}
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
