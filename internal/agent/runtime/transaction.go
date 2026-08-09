package runtime

import "time"

// A Transaction is memcode's first-class unit of work: one user intent, run start to
// finish by the executor (exactly one active at a time). It has identity, an explicit
// lifecycle state, in-flight steer events, and a result — so queueing, steering,
// cancellation, and (later) undo all hang off a single coherent object instead of being
// ad-hoc behaviors layered around a "turn". See COMPACTION.md's sibling design notes and
// the scheduler (schedstate.go / scheduler.go).

// TxState is the explicit lifecycle of a Transaction. Transitions are owned by the
// scheduler state core (schedState) and serialized by the actor; nothing mutates a
// transaction's state from two goroutines.
type TxState string

const (
	TxQueued           TxState = "queued"            // accepted while another was active; inert (no ChatState yet)
	TxRunning          TxState = "running"           // the active transaction; the only writer of ChatState
	TxAwaitingApproval TxState = "awaiting_approval" // paused on an approval/ask card mid-run
	TxCancelling       TxState = "cancelling"        // Esc/Ctrl-C requested; unwinding
	TxCompleted        TxState = "completed"         // finished normally (terminal)
	TxFailed           TxState = "failed"            // errored (terminal)
	TxCancelled        TxState = "cancelled"         // aborted before completion (terminal)
)

// terminal reports whether a state is post-execution (no further work).
func (s TxState) terminal() bool {
	switch s {
	case TxCompleted, TxFailed, TxCancelled:
		return true
	}
	return false
}

// SteerEvent is a `+input` the user submitted while a transaction was active: recorded
// immediately, then folded into the SAME transaction at the next safe boundary in
// runLoop (after tool_results, before the next model call). It cannot affect an
// in-flight model call — that is what cancel is for.
type SteerEvent struct {
	Text string
	At   time.Time
}

// TransactionResult is a transaction's outcome. v1 records only the terminal state and
// any error; the commented fields are the deliberate seam for Phase-3 undo (revert a
// completed transaction's edits via git/patch) — shaped now so it attaches cleanly later.
type TransactionResult struct {
	State TxState
	Err   string
	// FilesChanged []string // Phase 3 (undo)
	// Patch        string   // Phase 3 (undo)
}

// Transaction is the unit the scheduler manages. steers is unexported because it is
// mutated only through schedState (actor-serialized); everything else is read-only once
// the transaction is created, except the lifecycle fields the scheduler advances.
type Transaction struct {
	ID        string // "tx_<n>" — unique within a session, deterministic for tests
	Text      string // the user intent (paste-expanded, routing marker stripped)
	State     TxState
	CreatedAt time.Time
	StartedAt time.Time
	EndedAt   time.Time
	steers    []SteerEvent
	Result    TransactionResult

	// classified is set once the executor's safe-boundary follow-up classifier has looked
	// at this QUEUED transaction (related → folded into the active turn as a steer;
	// unrelated → left queued). It stops the boundary from re-classifying (and re-spending
	// an LLM call on) the same queued item every tool iteration. Mutated only via schedState.
	classified bool
}
