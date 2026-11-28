package runtime

import (
	"strings"
	"sync"

	"github.com/memcode-ai/memcode/internal/wire"
)

// Background results (a report-back agent finished, a background shell exited) are
// the outcome of work the agent itself started, so they belong to the agent's loop,
// not the user's input queue. While a turn runs they are posted here and folded into
// that turn at the loop's safe boundary (after a tool_result, before the next model
// call), the way Claude Code delivers task notifications. Nothing is waiting in the
// queue behind the turn, and the model never mistakes a result for something the user
// typed. Whatever the running turn did not reach (it ended on that model call) is
// taken back by the frontend at turn end and starts a turn of its own.
type bgEvents struct {
	mu    sync.Mutex
	items []string
}

// PostBackground queues a finished background result for the ACTIVE turn. Safe from
// any goroutine. The text is the same report the idle path hands over as a turn.
func (s *Session) PostBackground(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.bg.mu.Lock()
	s.bg.items = append(s.bg.items, text)
	s.bg.mu.Unlock()
}

// TakeBackground removes and returns every background result not yet delivered. The
// loop calls it at its safe boundary; the frontend calls it after a turn ends, to start
// a turn for results the finished turn never reached.
func (s *Session) TakeBackground() []string {
	s.bg.mu.Lock()
	defer s.bg.mu.Unlock()
	out := s.bg.items
	s.bg.items = nil
	return out
}

// backgroundNote frames delivered results as runtime events: the model must know they
// are results of its own background work, not user input to reconcile like a steer.
func backgroundNote(items []string) string {
	var b strings.Builder
	b.WriteString("[runtime event — not from the user] Background work you started has finished while this turn was running. Its result follows; use it where it bears on the task.")
	for _, it := range items {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(it))
	}
	return b.String()
}

// foldBackground appends pending background results to the conversation. It returns
// how many were folded. Called only at the loop's safe boundary.
func (s *Session) foldBackground(messages *[]wire.Message) int {
	items := s.TakeBackground()
	if len(items) == 0 {
		return 0
	}
	*messages = append(*messages, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: backgroundNote(items)}}})
	return len(items)
}
