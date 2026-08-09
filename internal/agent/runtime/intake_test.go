package runtime

import (
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/plan"
)

// TestDecideIntake is the exhaustive decision table: phase × route × verdict ×
// active × busy, with the clock pinned. Every row is a contract; the priority
// order (plan gate > steer > busy-collision > start > coalesce > queue) is the
// thing being pinned, not just individual outcomes.
func TestDecideIntake(t *testing.T) {
	base := t0
	recent := base.Add(100 * time.Millisecond) // inside queueCoalesceWindow
	later := base.Add(5 * time.Second)         // outside it

	planning := func(v PlanVerdict, submitEpoch, curEpoch int) GateInput {
		return GateInput{Phase: plan.Researching, Verdict: v, PlanEpoch: submitEpoch, CurrentEpoch: curEpoch}
	}

	cases := []struct {
		name       string
		route      input.Route
		g          GateInput
		hasActive  bool
		queueDepth int
		last, now  time.Time
		want       RouteAction
	}{
		// Plain chat (zero gate) — the scheduler's classic rules.
		{"idle plain starts", input.Queue, GateInput{}, false, 0, base, later, ActStartTurn},
		{"idle steer starts (nothing to steer)", input.Steer, GateInput{}, false, 0, base, later, ActStartTurn},
		{"active plain queues", input.Queue, GateInput{}, true, 0, base, later, ActQueue},
		{"active steer steers", input.Steer, GateInput{}, true, 0, base, later, ActSteer},
		{"rapid burst coalesces", input.Queue, GateInput{}, true, 1, base, recent, ActCoalesce},
		{"slow follow-up queues (no coalesce)", input.Queue, GateInput{}, true, 1, base, later, ActQueue},

		// The plan gate: outranks everything except an explicit steer.
		{"planning unclassified awaits verdict", input.Queue, planning(VerdictNone, 0, 3), false, 0, base, later, ActAwaitVerdict},
		{"planning separate defers", input.Queue, planning(VerdictSeparate, 3, 3), false, 0, base, later, ActDeferForPlan},
		{"planning related falls through to start", input.Queue, planning(VerdictRelated, 3, 3), false, 0, base, later, ActStartTurn},
		{"planning related mid-turn queues", input.Queue, planning(VerdictRelated, 3, 3), true, 0, base, later, ActQueue},
		{"planning explicit steer bypasses the gate", input.Steer, planning(VerdictNone, 0, 3), true, 0, base, later, ActSteer},

		// Stale epoch: a verdict from plan A must never act against plan B.
		{"stale separate re-awaits (never mis-defers)", input.Queue, planning(VerdictSeparate, 2, 3), false, 0, base, later, ActAwaitVerdict},
		{"stale related re-awaits (never mis-folds)", input.Queue, planning(VerdictRelated, 2, 3), false, 0, base, later, ActAwaitVerdict},
		// Plan exited while classifying: the gate is out of the way; route normally.
		{"verdict after plan exit routes normally", input.Queue,
			GateInput{Phase: plan.Idle, Verdict: VerdictSeparate, PlanEpoch: 2, CurrentEpoch: 4}, false, 0, base, later, ActStartTurn},
		{"verdict lands during the apply phase routes normally", input.Queue,
			GateInput{Phase: plan.Applying, Verdict: VerdictSeparate, PlanEpoch: 2, CurrentEpoch: 4}, true, 0, base, later, ActQueue},

		// Busy-collision: an async op owns the surface while the scheduler is idle.
		{"idle + async busy declines", input.Queue, GateInput{Busy: OwnerAsync}, false, 0, base, later, ActRejectBusy},
		{"active + async busy still queues (turn machinery works)", input.Queue, GateInput{Busy: OwnerAsync}, true, 0, base, later, ActQueue},
		{"idle + turn busy starts (busy owner is the scheduler's own turn)", input.Queue, GateInput{Busy: OwnerTurn}, false, 0, base, later, ActStartTurn},
	}
	for _, c := range cases {
		if got := decideIntake(c.route, c.g, c.hasActive, c.queueDepth, c.last, c.now); got != c.want {
			t.Errorf("%s: decideIntake = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAcceptGateOutcomesMutateNothing: the three frontend-action decisions
// (await/defer/decline) mint no transaction and touch no queue — the old
// Accept-then-Cancel undo dance must have nothing to undo.
func TestAcceptGateOutcomesMutateNothing(t *testing.T) {
	var s schedState

	d := s.accept("muse", GateInput{Phase: plan.Researching}, t0)
	if d.Kind != DecisionAwaitVerdict || s.active != nil || len(s.queue) != 0 {
		t.Fatalf("await must mutate nothing: %+v active=%v queue=%d", d, s.active, len(s.queue))
	}
	d = s.accept("muse", GateInput{Phase: plan.Researching, Verdict: VerdictSeparate}, t0)
	if d.Kind != DecisionPlanDeferred || s.active != nil || len(s.queue) != 0 {
		t.Fatalf("defer must mutate nothing: %+v", d)
	}
	d = s.accept("hi", GateInput{Busy: OwnerAsync}, t0)
	if d.Kind != DecisionBusyDeclined || s.active != nil || len(s.queue) != 0 {
		t.Fatalf("decline must mutate nothing: %+v", d)
	}
	// A related verdict falls through identically to a plain accept.
	d = s.accept("do it", GateInput{Phase: plan.Researching, Verdict: VerdictRelated}, t0)
	if d.Kind != DecisionStarted || s.active == nil {
		t.Fatalf("related verdict must start: %+v", d)
	}
}

// TestInternalAcceptSkipsFollowupClassification: system-generated accepts (the
// apply instruction, deferred replays) are pre-marked classified so the
// background follow-up classifier never judges them against the active task —
// folding "Begin implementing the approved plan" as a steer broke the apply.
func TestInternalAcceptSkipsFollowupClassification(t *testing.T) {
	var s schedState
	s.accept("the active task", GateInput{}, t0)
	s.accept("Begin implementing the approved plan now, working its steps in order.", GateInput{Internal: true}, at(1000))
	s.accept("a real user follow-up", GateInput{}, at(6000))

	_, _, items := s.pendingClassification()
	if len(items) != 1 || items[0].Text != "a real user follow-up" {
		t.Fatalf("only the user follow-up may reach the classifier, got %+v", items)
	}
}

// TestPlanDrainSinkPolicy pins the one drain policy the three hand-rolled
// loops used to encode separately.
func TestPlanDrainSinkPolicy(t *testing.T) {
	if PlanDrainSink(ExitExecute) != SinkQueueBehind {
		t.Error("execute → queue behind the apply")
	}
	if PlanDrainSink(ExitCancel) != SinkStartNow {
		t.Error("cancel → start now")
	}
	if PlanDrainSink(ExitNewPlan) != SinkCarryForward {
		t.Error("new plan → carry forward")
	}
}
