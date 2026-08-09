package runtime

import (
	"context"
	"testing"
	"time"
)

// A blocked HITL prompt must unblock when the turn is cancelled (Ctrl-C). Before the
// ctx-aware asker, cancel only cancelled the context while the runtime was parked on a
// bare channel receive — so Ctrl-C appeared to do nothing until the user hit Esc.
func TestAskUnblocksOnCancel(t *testing.T) {
	s := newTodoSession(t)
	// Mimic the TUI asker: it blocks until either an answer arrives or the turn ctx is
	// cancelled. Here no answer ever arrives — only cancellation can free it.
	s.ask = func(ctx context.Context, req AskRequest) AskResponse {
		<-ctx.Done()
		return AskResponse{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.askUserTool(ctx, []byte(`{"question":"which?","options":["a","b"]}`))
		close(done)
	}()
	cancel() // the "Ctrl-C"
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("askUserTool hung after cancel — Ctrl-C cannot interrupt a pending question")
	}
}
