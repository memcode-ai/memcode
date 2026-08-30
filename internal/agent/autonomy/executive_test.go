package autonomy

import (
	"testing"
	"time"
)

func TestExecutiveSelectionEvaluationAndScheduling(t *testing.T) {
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	state := ExecutiveState{Objective: Objective{Status: "active"}, Subgoals: []Subgoal{{ID: "low", Status: "pending", Priority: 1}, {ID: "high", Status: "pending", Priority: 5}}}
	d := SelectNextAction(state, now)
	if d.Kind != "execute" || d.SubgoalID != "high" {
		t.Fatalf("decision=%+v", d)
	}
	state.PendingInteractions = 1
	if d = SelectNextAction(state, now); d.Kind != "ask" {
		t.Fatalf("decision=%+v", d)
	}
	state.PendingInteractions = 0
	state.Subgoals = nil
	if d = SelectNextAction(state, now); d.Kind != "defer" || d.NextWake == nil {
		t.Fatalf("decision=%+v", d)
	}
	if d = EvaluateEffectiveness(EffectivenessEvaluation{RepeatedSteps: 3}); d.Kind != "generate_artifact" {
		t.Fatalf("decision=%+v", d)
	}
	if d = EvaluateEffectiveness(EffectivenessEvaluation{Errors: 3}); d.Kind != "change_strategy" {
		t.Fatalf("decision=%+v", d)
	}
	if d = EvaluateEffectiveness(EffectivenessEvaluation{Success: true, Progress: 1}); d.Kind != "complete" {
		t.Fatalf("decision=%+v", d)
	}
}
func TestSelfEvolutionChoicesFollowObservedFriction(t *testing.T) {
	if got := ChooseEvolution(EffectivenessEvaluation{RepeatedSteps: 2}, false); got != EvolutionGenerate {
		t.Fatalf("choice=%s", got)
	}
	if got := ChooseEvolution(EffectivenessEvaluation{CapabilityGap: "missing transform"}, true); got != EvolutionImprove {
		t.Fatalf("choice=%s", got)
	}
	if got := ChooseEvolution(EffectivenessEvaluation{Errors: 3}, false); got != EvolutionChangeStrategy {
		t.Fatalf("choice=%s", got)
	}
	if got := ChooseEvolution(EffectivenessEvaluation{UserCorrections: 2}, false); got != EvolutionEscalate {
		t.Fatalf("choice=%s", got)
	}
}

func TestExecutiveBudgetBounded(t *testing.T) {
	if err := ValidateExecutiveBudget(60, 10, 2); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExecutiveBudget(0, 10, 2); err == nil {
		t.Fatal("unbounded wake accepted")
	}
}
