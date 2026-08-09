package ui

import "sync"

// dispatchQueue decouples Dispatch callers from the bounded vaxis event
// queue. Callers enqueue without blocking or dropping; a single pump
// goroutine forwards each fn to the event loop with a BLOCKING post.
//
// Dropping is not an option here: a dropped SyncFunc can be a HITL approval
// card presentation, which leaves the engine blocked on a reply channel for
// a card that never rendered — an unexplainable dead hang. Blocking the
// caller isn't an option either: Dispatch may run on the UI thread itself
// (a SyncFunc dispatching follow-up work), where a blocking post on a full
// queue deadlocks the very loop that would drain it. An unbounded FIFO with
// a blocking pump gives both guarantees; order is preserved.
type dispatchQueue struct {
	post func(func()) // blocking post into the event loop

	mu     sync.Mutex
	cond   *sync.Cond
	items  []func()
	closed bool
}

func newDispatchQueue(post func(func())) *dispatchQueue {
	q := &dispatchQueue{post: post}
	q.cond = sync.NewCond(&q.mu)
	go q.pump()
	return q
}

func (q *dispatchQueue) enqueue(fn func()) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, fn)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *dispatchQueue) pump() {
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.items) == 0 && q.closed {
			q.mu.Unlock()
			return
		}
		batch := q.items
		q.items = nil
		q.mu.Unlock()
		for _, fn := range batch {
			q.post(fn)
		}
	}
}

// close stops accepting new work; queued items still drain.
func (q *dispatchQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}
