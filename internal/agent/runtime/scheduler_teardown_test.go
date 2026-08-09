package runtime

import (
	"context"
	"testing"
	"time"
)

// A send after the scheduler's parent context is cancelled (the actor goroutine has exited)
// must return promptly instead of blocking forever on the unbuffered command channel — the
// same deadlock family as the historic p.Send-in-Update freeze.
func TestSchedulerSendAfterTeardownDoesNotHang(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := NewScheduler(ctx, nil, nil)
	cancel()
	// Let the actor goroutine observe the cancel and close done.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			goto teardownConfirmed
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
teardownConfirmed:

	done := make(chan struct{})
	go func() {
		s.Accept("hello", GateInput{}) // routes through send()
		s.Snapshot()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler send hung after teardown")
	}
}
