package runtime

import (
	"context"
	"time"
)

// Scheduler is the actor wrapper around schedState: ONE owner goroutine (run) holds the
// state and serializes every access through a command channel. Neither the TUI intake
// nor the executor mutates scheduler state directly — they only send commands and read
// replies — so there is no shared mutable state to race (the design constraint).
//
// Drivers:
//   - intake (TUI Update goroutine): Accept / Cancel / Snapshot — fast,
//     synchronous round-trips; they never block on execution.
//   - executor (TUI engine goroutine): on a "run" kick, TakeActive() → RunTransaction →
//     Finish; DrainSteers mid-run at the runLoop safe boundary. TakeActive is
//     NON-BLOCKING so the executor can keep selecting on its other inputs (sentinels).
//
// The actor owns the per-transaction context: TakeActive mints context.WithCancel and
// stores the cancel, so Cancel() is just a command and the executor never owns
// cancellation state.
type Scheduler struct {
	cmds chan schedCmd
	obs  SchedulerObserver
	now  func() time.Time
	done chan struct{} // closed when the actor goroutine exits, so send() can't hang post-teardown
}

// SchedulerObserver is notified (best-effort) when the active/queued set changes, so a
// front-end can render a quiet "(N queued)" indicator. The queue is runtime-managed;
// there is no user-facing queue-editing command in Phase 1.
type SchedulerObserver interface {
	SchedulerChanged(activeID string, queued []string)
}

type cmdKind int

const (
	cmdAccept cmdKind = iota
	cmdCancel
	cmdTake
	cmdFinish
	cmdDrain
	cmdSnapshot
	cmdPending
	cmdFold
	cmdNoteSeparate
	cmdDrainSeparate
)

type schedCmd struct {
	kind        cmdKind
	line        string
	gate        GateInput
	ids         []string
	sep         []separateAsk
	activeTitle string
	result      TransactionResult
	reply       chan schedReply
}

type schedReply struct {
	decision    Decision
	tx          *Transaction
	ctx         context.Context
	ok          bool
	steers      []string
	snap        []string
	activeID    string
	active      string
	activeTitle string
	txs         []*Transaction
	folded      []string
	separate    []separateAsk
}

// NewScheduler starts the actor goroutine; it runs until parent is cancelled. clock is
// injectable for tests — pass time.Now in production.
func NewScheduler(parent context.Context, obs SchedulerObserver, clock func() time.Time) *Scheduler {
	if clock == nil {
		clock = time.Now
	}
	s := &Scheduler{cmds: make(chan schedCmd), obs: obs, now: clock, done: make(chan struct{})}
	go s.run(parent)
	return s
}

func (s *Scheduler) run(parent context.Context) {
	defer close(s.done) // unblock any send() waiting on a torn-down scheduler
	var st schedState
	var activeCancel context.CancelFunc
	dispatched := false // is the current active tx already handed to the executor?

	notify := func() {
		if s.obs != nil {
			s.obs.SchedulerChanged(st.activeID(), st.snapshot())
		}
	}

	for {
		select {
		case <-parent.Done():
			return
		case c := <-s.cmds:
			switch c.kind {
			case cmdAccept:
				c.reply <- schedReply{decision: st.accept(c.line, c.gate, s.now())}
				notify()
			case cmdCancel:
				ok := st.cancel()
				if ok {
					if dispatched && activeCancel != nil {
						activeCancel() // a running turn — cancel its ctx; runLoop unwinds and calls Finish
					} else if !dispatched {
						// The active tx was cancelled in the window AFTER promotion but BEFORE the
						// executor took it: no turn is running it, so no Finish is coming. Finalize
						// it here (queue already discarded by cancel) so the slot can't wedge and a
						// later TakeActive can't run a cancelled tx.
						st.finish(TransactionResult{State: TxCancelled}, s.now())
						dispatched = false
					}
				}
				c.reply <- schedReply{ok: ok}
				notify()
			case cmdTake:
				// Non-blocking: hand the active, not-yet-dispatched tx to the executor with
				// a fresh cancellable ctx. Replies ok=false when there's nothing to run, OR
				// when the active tx isn't runnable (cancelled/terminal in the take window) —
				// without the State guard an Esc'd-but-not-yet-dispatched tx ran to completion.
				if st.active == nil || dispatched || st.active.State != TxRunning {
					c.reply <- schedReply{ok: false}
					break
				}
				ctx, cancel := context.WithCancel(parent)
				activeCancel = cancel
				dispatched = true
				c.reply <- schedReply{tx: st.active, ctx: ctx, ok: true}
			case cmdFinish:
				if activeCancel != nil {
					activeCancel() // release the per-turn ctx (go vet lostcancel) — the turn is done
				}
				next := st.finish(c.result, s.now()) // mark terminal + promote the next (FIFO)
				activeCancel = nil
				dispatched = false
				c.reply <- schedReply{tx: next, ok: next != nil} // ok = a tx was promoted → kick the executor
				notify()
			case cmdDrain:
				c.reply <- schedReply{steers: st.drainSteers()}
			case cmdPending:
				activeID, active, txs := st.pendingClassification()
				c.reply <- schedReply{activeID: activeID, active: active, txs: txs}
			case cmdFold:
				folded := st.foldQueued(c.line, c.ids) // c.line carries the expected active id
				c.reply <- schedReply{folded: folded}
				notify()
			case cmdNoteSeparate:
				st.noteSeparate(c.sep, c.activeTitle)
				c.reply <- schedReply{}
			case cmdDrainSeparate:
				title, items := st.drainSeparate()
				c.reply <- schedReply{active: st.activeText(), activeTitle: title, separate: items}
			case cmdSnapshot:
				c.reply <- schedReply{snap: st.snapshot()}
			}
		}
	}
}

func (s *Scheduler) send(c schedCmd) schedReply {
	c.reply = make(chan schedReply, 1)
	// Never block forever once the actor goroutine has exited (parent ctx cancelled): a late
	// Accept/Cancel/Snapshot in the teardown window returns a zero reply (a no-op) instead of
	// hanging — the same deadlock family as the historic p.Send-in-Update freeze.
	select {
	case s.cmds <- c:
	case <-s.done:
		return schedReply{}
	}
	select {
	case r := <-c.reply:
		return r
	case <-s.done:
		return schedReply{}
	}
}

// Accept routes one submitted line against the current state (intake; never blocks on
// execution). gate carries the non-scheduler context (plan phase/epoch, relevance
// verdict, busy owner) — GateInput{} is plain-chat behavior. Returns what the scheduler
// did, for the UI to acknowledge; the AwaitVerdict/PlanDeferred/BusyDeclined kinds
// mutated nothing and expect the frontend to act (classify / park / decline).
func (s *Scheduler) Accept(line string, gate GateInput) Decision {
	return s.send(schedCmd{kind: cmdAccept, line: line, gate: gate}).decision
}

// Cancel aborts the active transaction (Esc/Ctrl-C): marks it cancelling, cancels its
// context, and DISCARDS the queue — interrupt means STOP, not advance to the next queued
// item. Reports whether there was an active transaction.
func (s *Scheduler) Cancel() bool { return s.send(schedCmd{kind: cmdCancel}).ok }

// TakeActive hands the active, not-yet-running transaction to the executor with its
// cancellable context. NON-BLOCKING: returns (nil, nil, false) when nothing is ready, so
// the executor stays free to handle its other inputs. The executor calls this on a "run"
// kick (after Accept reported Started, or after Finish promoted the next).
func (s *Scheduler) TakeActive() (*Transaction, context.Context, bool) {
	r := s.send(schedCmd{kind: cmdTake})
	return r.tx, r.ctx, r.ok
}

// Finish marks the active transaction terminal and promotes the next queued one (FIFO).
// Returns true when a transaction was promoted — the executor should kick itself to run it.
func (s *Scheduler) Finish(result TransactionResult) (promoted bool) {
	return s.send(schedCmd{kind: cmdFinish, result: result}).ok
}

// DrainSteers returns and clears the active transaction's pending steers — called at the
// runLoop safe boundary to fold `+input` into the active turn.
func (s *Scheduler) DrainSteers() []string { return s.send(schedCmd{kind: cmdDrain}).steers }

// PendingClassification returns the active task's text and the queued transactions the
// background follow-up classifier hasn't examined yet (marking them examined). The classifier
// runs the cheap structured-output model OFF the actor goroutine, then calls FoldQueued with
// the ids it judged related to the active task.
func (s *Scheduler) PendingClassification() (activeID, active string, items []*Transaction) {
	r := s.send(schedCmd{kind: cmdPending})
	return r.activeID, r.active, r.txs
}

// FoldQueued promotes the named queued transactions into the active transaction as steers
// (the classifier's "related" verdict) and returns their folded texts. expectActive is the
// active-tx id the classifier judged against — the fold is dropped if the active tx changed
// since (so a refinement of a finished task can't steer an unrelated running turn).
func (s *Scheduler) FoldQueued(expectActive string, ids []string) []string {
	return s.send(schedCmd{kind: cmdFold, line: expectActive, ids: ids}).folded
}

// NoteSeparate records items the background follow-up classifier judged SEPARATE from the
// active task (the "not related" verdict) — they stay queued to run as their own turn
// later, but this buffers them so the runLoop safe boundary can track them on the todo
// list and give the model a brief FYI note (see DrainSeparate). activeTitle is the SAME
// classify call's synthesized title for the CURRENT task (empty when synthesis failed).
func (s *Scheduler) NoteSeparate(items []separateAsk, activeTitle string) {
	if len(items) == 0 {
		return
	}
	s.send(schedCmd{kind: cmdNoteSeparate, sep: items, activeTitle: activeTitle})
}

// DrainSeparate returns and clears the buffered separate-task items, along with the active
// transaction's raw text and the classifier's synthesized title for it (for seeding a
// "current task" placeholder todo item the first time this fires — both go through the
// synthTitle guard, so raw prose never becomes a list title) — called at the runLoop
// safe boundary.
func (s *Scheduler) DrainSeparate() (activeText, activeTitle string, items []separateAsk) {
	r := s.send(schedCmd{kind: cmdDrainSeparate})
	return r.active, r.activeTitle, r.separate
}

// Snapshot returns the queued transactions' texts (for a quiet UI indicator).
func (s *Scheduler) Snapshot() []string { return s.send(schedCmd{kind: cmdSnapshot}).snap }
