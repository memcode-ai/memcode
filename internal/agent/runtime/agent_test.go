package runtime

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/llm"
)

// TestStrongTierForceEscalates is DELETED. It asserted that a "strong tier"
// agent escalated every request via the agent_strong hint. Delegated workers
// are user-work inference now and run on the session's pinned model; there is
// no tier for an agent to be pinned to, and no hint mechanism left.

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
