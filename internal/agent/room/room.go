// Package room reads the *room* — the current state of the human↔agent
// interaction — and turns it into a runtime policy. It is the integration layer
// the friction/cadence signals feed into: not "is the user angry", but "what is
// the interaction state, and how should the agent behave right now".
//
// This is a reducer, in the literal memcode sense: it folds the event log (plus
// the live friction reading) into a RoomState. Normal agents flatten tone,
// cadence, interrupts, denials, loops and acceptance into plain text and miss
// when the interaction has changed. memcode treats them as first-class signals.
//
// The output is a policy, because reading the room only matters if behavior
// changes: under repair the agent executes the user's correction immediately and
// narrowly (frustration demands ACTION — the 2026-07-18 eggshells incident proved
// that stall-and-confirm makes an angry user angrier; the prose lives server-side
// in roomGuidance); when the user is exploring it reasons openly instead of
// editing; when urgent it gets terse and acts. The Policy fields are ADVISORY —
// only MemoryWeight is consumed today; the room never tightens the permission gate.
package room

import (
	"context"
	"encoding/json"

	"github.com/memcode-ai/memcode/internal/agent/mood"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/store"
)

// Intent is what the user is trying to do right now.
type Intent string

const (
	Working       Intent = "working"       // neutral, making progress
	Exploring     Intent = "exploring"     // curious / idea generation
	Understanding Intent = "understanding" // confused / wants explanation
	Correcting    Intent = "correcting"    // fixing the agent's course
	Executing     Intent = "executing"     // urgent, wants action not talk
)

// Trust is the direction the user's confidence is moving.
type Trust string

const (
	Steady   Trust = "steady"
	Dropping Trust = "dropping"
	Low      Trust = "low"
)

// Loop is the risk that the AGENT (not the user) is stuck going in circles.
type Loop string

const (
	NoLoop Loop = "none"
	Rising Loop = "rising"
	Stuck  Loop = "stuck"
)

// Outcome is the fate of the last agent session (acceptance telemetry).
type Outcome string

const (
	OutcomeUnknown   Outcome = "unknown"
	OutcomeAccepted  Outcome = "accepted"  // committed
	OutcomeCorrected Outcome = "corrected" // manually edited after
	OutcomeRejected  Outcome = "rejected"  // reverted / reset
)

// Mode is the agent's operating mode derived from the room.
type Mode string

const (
	Normal  Mode = "normal"
	Repair  Mode = "repair"  // things are going badly — stop and fix
	Explain Mode = "explain" // user is confused — clarify, don't pile on
	Explore Mode = "explore" // user is curious — reason openly
	Execute Mode = "execute" // user is urgent — act, be terse
	Replan  Mode = "replan"  // the agent is looping — re-plan from context
)

// Policy is the concrete behavior the runtime should adopt.
type Policy struct {
	AllowAutoWrite bool   `json:"allow_auto_write"` // false ⇒ confirm every write
	ShowStatus     bool   `json:"show_status"`      // surface diff/status before continuing
	AskBeforeNext  bool   `json:"ask_before_next"`  // pause for confirmation between steps
	SummarizeLast  bool   `json:"summarize_last"`   // recap the last action
	Terse          bool   `json:"terse"`            // minimize narration
	MemoryWeight   string `json:"memory_weight"`    // normal | strong (for directions given now)
}

// State is the assessed room.
type State struct {
	Friction  string  `json:"friction"`   // low | elevated | high (from mood)
	MoodState string  `json:"mood_state"` // the mood.State
	Intent    Intent  `json:"intent"`
	Urgency   string  `json:"urgency"` // low | medium | high
	Trust     Trust   `json:"trust"`
	LoopRisk  Loop    `json:"loop_risk"`
	Outcome   Outcome `json:"last_outcome"`
	Mode      Mode    `json:"mode"`
	Policy    Policy  `json:"policy"`
}

// Signals are the event-derived inputs to the reducer (separate from the live
// friction reading so Assess stays a pure, testable function).
type Signals struct {
	Interrupts         int
	Denials            int
	RepeatedCorrection bool
	LoopRisk           Loop
	LastOutcome        Outcome
}

// Assess folds the live friction reading and the event-derived signals into a
// RoomState + Policy. Pure and deterministic.
func Assess(cur mood.Reading, sig Signals) State {
	friction := mood.Friction(cur.State)
	s := State{
		Friction:  friction,
		MoodState: string(cur.State),
		Intent:    intentFor(cur, sig),
		Urgency:   urgencyFor(cur),
		Trust:     trustFor(friction, sig),
		LoopRisk:  sig.LoopRisk,
		Outcome:   sig.LastOutcome,
	}
	s.Mode = modeFor(s, cur)
	s.Policy = policyFor(s.Mode)
	return s
}

func intentFor(cur mood.Reading, sig Signals) Intent {
	switch {
	case cur.State == mood.Frustrated || cur.State == mood.Angry || sig.Interrupts > 0 || sig.RepeatedCorrection:
		return Correcting
	case cur.State == mood.Urgent:
		return Executing
	case cur.State == mood.Confused:
		return Understanding
	case cur.State == mood.Curious:
		return Exploring
	default:
		return Working
	}
}

func urgencyFor(cur mood.Reading) string {
	switch {
	case cur.State == mood.Urgent:
		return "high"
	case cur.Intensity >= 0.6:
		return "medium"
	default:
		return "low"
	}
}

func trustFor(friction string, sig Signals) Trust {
	switch {
	case sig.Denials >= 2 || sig.LastOutcome == OutcomeRejected:
		return Low
	case sig.Denials == 1 || sig.RepeatedCorrection || sig.Interrupts >= 2 || friction == "high":
		return Dropping
	default:
		return Steady
	}
}

func modeFor(s State, cur mood.Reading) Mode {
	switch {
	case cur.State == mood.Angry || cur.State == mood.Frustrated || s.Trust == Low || s.Outcome == OutcomeRejected:
		return Repair
	case s.LoopRisk == Stuck:
		return Replan
	case cur.State == mood.Confused:
		return Explain
	case cur.State == mood.Curious:
		return Explore
	case cur.State == mood.Urgent:
		return Execute
	default:
		return Normal
	}
}

func policyFor(m Mode) Policy {
	switch m {
	case Repair:
		return Policy{AllowAutoWrite: false, ShowStatus: true, AskBeforeNext: true, SummarizeLast: true, MemoryWeight: "strong"}
	case Replan:
		return Policy{AllowAutoWrite: false, ShowStatus: true, SummarizeLast: true, MemoryWeight: "normal"}
	case Explain:
		return Policy{AllowAutoWrite: false, ShowStatus: true, SummarizeLast: true, MemoryWeight: "normal"}
	case Explore:
		return Policy{AllowAutoWrite: false, MemoryWeight: "normal"}
	case Execute:
		return Policy{AllowAutoWrite: true, Terse: true, MemoryWeight: "normal"}
	default:
		return Policy{AllowAutoWrite: true, MemoryWeight: "normal"}
	}
}

// NOTE: the room's natural-language strategy prose is DOCTRINE-OWNED — it lives
// in the composer's roomGuidance map (internal/doctrine/prompts.go). This
// package computes and sends only the room Mode fact; it must not also carry
// the prose. (A duplicate Guidance() lived here and was dead code + a drift
// trap: two copies of the same strings silently diverging.)

// turnWindowStart returns the index of the first event belonging to the last n user turns
// (the n-th KindUserInputReceived counting back from the end). Returns 0 when there are ≤ n
// user turns so far, i.e. keep everything. Used to keep the room reading recency-focused.
func turnWindowStart(evs []store.Event, n int) int {
	seen := 0
	for i := len(evs) - 1; i >= 0; i-- {
		if events.Kind(evs[i].Kind) == events.KindUserInputReceived {
			seen++
			if seen == n {
				return i
			}
		}
	}
	return 0
}

// Gather reduces the recent event log into Signals (interrupts, denials, loop
// risk, last session outcome).
func Gather(ctx context.Context, st store.Store, sessionID string) Signals {
	calm := Signals{LoopRisk: NoLoop, LastOutcome: OutcomeUnknown}
	// Bounded to the most-recent N of these event kinds (ListEvents returns newest-N in
	// chronological order): the room only needs the current session's last few turns, so
	// scanning the entire, ever-growing events table each turn was pure waste. 500 of these
	// (sparse) kinds comfortably spans the active session's recent turns.
	evs, err := st.ListEvents(ctx, store.EventFilter{Limit: 500, Kinds: []string{
		string(events.KindInputInterrupted),
		string(events.KindActionDenied),
		string(events.KindCommandExecuted),
		string(events.KindTestRun),
		string(events.KindSessionOutcome),
		string(events.KindUserInputReceived),
	}})
	if err != nil || len(evs) == 0 {
		return calm
	}
	// The room is about NOW — THIS session only. The store is persistent, so without
	// this filter a fresh, calm session would inherit a PRIOR session's friction/loops
	// (repeated commands, failed tests) and open in repair/replan. Each event carries
	// its session_id in the payload (Session.emit); keep only the current session's.
	if sessionID != "" {
		kept := evs[:0]
		for _, e := range evs {
			if field(e.Payload, "session_id") == sessionID {
				kept = append(kept, e)
			}
		}
		evs = kept
	}
	if len(evs) == 0 {
		return calm
	}
	// Window by the most RECENT user turns, not a flat event tail. The room tracks the
	// user's CURRENT state, not a long tail of past grievances: a flat 40-event window
	// reached back into earlier rough turns (interrupts, denials), so a benign "hello"
	// after a hard debugging stretch still read as Correcting and escalated. Keep only the
	// events from the last `recentTurns` user inputs onward, so a couple of calm turns
	// lets the room recover. (Fewer turns so far → keep all.)
	const recentTurns = 3
	if start := turnWindowStart(evs, recentTurns); start > 0 {
		evs = evs[start:]
	}

	sig := Signals{LoopRisk: NoLoop, LastOutcome: OutcomeUnknown}
	cmdCount := map[string]int{}
	failedTests := 0
	for _, e := range evs {
		switch events.Kind(e.Kind) {
		case events.KindInputInterrupted:
			sig.Interrupts++
		case events.KindActionDenied:
			sig.Denials++
		case events.KindUserInputReceived:
			if hasSignal(e.Payload, "repeated-correction") {
				sig.RepeatedCorrection = true
			}
		case events.KindCommandExecuted:
			if c := field(e.Payload, "command"); c != "" {
				cmdCount[c]++
			}
		case events.KindTestRun:
			if exitNonZero(e.Payload) {
				failedTests++
			}
		case events.KindSessionOutcome:
			sig.LastOutcome = Outcome(field(e.Payload, "outcome"))
		}
	}

	maxRepeat := 0
	for _, n := range cmdCount {
		if n > maxRepeat {
			maxRepeat = n
		}
	}
	switch {
	case maxRepeat >= 4 || failedTests >= 3:
		sig.LoopRisk = Stuck
	case maxRepeat >= 2 || failedTests >= 2:
		sig.LoopRisk = Rising
	}
	return sig
}

// --- payload helpers ---

func field(p json.RawMessage, key string) string {
	if len(p) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(p, &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func exitNonZero(p json.RawMessage) bool {
	var m map[string]any
	if len(p) == 0 || json.Unmarshal(p, &m) != nil {
		return false
	}
	if v, ok := m["exit"].(float64); ok {
		return v != 0
	}
	return false
}

func hasSignal(p json.RawMessage, want string) bool {
	var m struct {
		Mood struct {
			Signals []string `json:"signals"`
		} `json:"mood"`
	}
	if len(p) == 0 || json.Unmarshal(p, &m) != nil {
		return false
	}
	for _, s := range m.Mood.Signals {
		if s == want {
			return true
		}
	}
	return false
}
