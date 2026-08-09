package runtime

// intake.go — the ONE deterministic decision core for "what happens to an incoming
// message". Before this existed the decision was re-implemented at seven call sites
// across two parallel engines (the TUI's scheduler vs headless chat's own queue), the
// plan-intake gate was spread over five files with three drain implementations, and the
// busy-collision guard lived in exactly one of the paths that needed it. Every frontend
// now feeds the same pure function; only the EFFECT executors differ (the scheduler
// actor for the TUI, the synchronous queue for headless).
//
// There is NO content heuristic here on purpose (the scheduler's founding rule): a plain
// mid-turn line is never guessed at on the hot path. Judgment calls — does this message
// continue the plan, does a follow-up refine the active task — are made by the cheap
// structured-output classifiers, and their verdicts arrive HERE as data, keyed to the
// plan epoch they were computed against. A verdict whose epoch no longer matches is
// void (the plan it judged is gone) — the same state-version rule as the scheduler's
// expectActive fold guard, generalized.
//
// input.Route stays the LEXICAL layer feeding in ($ shell and slash commands never reach
// this function; both frontends divert them earlier). Explicit `+steer` bypasses the
// plan gate: the user is unambiguously steering the live work, not musing.

import (
	"time"

	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/plan"
)

// PlanVerdict is the plan-relevance classifier's answer as data.
type PlanVerdict int

const (
	VerdictNone     PlanVerdict = iota // not classified (yet)
	VerdictRelated                     // continues/steers the plan being drafted
	VerdictSeparate                    // a separate ask — park it until the plan is done
)

// BusyOwner says who owns the frontend's busy state. OwnerTurn is a scheduler
// transaction (queueing behind it is normal); OwnerAsync is a non-transaction
// operation (advisor, /compact) that a new turn must not trample.
type BusyOwner int

const (
	OwnerNone BusyOwner = iota
	OwnerTurn
	OwnerAsync
)

// GateInput is the non-scheduler context a frontend supplies with each accept.
// The zero value is exactly today's plain-chat behavior: chatting, no verdict,
// not busy.
type GateInput struct {
	Phase        plan.Phase  // the plan machine's phase at submit/finalize time
	PlanEpoch    int         // epoch the verdict was computed against (0 = n/a)
	CurrentEpoch int         // the machine's epoch NOW (finalize time)
	Verdict      PlanVerdict // the relevance verdict, when one exists
	Busy         BusyOwner   // who owns the frontend's busy state
	// Internal marks a system-generated accept (the plan task itself, the apply
	// instruction, a revise, an already-classified deferred replay): it bypasses the
	// plan gate by construction (Phase zero) AND pre-marks a queued tx classified, so
	// the background follow-up classifier never judges system text against the active
	// task (folding "Begin implementing the approved plan" as a steer broke the apply).
	Internal bool
}

// RouteAction is the single vocabulary for what happens to an incoming message.
type RouteAction int

const (
	ActStartTurn    RouteAction = iota // idle → run it now
	ActSteer                           // fold into the active transaction
	ActQueue                           // run after the active transaction
	ActCoalesce                        // merge into the last queued item (rapid burst)
	ActAwaitVerdict                    // planning, unclassified → classify first, re-enter with the verdict
	ActDeferForPlan                    // classified separate → park until the plan exits
	ActRejectBusy                      // an async op owns busy and nothing is active — decline politely
)

// decideIntake is the pure routing rule. Priority order matters and is the contract:
// the plan gate outranks everything except an explicit steer; busy-collision outranks
// starting a turn; coalescing is only ever a queue refinement.
func decideIntake(route input.Route, g GateInput, hasActive bool, queueDepth int, last, now time.Time) RouteAction {
	if g.Phase.Planning() && route != input.Steer {
		switch {
		case g.Verdict == VerdictNone:
			return ActAwaitVerdict
		case g.PlanEpoch != g.CurrentEpoch:
			// The verdict was computed against a plan session that no longer exists
			// (cancelled + a NEW plan started while classifying). Void it and judge
			// against the current plan — never apply plan A's verdict to plan B.
			return ActAwaitVerdict
		case g.Verdict == VerdictSeparate:
			return ActDeferForPlan
		}
		// VerdictRelated falls through to normal routing.
	}
	if route == input.Steer && hasActive {
		return ActSteer
	}
	if !hasActive {
		if g.Busy == OwnerAsync {
			return ActRejectBusy // scheduler idle but an async op owns the surface
		}
		return ActStartTurn
	}
	if queueDepth > 0 && now.Sub(last) < queueCoalesceWindow {
		return ActCoalesce
	}
	return ActQueue
}

// PlanExit names how a plan session ended, for the deferred-message drain policy.
type PlanExit int

const (
	ExitExecute PlanExit = iota // approved: parked messages run AFTER the apply turn
	ExitCancel                  // cancelled: nothing ahead — parked messages start now
	ExitNewPlan                 // a new plan is starting: carry leftovers into it
)

// DrainSink is where drained deferred messages go.
type DrainSink int

const (
	SinkQueueBehind  DrainSink = iota // queue behind what was just accepted (the apply turn)
	SinkStartNow                      // route normally — nothing is ahead
	SinkCarryForward                  // re-park against the new plan session
)

// PlanDrainSink is the ONE policy for replaying parked messages at a plan exit —
// the three hand-rolled drain loops used to each encode a different slice of it.
func PlanDrainSink(exit PlanExit) DrainSink {
	switch exit {
	case ExitCancel:
		return SinkStartNow
	case ExitNewPlan:
		return SinkCarryForward
	default:
		return SinkQueueBehind
	}
}
