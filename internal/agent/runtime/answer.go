package runtime

import (
	"context"

	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Answer runs a read-only investigation to completion and returns the model's
// final text. It is the unit of a parallel explorer (a "reader" sub-agent): it
// orients on a scope, reads/searches under the read-only tool set, and never edits
// or runs MUTATING commands (it may run read-only shell commands like git log via
// the gated inspect shell) — so any number can run concurrently with no
// serialized-writer contention. Output is suppressed (the orchestrator owns
// presentation); every tool call is still recorded as an event.
func (s *Session) Answer(ctx context.Context, scope, question string) (string, error) {
	s.readOnly = true
	s.scope = scope // tag this scout's gather telemetry with its subsystem scope
	if s.sessionID == "" {
		s.setSessionID(newSessionID())
	}
	s.emit(ctx, events.KindAgentSessionStarted, map[string]any{
		"mode": "read-only", "model": s.model, "scope": scope, "explorer": true})

	sys := promptSpec{mode: "scout", facts: s.baseFacts()}.withFact("scope", scope)
	messages := []wire.Message{{
		Role:   "user",
		Blocks: []wire.Block{{Type: "text", Text: question}},
	}}
	if _, _, err := s.runLoop(ctx, sys, &messages); err != nil {
		return "", err
	}
	s.emit(ctx, events.KindAgentSessionFinished, map[string]any{
		"explorer": true, "scope": scope, "tool_calls": s.metrics.toolCalls})
	return s.lastText, nil
}
