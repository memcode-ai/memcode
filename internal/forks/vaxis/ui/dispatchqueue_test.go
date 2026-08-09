package ui

import (
	"sync"
	"testing"
	"time"
)

// All dispatched fns must arrive, in order, even when the downstream event
// queue is bounded and slow — the regression this queue exists to prevent is
// silent drops under streaming bursts (a dropped HITL present = engine hang).
func TestDispatchQueueNeverDropsAndPreservesOrder(t *testing.T) {
	const n = 5000
	sink := make(chan func(), 8) // much smaller than the burst, like vx.queue

	q := newDispatchQueue(func(fn func()) { sink <- fn }) // blocking post
	defer q.close()

	got := make([]int, 0, n)
	var mu sync.Mutex
	done := make(chan struct{})

	// Slow consumer (the UI event loop).
	go func() {
		for fn := range sink {
			fn()
			mu.Lock()
			if len(got) == n {
				mu.Unlock()
				close(done)
				return
			}
			mu.Unlock()
		}
	}()

	for i := 0; i < n; i++ {
		i := i
		q.enqueue(func() {
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
		})
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		mu.Lock()
		t.Fatalf("timed out: %d/%d fns delivered", len(got), n)
	}

	mu.Lock()
	defer mu.Unlock()
	for i, v := range got {
		if v != i {
			t.Fatalf("order broken at %d: got %d", i, v)
		}
	}
}

// Enqueue from inside a dispatched fn (the UI thread dispatching follow-up
// work) must not deadlock even when the pump is blocked on a full sink.
func TestDispatchQueueReentrantEnqueueDoesNotDeadlock(t *testing.T) {
	sink := make(chan func()) // unbuffered: pump is always blocked mid-post
	q := newDispatchQueue(func(fn func()) { sink <- fn })
	defer q.close()

	done := make(chan struct{})
	q.enqueue(func() {
		q.enqueue(func() { close(done) }) // reentrant: must not block
	})

	go func() {
		for fn := range sink {
			fn()
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reentrant enqueue deadlocked")
	}
}
