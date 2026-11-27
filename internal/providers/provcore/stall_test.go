package provcore

import (
	"context"
	"testing"
	"time"
)

// A stream that goes silent is cancelled and reported stalled; one that keeps
// receiving data is left alone. A stall classifies as a retryable 503.
func TestStallWatch(t *testing.T) {
	ctx, w, stop := WatchStalls(context.Background(), 40*time.Millisecond)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a silent stream must be cancelled")
	}
	if !w.Stalled() {
		t.Fatal("the cancellation must be reported as a stall")
	}

	ctx2, w2, stop2 := WatchStalls(context.Background(), 80*time.Millisecond)
	defer stop2()
	for i := 0; i < 6; i++ {
		time.Sleep(30 * time.Millisecond)
		w2.Touch()
	}
	if ctx2.Err() != nil || w2.Stalled() {
		t.Fatal("a stream that keeps receiving data must not be cancelled")
	}

	code, _, ok := APIErrorInfo(&StreamStallError{Backend: "gemini", Idle: time.Minute})
	if !ok || code != 503 || !IsRetryable(code) {
		t.Fatalf("a stall must classify as a retryable 503, got %d ok=%v", code, ok)
	}
}

// Backoff waits are real and grow: never below half the nominal step, capped.
func TestBackoffEqualJitter(t *testing.T) {
	for attempt, nominal := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 9: 20 * time.Second} {
		for i := 0; i < 50; i++ {
			if d := Backoff(attempt); d < nominal/2 || d >= nominal {
				t.Fatalf("Backoff(%d) = %s, want in [%s, %s)", attempt, d, nominal/2, nominal)
			}
		}
	}
}
