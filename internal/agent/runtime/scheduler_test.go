package runtime

import (
	"context"
	"testing"
	"time"
)

// The actor is a thin serializer over schedState (exhaustively tested elsewhere); these
// verify the channel plumbing: Accept → TakeActive hands off with a context, a second
// take finds nothing, queue + Finish promotes FIFO, and Cancel preserves the queue.
func TestSchedulerActorFlow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewScheduler(ctx, nil, func() time.Time { return t0 })

	if d := s.Accept("first", GateInput{}); d.Kind != DecisionStarted {
		t.Fatalf("first (idle) should start, got %d", d.Kind)
	}
	tx, tctx, ok := s.TakeActive()
	if !ok || tx == nil || tx.Text != "first" || tctx == nil {
		t.Fatalf("TakeActive should hand 'first' with a ctx, got ok=%v tx=%+v ctx=%v", ok, tx, tctx)
	}
	if _, _, ok := s.TakeActive(); ok {
		t.Fatal("a dispatched active tx must not be handed out twice")
	}
	if d := s.Accept("second", GateInput{}); d.Kind != DecisionQueued {
		t.Fatalf("second (busy) should queue, got %d", d.Kind)
	}
	if promoted := s.Finish(TransactionResult{State: TxCompleted}); !promoted {
		t.Fatal("finish should promote the queued 'second'")
	}
	tx2, _, ok := s.TakeActive()
	if !ok || tx2.Text != "second" {
		t.Fatalf("should take 'second' after promotion, got ok=%v tx=%+v", ok, tx2)
	}
	if promoted := s.Finish(TransactionResult{State: TxCompleted}); promoted {
		t.Fatal("empty queue → finish promotes nothing")
	}
	if _, _, ok := s.TakeActive(); ok {
		t.Fatal("idle scheduler should have nothing to take")
	}
}

func TestSchedulerCancelDiscardsQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewScheduler(ctx, nil, func() time.Time { return t0 })

	s.Accept("active", GateInput{})
	tx, tctx, _ := s.TakeActive()
	if tx == nil {
		t.Fatal("expected an active tx")
	}
	s.Accept("queued one", GateInput{})
	if !s.Cancel() {
		t.Fatal("cancel should report it acted")
	}
	if tctx.Err() == nil {
		t.Fatal("cancel must cancel the active transaction's context")
	}
	// Interrupt = STOP: the queue is dropped, so the cancelled turn can't promote a
	// follow-up and keep going.
	if snap := s.Snapshot(); len(snap) != 0 {
		t.Fatalf("cancel must discard the queue, got %#v", snap)
	}
	if promoted := s.Finish(TransactionResult{State: TxCancelled}); promoted {
		t.Fatal("finish after cancel must not promote a queued follow-up")
	}
}

// TestSchedulerNoteAndDrainSeparate proves the actor plumbing for the separate-task
// buffer: NoteSeparate/DrainSeparate round-trip through the actor goroutine, and
// DrainSeparate reports the active transaction's text and the classifier's synthesized
// title for it alongside the buffered texts — mirrors TestSchedulerActorFlow's coverage
// of FoldQueued's plumbing.
func TestSchedulerNoteAndDrainSeparate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewScheduler(ctx, nil, func() time.Time { return t0 })

	if active, title, items := s.DrainSeparate(); active != "" || title != "" || items != nil {
		t.Fatalf("idle drain should be empty, got active=%q title=%q items=%#v", active, title, items)
	}

	s.Accept("active work", GateInput{}) // starts the active transaction
	s.NoteSeparate([]separateAsk{{Text: "add a dashboard page", Title: "Add a dashboard page"}}, "Fix the auth bug")
	s.NoteSeparate([]separateAsk{{Text: "fix the readme typo", Title: "Fix the readme typo"}}, "")

	active, activeTitle, items := s.DrainSeparate()
	if active != "active work" {
		t.Fatalf("DrainSeparate active = %q, want the running transaction's text", active)
	}
	if activeTitle != "Fix the auth bug" {
		t.Fatalf("DrainSeparate activeTitle = %q, want the synthesized title from the note that supplied one", activeTitle)
	}
	want := []string{"add a dashboard page", "fix the readme typo"}
	if len(items) != len(want) || items[0].Text != want[0] || items[1].Text != want[1] {
		t.Fatalf("DrainSeparate items = %#v, want %#v", items, want)
	}
	// Draining clears the buffer.
	if _, _, items := s.DrainSeparate(); items != nil {
		t.Fatalf("a second drain should return nothing, got %#v", items)
	}
	// NoteSeparate with no items is a no-op, not a panic or a phantom entry.
	s.NoteSeparate(nil, "")
	if _, _, items := s.DrainSeparate(); items != nil {
		t.Fatalf("noting an empty slice should add nothing, got %#v", items)
	}
}
