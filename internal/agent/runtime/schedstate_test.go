package runtime

import (
	"testing"
	"time"
)

// A fixed base time + helper so every test is deterministic (no real clock).
var t0 = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

// classifyForTest reproduces the old classify() contract through the REAL intake
// path (decideIntake via accept) — the pure routing rule now lives in intake.go.
func classifyForTest(text string, active bool) DecisionKind {
	var s schedState
	if active {
		s.accept("occupy the active slot", GateInput{}, t0)
	}
	return s.accept(text, GateInput{}, at(5000)).Kind // outside the coalesce window
}

func TestClassifyRouting(t *testing.T) {
	cases := []struct {
		text   string
		active bool
		want   DecisionKind
	}{
		{"do the thing", false, DecisionStarted}, // idle plain → start
		{"+steer", false, DecisionStarted},       // idle + → start (nothing to steer)
		{"do the thing", true, DecisionQueued},   // busy plain → queue
		{">later", true, DecisionQueued},         // busy > → queue
		{"!stop", true, DecisionQueued},          // busy ! → queue (abort is Esc/Ctrl-C, not text)
		{"+focus on api", true, DecisionSteered}, // busy + → steer
	}
	for _, c := range cases {
		if got := classifyForTest(c.text, c.active); got != c.want {
			t.Errorf("classify(%q, active=%v) = %d, want %d", c.text, c.active, got, c.want)
		}
	}
}

// Spec: there is NO content heuristic at submit — a plain mid-turn line always queues
// (the background classifier decides later); only an explicit `+` steers synchronously.
// This pins that we do NOT guess at submit time (the design Tim asked for).
func TestClassifyNoSubmitHeuristic(t *testing.T) {
	cases := []struct {
		text string
		want DecisionKind
	}{
		{"you were supposed to build a provider abstraction", DecisionQueued}, // no submit-time guess
		{"no, use the pool manager", DecisionQueued},
		{"add a new dashboard page", DecisionQueued},
		{"+focus on the api", DecisionSteered}, // explicit steer only
	}
	for _, c := range cases {
		if got := classifyForTest(c.text, true); got != c.want {
			t.Errorf("classify(%q, active) = %d, want %d", c.text, got, c.want)
		}
		if got := classifyForTest(c.text, false); got != DecisionStarted {
			t.Errorf("classify(%q, idle) = %d, want Started", c.text, got)
		}
	}
}

// Spec: pendingClassification returns the active task + queued items exactly once (marking
// them so a slow classify can't re-pull the same item), and foldQueued promotes the RELATED
// ones onto the active transaction's steers, leaving the rest queued.
func TestPendingClassificationAndFold(t *testing.T) {
	var s schedState
	s.accept("active work", GateInput{}, at(0))
	a := s.accept("related refinement", GateInput{}, at(1000)).Tx
	b := s.accept("unrelated next task", GateInput{}, at(2000)).Tx

	activeID, active, pending := s.pendingClassification()
	if active != "active work" {
		t.Fatalf("pendingClassification active = %q, want the active task text", active)
	}
	if len(pending) != 2 {
		t.Fatalf("first peek should return both queued items, got %d", len(pending))
	}
	if _, _, again := s.pendingClassification(); len(again) != 0 {
		t.Fatalf("a second peek must return nothing (items marked), got %d", len(again))
	}

	// Fold only the related one; the unrelated stays queued.
	folded := s.foldQueued(activeID, []string{a.ID})
	if len(folded) != 1 || folded[0] != "related refinement" {
		t.Fatalf("foldQueued should return the folded text, got %#v", folded)
	}
	if len(s.queue) != 1 || s.queue[0].ID != b.ID {
		t.Fatalf("the unrelated task must stay queued, got %+v", s.snapshot())
	}
	if steers := s.drainSteers(); len(steers) != 1 || steers[0] != "related refinement" {
		t.Fatalf("the folded item must be drainable as a steer, got %#v", steers)
	}
	// Folding an id that's no longer queued (already promoted) is a no-op, not a panic.
	if folded := s.foldQueued(activeID, []string{a.ID}); len(folded) != 0 {
		t.Fatalf("re-folding a gone id should fold nothing, got %#v", folded)
	}
}

// Spec: a fold whose classified-against active tx has since changed (the turn finished
// and the next was promoted while classify was in flight) is DROPPED — it must not steer
// the now-active, unrelated turn.
func TestFoldQueuedDropsWhenActiveChanged(t *testing.T) {
	var s schedState
	s.accept("task A", GateInput{}, at(0))
	a := s.accept("refinement of A", GateInput{}, at(1000)).Tx
	activeID, _, _ := s.pendingClassification()
	// The turn for A finishes; B ("refinement of A", the only queued item) is promoted.
	s.finish(TransactionResult{State: TxCompleted}, at(2000))
	// Classifier (slow) now tries to fold a into the active — but active is no longer A.
	if folded := s.foldQueued(activeID, []string{a.ID}); len(folded) != 0 {
		t.Fatalf("fold must be dropped when the active tx changed, got %#v", folded)
	}
}

// Spec: pendingClassification is bounded per call so a pathological queue burst can't
// trigger an unbounded run of classify calls in one batch.
func TestPendingClassificationCap(t *testing.T) {
	var s schedState
	s.accept("active", GateInput{}, at(0))
	for i := 0; i < followupClassifyCap+3; i++ {
		s.accept("q", GateInput{}, at(1000*(i+1))) // 1s apart → never coalesced
	}
	if _, _, items := s.pendingClassification(); len(items) != followupClassifyCap {
		t.Fatalf("peek should cap at %d, got %d", followupClassifyCap, len(items))
	}
}

// Spec: a steer folded in but never drained (classifier folded it as the turn ended) is
// re-queued at the FRONT on finish — not silently lost.
func TestFinishRequeuesUndrainedSteer(t *testing.T) {
	var s schedState
	s.accept("active work", GateInput{}, at(0))
	a := s.accept("related refinement", GateInput{}, at(1000)).Tx
	s.accept("later task", GateInput{}, at(2000))
	activeID, _, _ := s.pendingClassification() // mark
	s.foldQueued(activeID, []string{a.ID})      // fold the related one (now a steer, undrained)
	next := s.finish(TransactionResult{State: TxCompleted}, at(3000))
	if next == nil || next.Text != "related refinement" {
		t.Fatalf("undrained folded steer must be re-queued at the front and promoted, got %+v", next)
	}
	if len(s.queue) != 1 || s.queue[0].Text != "later task" {
		t.Fatalf("the original queue should follow the re-queued steer, got %+v", s.snapshot())
	}
}

// Spec: idle plain input starts a transaction immediately.
func TestAcceptIdleStarts(t *testing.T) {
	var s schedState
	d := s.accept("fix the bug", GateInput{}, at(0))
	if d.Kind != DecisionStarted || d.Tx == nil {
		t.Fatalf("idle accept should start a tx, got %+v", d)
	}
	if s.active == nil || s.active.State != TxRunning || s.active.Text != "fix the bug" {
		t.Fatalf("active tx wrong: %+v", s.active)
	}
	if len(s.queue) != 0 {
		t.Fatal("nothing should be queued")
	}
}

// Spec: busy plain input queues (does not steer, does not touch the active tx).
func TestAcceptBusyQueues(t *testing.T) {
	var s schedState
	s.accept("active work", GateInput{}, at(0))
	d := s.accept("follow-up task", GateInput{}, at(1000)) // well outside the coalesce window
	if d.Kind != DecisionQueued || d.Pos != 1 {
		t.Fatalf("busy plain should queue at pos 1, got %+v", d)
	}
	if s.active.Text != "active work" {
		t.Fatal("the active transaction must be untouched by a queued submit")
	}
	if len(s.queue) != 1 || s.queue[0].State != TxQueued {
		t.Fatalf("queue wrong: %+v", s.queue)
	}
}

// Spec: `+input` while busy steers the active transaction (no new tx, no queue growth).
func TestAcceptBusySteers(t *testing.T) {
	var s schedState
	s.accept("active work", GateInput{}, at(0))
	d := s.accept("+actually focus on the pool manager", GateInput{}, at(1000))
	if d.Kind != DecisionSteered {
		t.Fatalf("busy + should steer, got %+v", d)
	}
	if len(s.queue) != 0 {
		t.Fatal("a steer must not create a queued transaction")
	}
	steers := s.drainSteers()
	if len(steers) != 1 || steers[0] != "actually focus on the pool manager" {
		t.Fatalf("steer text wrong (marker should be stripped): %#v", steers)
	}
	if len(s.drainSteers()) != 0 {
		t.Fatal("drainSteers must clear after draining")
	}
}

// Spec: cancelling the active transaction PRESERVES the queue.
// Spec: interrupt is STOP. Cancel aborts the active unit AND discards the queue, so a
// cancelled turn never promotes the next queued item and keeps going.
func TestCancelDiscardsQueue(t *testing.T) {
	var s schedState
	s.accept("active work", GateInput{}, at(0))
	a := s.accept("queued A", GateInput{}, at(1000)).Tx
	b := s.accept("queued B", GateInput{}, at(2000)).Tx
	if !s.cancel() {
		t.Fatal("cancel should report it acted")
	}
	if s.active.State != TxCancelling {
		t.Fatalf("active should be cancelling, got %s", s.active.State)
	}
	if len(s.queue) != 0 {
		t.Fatalf("cancel must discard the queue, got len %d", len(s.queue))
	}
	if a.State != TxCancelled || b.State != TxCancelled {
		t.Fatalf("dropped queued txs should be cancelled, got A=%s B=%s", a.State, b.State)
	}
	// finish() after the abort must NOT promote anything — interrupt means idle, not next.
	if next := s.finish(TransactionResult{State: TxCancelled}, at(3000)); next != nil {
		t.Fatalf("a cancelled turn must not promote a follow-up, got %+v", next)
	}
}

// Spec: after finish, the next queued transaction is promoted (FIFO); a cancelling
// active resolves to cancelled.
func TestFinishPromotesFIFO(t *testing.T) {
	var s schedState
	s.accept("active work", GateInput{}, at(0))
	s.accept("queued A", GateInput{}, at(1000))
	s.accept("queued B", GateInput{}, at(2000))

	next := s.finish(TransactionResult{State: TxCompleted}, at(3000))
	if next == nil || next.Text != "queued A" || next.State != TxRunning {
		t.Fatalf("finish should promote 'queued A' to running, got %+v", next)
	}
	if len(s.queue) != 1 || s.queue[0].Text != "queued B" {
		t.Fatalf("queue should now hold only 'queued B', got %+v", s.snapshot())
	}

	// A cancel discards the rest of the queue (interrupt = STOP), so the next finish
	// promotes nothing and we go idle.
	s.cancel()
	if next = s.finish(TransactionResult{}, at(4000)); next != nil {
		t.Fatalf("finish after cancel must NOT promote — interrupt is STOP, got %+v", next)
	}
	if s.active != nil {
		t.Fatal("should be idle after the last finish")
	}
}

// Spec: rapid pasted lines coalesce into ONE queued transaction; submissions outside
// the window stay separate.
func TestCoalesceWindow(t *testing.T) {
	var s schedState
	s.accept("active work", GateInput{}, at(0))
	s.accept("paste line 1", GateInput{}, at(100)) // first queued item
	d := s.accept("paste line 2", GateInput{}, at(200))
	if d.Kind != DecisionCoalesced || d.Pos != 1 {
		t.Fatalf("a rapid second paste should coalesce into item 1, got %+v", d)
	}
	if len(s.queue) != 1 {
		t.Fatalf("coalesced paste must be ONE queued tx, got %d", len(s.queue))
	}
	if s.queue[0].Text != "paste line 1\npaste line 2" {
		t.Fatalf("coalesced text = %q, want the two lines joined", s.queue[0].Text)
	}
	// A submission well past the window starts a new queued item.
	s.accept("much later task", GateInput{}, at(2000))
	if len(s.queue) != 2 {
		t.Fatalf("a submit past the window should NOT coalesce, got %d items", len(s.queue))
	}
}

// Conservative coalescing: a cancel resets the coalesce anchor (and discards the queue),
// so a rapid paste right after an abort starts a fresh queued item — it never merges into
// a pre-cancel item (which is gone anyway).
func TestNoCoalesceAcrossCancel(t *testing.T) {
	var s schedState
	s.accept("active", GateInput{}, at(0))
	s.accept("queued A", GateInput{}, at(100)) // one queued item — dropped by the cancel below
	s.cancel()                                 // boundary: queue cleared, coalesce anchor reset
	s.accept("queued B", GateInput{}, at(150)) // within 50ms of the last accept, but across the cancel
	if len(s.queue) != 1 || s.queue[0].Text != "queued B" {
		t.Fatalf("a paste after cancel must start fresh (not merge into the dropped item), got %+v", s.snapshot())
	}
}

// /queue view.
func TestSnapshot(t *testing.T) {
	var s schedState
	s.accept("active", GateInput{}, at(0))
	s.accept("q1", GateInput{}, at(1000))
	s.accept("q2", GateInput{}, at(2000))
	if snap := s.snapshot(); len(snap) != 2 || snap[0] != "q1" || snap[1] != "q2" {
		t.Fatalf("snapshot wrong: %#v", snap)
	}
}

// Spec: noteSeparate buffers texts (append-only, FIFO) plus a best-effort synthesized
// active-task title; drainSeparate pops and clears both once — mirrors drainSteers's spec.
func TestNoteAndDrainSeparate(t *testing.T) {
	var s schedState
	if title, got := s.drainSeparate(); got != nil || title != "" {
		t.Fatalf("drainSeparate on an empty buffer should be nil/empty, got title=%q items=%#v", title, got)
	}
	s.noteSeparate([]separateAsk{{Text: "add a dashboard page", Title: "Add a dashboard page"}}, "Fix the auth bug")
	s.noteSeparate([]separateAsk{{Text: "fix the readme typo"}, {Text: "also rotate the logs"}}, "")
	title, got := s.drainSeparate()
	want := []string{"add a dashboard page", "fix the readme typo", "also rotate the logs"}
	if len(got) != len(want) {
		t.Fatalf("drainSeparate = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i].Text != want[i] {
			t.Fatalf("drainSeparate[%d] = %q, want %q", i, got[i].Text, want[i])
		}
	}
	if got[0].Title != "Add a dashboard page" {
		t.Fatalf("drainSeparate[0].Title = %q, want the synthesized title", got[0].Title)
	}
	// A later noteSeparate call with an empty activeTitle must not blank out the earlier
	// non-empty one — best-effort, keep the last GOOD title until it's drained.
	if title != "Fix the auth bug" {
		t.Fatalf("drainSeparate title = %q, want the synthesized active-task title", title)
	}
	// Draining clears the buffer — a second drain returns nothing.
	if againTitle, again := s.drainSeparate(); again != nil || againTitle != "" {
		t.Fatalf("a second drainSeparate must return nothing, got title=%q items=%#v", againTitle, again)
	}
}

// Spec: activeText is a nil-safe accessor — "" when idle, the active tx's text otherwise.
func TestActiveText(t *testing.T) {
	var s schedState
	if got := s.activeText(); got != "" {
		t.Fatalf("idle activeText should be empty, got %q", got)
	}
	s.accept("do the thing", GateInput{}, at(0))
	if got := s.activeText(); got != "do the thing" {
		t.Fatalf("activeText = %q, want the active tx's text", got)
	}
}
