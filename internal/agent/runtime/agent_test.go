package runtime

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/llm"
)

// A strong-tier agent keeps every request on the strong tier via the agent_strong
// hint (the same Risk mechanism self-heal / plan-synth use); an ordinary turn
// carries no hint (cheap lane).
func TestStrongTierForceEscalates(t *testing.T) {
	strong := &Session{forceEscalate: true, turn: newTurnState()}
	h := strong.turnRoutingHint()
	if h == nil || h.Reason != "agent_strong" {
		t.Fatalf("strong-tier agent must escalate to the strong tier, got %+v", h)
	}
	if ordinary := (&Session{turn: newTurnState()}).turnRoutingHint(); ordinary != nil {
		t.Errorf("an ordinary turn must carry no escalation hint, got %+v", ordinary)
	}
}

// The agent tool is offered to the executive, but never to a read-only explorer, and never from
// inside a spawned agent (no runaway nesting). explore stays parallel-safe; agent is serial.
func TestAgentToolGating(t *testing.T) {
	s := newTodoSession(t) // executive, normal chat
	if !hasTool(s.toolDefs(), tools.Agent) {
		t.Error("executive chat must offer the agent tool")
	}
	if !isParallelSafe(tools.Explore) {
		t.Error("explore must stay parallel-safe (read-only fan-out)")
	}
	if isParallelSafe(tools.Agent) {
		t.Error("the agent tool must be serial (it can mutate)")
	}

	s.readOnly = true
	if hasTool(s.toolDefs(), tools.Agent) {
		t.Error("read-only explorers must NOT get the agent tool")
	}
	s.readOnly = false

	s.purpose = llm.Agent
	if hasTool(s.toolDefs(), tools.Agent) {
		t.Error("a spawned agent must NOT be able to nest more agents")
	}
}
