package vxui

import "testing"

// marker waiter: records the order in which prompts are SHOWN, so we can assert one-at-a-time FIFO.
func (s *appState) markWaiter(shown *[]string, name string) hitlWaiter {
	return hitlWaiter{id: s.nextHitlID(), present: func() { *shown = append(*shown, name) }}
}

// TestHitlQueueSerializesFIFO: prompts show one at a time, in order, and the pending count tracks
// the ones waiting behind the visible card.
func TestHitlQueueSerializesFIFO(t *testing.T) {
	s := &appState{}
	var shown []string
	a := s.markWaiter(&shown, "A")
	b := s.markWaiter(&shown, "B")
	c := s.markWaiter(&shown, "C")

	s.enqueueHitl(a) // A shows now
	s.enqueueHitl(b) // B waits
	s.enqueueHitl(c) // C waits
	if len(shown) != 1 || shown[0] != "A" {
		t.Fatalf("only A should be shown first, got %v", shown)
	}
	if s.hitlPending() != 2 {
		t.Fatalf("two prompts queued behind A, got %d", s.hitlPending())
	}

	s.advanceHitl() // answer A → B shows
	if len(shown) != 2 || shown[1] != "B" || s.hitlPending() != 1 {
		t.Fatalf("after A: shown=%v pending=%d, want [A B] / 1", shown, s.hitlPending())
	}
	s.advanceHitl() // answer B → C shows
	if len(shown) != 3 || shown[2] != "C" || s.hitlPending() != 0 {
		t.Fatalf("after B: shown=%v pending=%d, want [A B C] / 0", shown, s.hitlPending())
	}
	s.advanceHitl() // answer C → empty
	if len(s.hitlQueue) != 0 || s.hitlPending() != 0 {
		t.Fatalf("queue should be empty, got len=%d pending=%d", len(s.hitlQueue), s.hitlPending())
	}
}

// TestHitlWithdrawQueuedNeverShows: a prompt whose ctx cancels while it's still WAITING is dropped
// and never displayed; the visible card is untouched.
func TestHitlWithdrawQueuedNeverShows(t *testing.T) {
	s := &appState{}
	var shown []string
	a := s.markWaiter(&shown, "A")
	b := s.markWaiter(&shown, "B")
	s.enqueueHitl(a) // A shows
	s.enqueueHitl(b) // B waits

	s.withdrawHitl(b.id) // B's turn was cancelled before it ever showed
	if s.hitlPending() != 0 {
		t.Fatalf("withdrawing the queued prompt → 0 pending, got %d", s.hitlPending())
	}
	s.advanceHitl() // answer A → nothing left, B must never appear
	if len(shown) != 1 || shown[0] != "A" {
		t.Fatalf("B was withdrawn and must never show, got %v", shown)
	}
}

// TestHitlWithdrawActiveAdvances: cancelling the prompt that's ON SCREEN clears it and surfaces the
// next one (e.g. the turn's approval card is cancelled by Esc → the queued shell command's card shows).
func TestHitlWithdrawActiveAdvances(t *testing.T) {
	s := &appState{}
	var shown []string
	a := s.markWaiter(&shown, "A")
	b := s.markWaiter(&shown, "B")
	s.enqueueHitl(a) // A shows
	s.enqueueHitl(b) // B waits

	s.withdrawHitl(a.id) // A (on screen) cancelled → B advances in
	if len(shown) != 2 || shown[1] != "B" || s.hitlPending() != 0 {
		t.Fatalf("withdrawing the active card should advance to B, got shown=%v pending=%d", shown, s.hitlPending())
	}
}
