package vxui

import "testing"

// Regression: an approval/ask card used to share the ordinary composer, so a draft typed BEFORE
// the card appeared was read as "tell" feedback (or an ask answer) on a plain Enter and sent to
// the agent. Cards now get a blank temporary buffer and the draft is restored afterwards.
func TestModalInputPreservesUnsentDraft(t *testing.T) {
	s := &appState{composer: "half-typed [pasted #1 1KB]", cursor: 7, pastes: map[string]string{"[pasted #1 1KB]": "body"}}

	s.beginModalInput()
	if s.composer != "" || s.cursor != 0 || s.pastes != nil {
		t.Fatalf("modal card must start with a blank reply buffer, got %q/%d/%v", s.composer, s.cursor, s.pastes)
	}
	if got := s.modalReply(); got != "" {
		t.Fatalf("modalReply must not see the pre-existing draft, got %q", got)
	}

	s.composer = "feedback for the card" // typed into the card
	s.finishModalInput()
	if s.composer != "half-typed [pasted #1 1KB]" || s.cursor != 7 || s.pastes["[pasted #1 1KB]"] != "body" {
		t.Fatalf("draft not restored after the card: %q/%d/%v", s.composer, s.cursor, s.pastes)
	}
	if s.modalDraftActive {
		t.Fatal("modal stash should be released")
	}
}

// A draft survives a whole queue of HITL cards: finishModalInput restores it before advanceHitl
// presents the next card, which snapshots it again.
func TestModalInputDraftSurvivesQueuedCards(t *testing.T) {
	s := &appState{composer: "my draft", cursor: 8}
	var shown int
	card := func() hitlWaiter {
		return hitlWaiter{id: s.nextHitlID(), present: func() { shown++; s.beginModalInput() }}
	}
	a, b := card(), card()
	s.enqueueHitl(a)
	s.enqueueHitl(b)
	if s.composer != "" {
		t.Fatalf("card A should own a blank buffer, got %q", s.composer)
	}

	s.composer = "answer A"
	s.finishModalInput()
	s.advanceHitl() // B presents
	if shown != 2 || s.composer != "" {
		t.Fatalf("card B should show with a blank buffer, shown=%d composer=%q", shown, s.composer)
	}

	s.withdrawHitl(b.id) // B cancelled while on screen
	if s.composer != "my draft" || s.cursor != 8 {
		t.Fatalf("draft lost across queued cards: %q/%d", s.composer, s.cursor)
	}
}
