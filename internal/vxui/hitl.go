package vxui

import (
	"fmt"
	"sync/atomic"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"
)

// HITL (human-in-the-loop) prompt queue.
//
// The engine can ask the user from MORE than one goroutine — the active turn AND a hand-run `$`
// shell command, or two concurrent shell commands — but the live region shows ONE card at a time.
// A plain mutex "fixes" the overwrite but is the wrong model: it blocks a goroutine holding a lock
// for as long as the user takes to answer, it ignores context (a cancelled turn can't bail while
// parked in Lock), and it hides that anything is waiting.
//
// Instead, prompts join a FIFO QUEUE owned by the UI thread. hitlQueue[0] is the card currently
// shown (its fields live in s.pending/s.askReq, which the renderer + key handler already use);
// [1:] wait their turn; the UI shows a "+N queued" badge. Each engine goroutine waits only on its
// OWN reply-or-ctx, so a cancelled request bails instantly — even while still queued — and withdraws
// itself from the queue. All queue mutation happens on the UI thread (inside SetState); the engine
// side only mints an id (atomically, before dispatch) and dispatches enqueue/withdraw.

// hitlWaiter is one queued prompt. present() installs it as the active card (sets the pending/
// askReq fields); id identifies it for withdrawal when its requester's context is cancelled.
type hitlWaiter struct {
	id      int64
	present func()
}

// nextHitlID mints a process-unique id on the ENGINE goroutine (before the enqueue dispatch lands),
// so a context that cancels in the gap still knows which waiter to withdraw.
func (s *appState) nextHitlID() int64 { return atomic.AddInt64(&s.hitlSeq, 1) }

// enqueueHitl appends a prompt and, if it's the only one, shows it immediately (UI thread).
func (s *appState) enqueueHitl(w hitlWaiter) {
	s.hitlQueue = append(s.hitlQueue, w)
	if len(s.hitlQueue) == 1 {
		w.present()
		s.beginHitlWait()
	}
}

// advanceHitl pops the just-answered head and shows the next prompt, if any. Called on the UI thread
// AFTER answerApproval/answerAsk has cleared the active-card fields. Ordering matters for the clock:
// present the next FIRST (so endHitlWait sees a card still up and the HITL pause stays continuous
// across back-to-back cards); only when the queue empties does endHitlWait fold the wait and resume.
func (s *appState) advanceHitl() {
	if len(s.hitlQueue) > 0 {
		s.hitlQueue = s.hitlQueue[1:]
	}
	if len(s.hitlQueue) > 0 {
		s.hitlQueue[0].present()
		s.beginHitlWait()
	}
	s.endHitlWait()
}

// withdrawHitl drops the prompt with id whose requester gave up (ctx cancelled). If it's the card on
// screen, clear it and advance to the next; if it's still waiting, just remove it from the queue.
func (s *appState) withdrawHitl(id int64) {
	if len(s.hitlQueue) > 0 && s.hitlQueue[0].id == id {
		s.pending, s.approval, s.approveChoice = nil, nil, 0
		s.askReq, s.askReply, s.askChoice = nil, nil, 0
		s.advanceHitl()
		return
	}
	for i := range s.hitlQueue {
		if s.hitlQueue[i].id == id {
			s.hitlQueue = append(s.hitlQueue[:i], s.hitlQueue[i+1:]...)
			return
		}
	}
}

// hitlPending is the number of prompts waiting BEHIND the one on screen (for the "+N queued" badge).
func (s *appState) hitlPending() int {
	if len(s.hitlQueue) <= 1 {
		return 0
	}
	return len(s.hitlQueue) - 1
}

// hitlBadge renders a muted "N more queued" line shown under the active card, so the user knows
// answering this one will surface another. Empty when nothing is waiting.
func (s *appState) hitlBadge() []ui.Widget {
	n := s.hitlPending()
	if n == 0 {
		return nil
	}
	noun := "question"
	if n > 1 {
		noun = "questions"
	}
	return []ui.Widget{ui.RichText{Spans: []ui.TextSpan{
		{Text: fmt.Sprintf("  ↳ %d more %s queued", n, noun), Style: s.sty.muted},
	}}}
}
