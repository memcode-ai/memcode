package vxui

import (
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
)

// TestTurnElapsedExcludesHitlWait: the "Thinking…" clock measures LLM time, NOT the time the user
// spent answering an ask/approval card. turnElapsed subtracts both finished and in-progress waits.
func TestTurnElapsedExcludesHitlWait(t *testing.T) {
	s := &appState{}
	s.turnStart = time.Now().Add(-10 * time.Second)

	if d := s.turnElapsed(); d < 9*time.Second || d > 11*time.Second {
		t.Fatalf("no-wait elapsed should be ~10s, got %v", d)
	}
	s.hitlWait = 4 * time.Second // a finished HITL wait
	if d := s.turnElapsed(); d < 5*time.Second || d > 7*time.Second {
		t.Fatalf("elapsed must subtract the finished wait (~6s), got %v", d)
	}
	s.hitlWaitAt = time.Now().Add(-3 * time.Second) // a card currently up
	if d := s.turnElapsed(); d < 2*time.Second || d > 4*time.Second {
		t.Fatalf("elapsed must also subtract the in-progress wait (~3s), got %v", d)
	}
	s.hitlWait = 100 * time.Second // over-subtraction must floor at 0, never negative
	if d := s.turnElapsed(); d < 0 {
		t.Fatalf("elapsed must never be negative, got %v", d)
	}
}

// TestHitlWaitBeginEnd: a card raised starts the wait; an overlapping card doesn't reset it; the
// wait closes (and accumulates) only once NO card remains up.
func TestHitlWaitBeginEnd(t *testing.T) {
	s := &appState{}
	s.beginHitlWait()
	if s.hitlWaitAt.IsZero() {
		t.Fatal("beginHitlWait must start the wait clock")
	}
	start := s.hitlWaitAt
	s.askReq = &runtime.AskRequest{} // a second card while one is up
	s.beginHitlWait()
	if !s.hitlWaitAt.Equal(start) {
		t.Fatal("an overlapping card must NOT reset the wait start")
	}

	// A card still up → endHitlWait is a no-op (keep counting).
	before := s.hitlWait
	s.endHitlWait()
	if s.hitlWait != before || s.hitlWaitAt.IsZero() {
		t.Fatal("endHitlWait must not close the wait while a card is still up")
	}

	// Last card cleared → accumulate the elapsed wait and reset the start.
	s.askReq = nil
	s.hitlWaitAt = time.Now().Add(-2 * time.Second)
	s.endHitlWait()
	if !s.hitlWaitAt.IsZero() {
		t.Fatal("endHitlWait must clear the wait start once no card is up")
	}
	if s.hitlWait < 1*time.Second {
		t.Fatalf("endHitlWait must accumulate the wait (~2s), got %v", s.hitlWait)
	}
}
