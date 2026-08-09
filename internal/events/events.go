// Package events defines the canonical event kinds and a typed helper for
// appending them. The event log (in internal/store) is the source of truth for
// the whole state engine; everything else is a projection rebuilt from it.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/memcode-ai/memcode/internal/store"
)

// Kind enumerates the canonical event kinds.
type Kind string

const (
	// Repository signals.
	KindCommit  Kind = "commit"
	KindDiff    Kind = "diff"
	KindFileAdd Kind = "file_add"
	KindFileDel Kind = "file_del"
	KindTestRun Kind = "test_run"

	// Agent signals.
	KindAgentAction          Kind = "agent_action"
	KindAgentSessionStarted  Kind = "agent_session_started"
	KindAgentSessionFinished Kind = "agent_session_finished"
	KindToolCalled           Kind = "tool_called"
	KindCommandExecuted      Kind = "command_executed"
	KindFileEdited           Kind = "file_edited"
	KindActionDenied         Kind = "action_denied"     // user declined an edit/command (trust signal)
	KindSessionOutcome       Kind = "session_outcome"   // accepted | corrected | rejected (acceptance telemetry)
	KindTodosUpdated         Kind = "todos_updated"     // agent's work-tracker checklist snapshot
	KindContextCompacted     Kind = "context_compacted" // older turns summarized to fit the window/cheap lane

	// Plan mode (deliberate research → proposal → approval loop).
	KindPlanStarted   Kind = "plan_started"
	KindPlanReflected Kind = "plan_reflected" // executive reflection: sufficient? research/ask/synthesize
	KindPlanReviewed  Kind = "plan_reviewed"  // cross-model critic verdict: ok | revise | escalate
	KindPlanProposed  Kind = "plan_proposed"  // a plan was drafted (revision 0)
	KindPlanRevised   Kind = "plan_revised"   // a plan was revised on feedback
	KindPlanApproved  Kind = "plan_approved"  // user approved → hand off to execution
	KindPlanCancelled Kind = "plan_cancelled" // user discarded the plan

	// Interactive input + multimodal.
	KindUserInputReceived  Kind = "user_input_received"
	KindInputSteered       Kind = "input_steered"
	KindInputQueued        Kind = "input_queued"
	KindInputInterrupted   Kind = "input_interrupted"
	KindAttachmentDetected Kind = "attachment_detected"
	KindAttachmentSent     Kind = "attachment_sent_to_model"
	KindAttachmentRejected Kind = "attachment_rejected"

	// Continuity signals — the human's voice. These consolidate into doctrine
	// and decisions so a project's intent survives across sessions.
	KindUserNote         Kind = "user_note"
	KindAssertion        Kind = "assertion"
	KindDecision         Kind = "decision"
	KindFrustration      Kind = "frustration"
	KindPreferenceSignal Kind = "preference_signal" // user stated a durable taste/constraint to remember (LLM-captured, reducer-promoted)
	KindLessonSignal     Kind = "lesson_signal"     // a strategy lesson distilled from a learning episode (failure-repair / human-correction / rejection; reducer-promoted like preferences)

	// The post-session learning loop (see runtime.processOutcomes): which promoted
	// rules rode a session's context, whether the agent followed them, and a marker
	// so each session_outcome is distilled/judged exactly once.
	KindContextInlined   Kind = "context_inlined"   // {session_id, lesson_ids, pref_ids} — the rules the model actually saw
	KindAdherence        Kind = "adherence"         // {session_id, rule_kind, rule_id, verdict, outcome} — judge verdict, feeds reducer weights
	KindOutcomeProcessed Kind = "outcome_processed" // {session_id} — this outcome's learning pass ran (attempted counts; failures are dropped, not retried)

	// Reducer-emitted.
	KindNote      Kind = "note"
	KindDeviation Kind = "deviation"

	// Per-turn efficiency telemetry: how much read-only GATHERING a turn did, the
	// mode budget it ran under, whether the gather→act gate fired, and which targets
	// were re-read. The deterministic substrate for retrospective cost/efficiency
	// analysis (/analyze) — so an expensive turn is diagnosable from data, not vibes.
	KindGatherSummary Kind = "gather_summary"
)

// Append records an event with a JSON-encodable payload and returns its id.
// actor is who/what produced it (e.g. "user", "agent", "reducer", a git author).
func Append(ctx context.Context, s store.Store, kind Kind, actor string, payload any) (int64, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		raw = b
	}
	return s.AppendEvent(ctx, store.Event{
		TS:      time.Now().UTC(),
		Kind:    string(kind),
		Actor:   actor,
		Payload: raw,
	})
}
