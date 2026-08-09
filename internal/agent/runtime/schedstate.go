package runtime

import (
	"fmt"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/input"
)

// schedState is the synchronous, deterministic CORE of the transaction scheduler: it
// owns the active transaction and the FIFO queue, and implements the whole routing +
// lifecycle policy. It has NO goroutine and NO locks — the actor (scheduler.go)
// serializes every call. The clock is injected (callers pass `now`) so the behavior is
// fully deterministic and unit-testable without timers.
//
// THE INVARIANT: nothing in here touches ChatState.messages. A queued transaction holds
// only its Text; it becomes part of the conversation only when the executor promotes it
// and runs it. Queued input is inert until promoted.
type schedState struct {
	active *Transaction
	queue  []*Transaction
	last   time.Time // time of the last accept — for rapid-paste coalescing
	seq    int       // transaction id counter (deterministic)

	// separateNotes buffers items the background follow-up classifier judged SEPARATE
	// (not a refinement of the active task) — mirrors the intent of the steer buffer, but
	// these are never folded into the active transaction: they stay queued to run as their
	// own turn later, and are only surfaced here so the runLoop safe boundary can track them
	// on the todo list and give the model an FYI note. FIFO, drained by drainSeparate.
	separateNotes []separateAsk
	// separateTitle is the classifier's synthesized title for the CURRENT (active) task,
	// from the SAME classify call that produced separateNotes — used to seed the placeholder
	// todo item for the active task instead of clipping its raw text. Best-effort: only
	// overwritten by a non-empty title, so a later batch that fails to synthesize one
	// doesn't blank out an earlier good one before it's drained.
	separateTitle string
}

// separateAsk is one SEPARATE (not-a-refinement) follow-up: Text is the user's verbatim
// words (quoted back to the model in the FYI note — it must stay literal), Title is a
// concise, synthesized todo-list title for it (never the raw text — a todo list of pasted
// sentences is unreadable). Title is filled by the classifier when it can; noteSeparateRequests
// falls back to a clipped Text when the classifier didn't supply one.
type separateAsk struct {
	Text  string
	Title string
}

// queueCoalesceWindow merges queued submissions arriving in a rapid burst (a multi-line
// paste the terminal delivers as several quick Enters) into ONE transaction.
const queueCoalesceWindow = 400 * time.Millisecond

// followupClassifyCap bounds how many queued items the safe-boundary follow-up classifier
// examines per tool iteration — each costs one (cheap, bounded) LLM call. The queue is
// almost always 0–1 deep; this just caps a pathological burst.
const followupClassifyCap = 4

// DecisionKind is what the scheduler did with an accepted line.
type DecisionKind int

const (
	DecisionStarted      DecisionKind = iota // started a transaction now (was idle)
	DecisionQueued                           // queued behind the active transaction
	DecisionCoalesced                        // merged into the previous queued item (rapid paste)
	DecisionSteered                          // folded into the active transaction as a steer
	DecisionAwaitVerdict                     // planning, unclassified — classify, then Accept again with the verdict (nothing mutated)
	DecisionPlanDeferred                     // classified separate — the frontend parks it (nothing mutated)
	DecisionBusyDeclined                     // an async op owns busy while idle — declined (nothing mutated, no ghost tx to cancel)
)

// Decision is the outcome of accept — for the UI to acknowledge.
type Decision struct {
	Kind DecisionKind
	Pos  int          // 1-based queue position (Queued / Coalesced)
	Tx   *Transaction // the started or queued transaction (nil for Steered)
}

// The synchronous input-routing rule lives in decideIntake (intake.go) — one pure
// decision core shared with the headless frontend. accept below maps its RouteActions
// onto this state's mutations. Abort (Esc/Ctrl-C) is a keypress, not a submitted line,
// so it is not routed here.

// pendingClassification returns the active task's text and the queued transactions the
// background follow-up classifier hasn't examined yet, marking them examined in the SAME
// actor step (peek+mark is atomic, so a slow classify can't make the next tick re-pull the
// same item). Bounded by followupClassifyCap. Empty when idle (a fold needs an active turn).
func (s *schedState) pendingClassification() (activeID, active string, items []*Transaction) {
	if s.active == nil {
		return "", "", nil
	}
	for _, tx := range s.queue {
		if !tx.classified {
			tx.classified = true
			items = append(items, tx)
			if len(items) >= followupClassifyCap {
				break // bound one batch; the rest wait for the next tick
			}
		}
	}
	return s.active.ID, s.active.Text, items
}

// foldQueued moves the named queued transactions into the active transaction as steers
// (FIFO, preserving queue order) and returns their texts. Used by the classifier to promote
// items it judged RELATED to the active task. Ids no longer in the queue (e.g. already
// promoted by finish) are skipped — they simply run as their own turn instead.
func (s *schedState) foldQueued(expectActive string, ids []string) []string {
	if s.active == nil || len(ids) == 0 {
		return nil
	}
	// The classifier judged these items RELATED to the task that was active when it
	// snapshotted. If the active tx has since changed (Finish promoted the next while
	// the ~20s classify call was in flight), folding them would steer an UNRELATED turn
	// with a refinement of the finished task — drop them instead (they run as their own
	// turns, still queued).
	if expectActive != "" && s.active.ID != expectActive {
		return nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var folded []string
	kept := s.queue[:0]
	for _, tx := range s.queue {
		if want[tx.ID] {
			s.active.steers = append(s.active.steers, SteerEvent{Text: tx.Text, At: tx.CreatedAt})
			tx.State = TxCompleted // folded into the active turn; it has no independent run
			folded = append(folded, tx.Text)
			continue
		}
		kept = append(kept, tx)
	}
	s.queue = kept
	return folded
}

// stripMarker removes a single leading routing marker (`+`/`>`/`!`) so the stored
// transaction/steer text is the user's actual instruction, not the prefix.
func stripMarker(line string) string {
	l := strings.TrimSpace(line)
	if len(l) > 0 && (l[0] == '+' || l[0] == '>' || l[0] == '!') {
		return strings.TrimSpace(l[1:])
	}
	return l
}

func (s *schedState) newTx(text string, now time.Time, state TxState) *Transaction {
	s.seq++
	return &Transaction{ID: fmt.Sprintf("tx_%d", s.seq), Text: text, State: state, CreatedAt: now}
}

// accept routes one submitted line against the current state and returns what happened.
// The decision itself is decideIntake's (running here, ON the actor goroutine, against
// actor-owned state — no snapshot race); this maps its verdict to mutations. The three
// non-mutating outcomes (await-verdict / plan-deferred / busy-declined) mint NOTHING, so
// the old Accept-then-Cancel undo dance is gone.
func (s *schedState) accept(line string, gate GateInput, now time.Time) Decision {
	route := input.Queue
	if strings.HasPrefix(strings.TrimSpace(line), "+") {
		route = input.Steer // the explicit-steer lexical marker; bypasses the plan gate
	}
	action := decideIntake(route, gate, s.active != nil, len(s.queue), s.last, now)
	text := stripMarker(line)

	switch action {
	case ActAwaitVerdict:
		return Decision{Kind: DecisionAwaitVerdict}
	case ActDeferForPlan:
		return Decision{Kind: DecisionPlanDeferred}
	case ActRejectBusy:
		return Decision{Kind: DecisionBusyDeclined}

	case ActStartTurn:
		s.last = now
		tx := s.newTx(text, now, TxRunning)
		tx.StartedAt = now
		s.active = tx
		return Decision{Kind: DecisionStarted, Tx: tx}

	case ActSteer:
		s.last = now
		if s.active != nil {
			s.active.steers = append(s.active.steers, SteerEvent{Text: text, At: now})
		}
		return Decision{Kind: DecisionSteered}

	case ActCoalesce:
		s.last = now
		last := s.queue[len(s.queue)-1]
		last.Text += "\n" + text
		return Decision{Kind: DecisionCoalesced, Pos: len(s.queue), Tx: last}

	default: // ActQueue
		s.last = now
		tx := s.newTx(text, now, TxQueued)
		tx.classified = gate.Internal // system text is never follow-up-classified against the active task
		s.queue = append(s.queue, tx)
		return Decision{Kind: DecisionQueued, Pos: len(s.queue), Tx: tx}
	}
}

// cancel marks the active transaction cancelling (the actor invokes the real ctx cancel)
// and DISCARDS the queue. Interrupt means STOP — not "abort this unit, then promote the
// next queued item and keep going." Queued transactions are inert (they never touched
// ChatState.messages), so dropping them is clean; the active unit resolves to TxCancelled
// when finish runs, and promote() then finds an empty queue, so nothing auto-starts.
func (s *schedState) cancel() bool {
	if s.active == nil {
		return false
	}
	s.active.State = TxCancelling
	for _, tx := range s.queue {
		tx.State = TxCancelled
	}
	s.queue = nil
	s.last = time.Time{} // a cancel is a coalesce boundary — the next paste starts fresh
	return true
}

// finish marks the active transaction terminal and promotes the next queued one to
// active (FIFO). Returns the newly-active transaction, or nil if the queue is empty.
func (s *schedState) finish(result TransactionResult, now time.Time) *Transaction {
	if s.active != nil {
		st := result.State
		if !st.terminal() {
			st = TxCompleted
		}
		if s.active.State == TxCancelling { // a cancel in flight wins
			st = TxCancelled
		}
		s.active.State = st
		result.State = st
		s.active.Result = result
		s.active.EndedAt = now
		// A steer that was folded in but never drained (the classifier folded it right as the
		// turn ended, before runLoop's next safe boundary) would otherwise vanish — removed
		// from the queue, never injected. Re-queue it at the FRONT so it runs next instead of
		// being silently dropped. Normal steers are already drained by here, so this is empty.
		if st != TxCancelled && len(s.active.steers) > 0 {
			texts := make([]string, len(s.active.steers))
			for i, e := range s.active.steers {
				texts[i] = e.Text
			}
			s.active.steers = nil
			leftover := s.newTx(strings.Join(texts, "\n"), now, TxQueued)
			leftover.classified = true // already judged related; don't re-classify it
			s.queue = append([]*Transaction{leftover}, s.queue...)
		}
	}
	s.last = time.Time{} // turn boundary — never coalesce a new paste into a pre-finish item
	s.active = s.promote(now)
	return s.active
}

// promote pops the next queued transaction into the active slot.
func (s *schedState) promote(now time.Time) *Transaction {
	if len(s.queue) == 0 {
		return nil
	}
	next := s.queue[0]
	s.queue = s.queue[1:]
	next.State = TxRunning
	next.StartedAt = now
	return next
}

// drainSteers pops and returns the active transaction's pending steer texts (FIFO).
// Called at the runLoop safe boundary to fold steers into the active turn.
func (s *schedState) drainSteers() []string {
	if s.active == nil || len(s.active.steers) == 0 {
		return nil
	}
	out := make([]string, len(s.active.steers))
	for i, e := range s.active.steers {
		out[i] = e.Text
	}
	s.active.steers = nil
	return out
}

// noteSeparate appends items the classifier judged SEPARATE from the active task (append-
// only buffer, mirrors the intent of the steer list but never touches the active
// transaction's steers — these stay queued to run as their own turn later). activeTitle is
// the SAME classify call's synthesized title for the CURRENT task; a non-empty value
// overwrites the buffered title (see separateTitle), an empty one leaves it as-is.
func (s *schedState) noteSeparate(items []separateAsk, activeTitle string) {
	s.separateNotes = append(s.separateNotes, items...)
	if t := strings.TrimSpace(activeTitle); t != "" {
		s.separateTitle = t
	}
}

// drainSeparate pops and returns the buffered separate-task items (FIFO) along with the
// buffered synthesized active-task title, clearing both. Called at the runLoop safe
// boundary to track the items on the todo list and seed its placeholder title.
func (s *schedState) drainSeparate() (title string, items []separateAsk) {
	if len(s.separateNotes) == 0 {
		return "", nil
	}
	items = s.separateNotes
	s.separateNotes = nil
	title = s.separateTitle
	s.separateTitle = ""
	return title, items
}

// activeText returns the active transaction's text, or "" when idle — a nil-safe
// accessor so the separate-note drain can hand back "what's currently running" for a
// placeholder todo item without callers reaching into s.active themselves.
func (s *schedState) activeText() string {
	if s.active == nil {
		return ""
	}
	return s.active.Text
}

// clear drops every queued transaction (e.g. `/queue clear`) and returns the count.
func (s *schedState) clear() int {
	n := len(s.queue)
	for _, tx := range s.queue {
		tx.State = TxCancelled
	}
	s.queue = nil
	s.last = time.Time{} // clearing is a coalesce boundary
	return n
}

// snapshot returns the queued transactions' texts in order (for `/queue`).
func (s *schedState) snapshot() []string {
	out := make([]string, len(s.queue))
	for i, tx := range s.queue {
		out[i] = tx.Text
	}
	return out
}

// activeID returns the active transaction's id, or "" when idle.
func (s *schedState) activeID() string {
	if s.active == nil {
		return ""
	}
	return s.active.ID
}
