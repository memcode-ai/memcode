package personal

import (
	"fmt"
	"sort"
	"time"
)

type ExecutiveState struct {
	Objective           Objective
	Subgoals            []Subgoal
	PendingInteractions int
	RecentActions       []Action
	LastEvaluation      *EffectivenessEvaluation
}
type ExecutiveDecision struct {
	Kind, SubgoalID, Reason string
	NextWake                *time.Time
}
type EffectivenessEvaluation struct {
	Progress                               float64
	Success                                bool
	Elapsed                                time.Duration
	Cost                                   float64
	RepeatedSteps, Errors, UserCorrections int
	EnvironmentalInstability               bool
	CapabilityGap, Recommendation          string
}

func SelectNextAction(state ExecutiveState, now time.Time) ExecutiveDecision {
	if state.Objective.Status == "paused" || state.Objective.Status == "stopped" {
		return ExecutiveDecision{Kind: "stop", Reason: "objective is not active"}
	}
	if state.PendingInteractions > 0 {
		return ExecutiveDecision{Kind: "ask", Reason: "human interaction is pending"}
	}
	eligible := append([]Subgoal(nil), state.Subgoals...)
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].Priority > eligible[j].Priority })
	for _, g := range eligible {
		if g.Status == "pending" || g.Status == "active" {
			return ExecutiveDecision{Kind: "execute", SubgoalID: g.ID, Reason: "highest-priority eligible subgoal"}
		}
	}
	next := now.Add(time.Hour)
	return ExecutiveDecision{Kind: "defer", Reason: "no eligible subgoal", NextWake: &next}
}
func EvaluateEffectiveness(e EffectivenessEvaluation) ExecutiveDecision {
	if e.Success && e.Progress >= 1 {
		return ExecutiveDecision{Kind: "complete", Reason: "success criteria satisfied"}
	}
	if e.Errors >= 3 || e.EnvironmentalInstability {
		return ExecutiveDecision{Kind: "change_strategy", Reason: "repeated failure or unstable environment"}
	}
	if e.RepeatedSteps >= 2 || e.CapabilityGap != "" {
		return ExecutiveDecision{Kind: "generate_artifact", Reason: "observed friction or capability gap"}
	}
	return ExecutiveDecision{Kind: "continue", Reason: "current strategy remains effective"}
}
func ValidateExecutiveBudget(maxSeconds, maxTools, maxDelegation int) error {
	if maxSeconds <= 0 || maxTools <= 0 || maxDelegation < 0 {
		return fmt.Errorf("executive wakes require positive time/tool budgets and non-negative delegation depth")
	}
	return nil
}
