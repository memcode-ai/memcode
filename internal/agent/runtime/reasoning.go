package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/wire"
)

// The adaptive-reasoning tool: the model matches thinking depth to task
// difficulty ITSELF instead of riding one per-turn heuristic. Self-adjust works
// mid-turn because runLoop reads turnEffort on every iteration — a change
// applies from the very next model call and dies with the turn (scoreTurn
// recomputes at the next turn's start, so reset is structural, not a timer).
// Delegation reuses the one sub-agent engine: a read-only, strong-tier,
// effort-pinned spawnAgent whose answer returns as the tool result.

// reasoningTool handles both shapes of tools.Reasoning. A user /effort setting
// is the session DEFAULT, not a cage: the model may adjust mid-turn, and the
// default reapplies at the next turn's scoring anyway (structural revert).
func (s *Session) reasoningTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.ReasoningInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return errResult(err.Error())
		}
	}
	eff := strings.ToLower(strings.TrimSpace(in.Effort))

	// DELEGATE: a hard sub-problem for the strong reasoning model.
	if task := strings.TrimSpace(in.Task); task != "" {
		level := wire.EffortHigh // delegation exists for the hard cases; default deep
		switch eff {
		case "", "high":
		case "medium":
			level = wire.EffortMedium
		case "off", "auto":
			return errResult("a reasoning delegate always thinks — use effort medium or high (or omit it), or drop `task` to adjust your own depth instead.")
		default:
			return errResult("effort must be off, medium, high, or auto")
		}
		full := "Reason through this hard sub-problem and return your conclusion, with the decisive why. Read the repository only as needed to ground your answer; be direct and take a position.\n\nProblem:\n" + task
		if c := strings.TrimSpace(in.Context); c != "" {
			full += "\n\nContext:\n" + c
		}
		res, err := s.spawnAgent(ctx, AgentSpec{
			Task: full, Tier: TierStrong, ReadOnly: true, Purpose: llm.Agent, Effort: level,
		})
		if err != nil {
			return errResult("reasoning delegate failed: " + err.Error())
		}
		s.toolLine(true, "Reasoning", shortTask(task), fmt.Sprintf("%d tools · %s", res.ToolCalls, res.ServedBy), false)
		if strings.TrimSpace(res.Text) == "" {
			return errResult("the reasoning delegate returned nothing — treat the question as still open.")
		}
		return textResult(res.Text)
	}

	// SELF: report or adjust this session's own turn effort. The user's /effort
	// choice is the DEFAULT that returns at the next turn; within a turn the
	// model owns the dial.
	current := s.ThinkingEffort()
	if current == "" {
		current = "off"
	}
	sessionDefault := "auto"
	if s.hasEffortOverride {
		sessionDefault = s.EffortOverride()
	}
	if eff == "" {
		return textResult(fmt.Sprintf("thinking effort is %s (session default: %s — it returns when this turn ends). Adjust with effort: off | medium | high | auto, or delegate a hard sub-problem with task.", current, sessionDefault))
	}
	var level wire.Effort
	switch eff {
	case "off":
		level = wire.EffortOff
	case "medium":
		level = wire.EffortMedium
	case "high":
		level = wire.EffortHigh
	case "auto":
		if s.hasEffortOverride {
			level = s.effortOverride // auto = back to the session default, which the user set
		} else {
			level = s.turnBaseEffort // the turn_intent judge's baseline for THIS turn — never re-judge mid-turn
		}
	default:
		return errResult("effort must be off, medium, high, or auto")
	}
	s.setTurnEffort(level)
	shown := string(level)
	if shown == "" {
		shown = "off"
	}
	s.toolLine(true, "Reasoning", shown, "for the rest of this turn", false)
	return textResult(fmt.Sprintf("thinking effort → %s, effective from your next step. The session default (%s) returns when this turn ends.", shown, sessionDefault))
}

// shortTask trims a delegate task to a marker-line label.
func shortTask(t string) string {
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[:i]
	}
	if len(t) > 60 {
		t = t[:60] + "…"
	}
	return t
}
