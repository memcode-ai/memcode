package server

import (
	"context"
	"sync"
)

// maxConcurrentJobs caps how many agent jobs run at once across all
// conversations. A flood of inbound messages must not spawn an unbounded number
// of agent subprocesses; excess work queues behind this. (Jobs also serialize on
// the repo's single-writer lock, so this mostly bounds how many subprocesses
// wait at once.)
const maxConcurrentJobs = 8

// dispatcher runs work per conversation: functions submitted under the same key
// run one at a time, in submission order, so a single conversation's messages are
// handled sequentially (replies can't interleave, no double-spend on overlapping
// turns). Different conversations proceed in parallel, bounded by a global
// concurrency semaphore.
type dispatcher struct {
	sem chan struct{}

	mu    sync.Mutex
	convs map[string]*convWorker
}

// convWorker is one conversation's serial queue plus a count of in-flight
// submissions, so the worker can retire itself (freeing the goroutine and the
// map entry) once idle — otherwise a daemon talking to many rooms over its
// lifetime would leak a goroutine + channel per distinct conversation.
type convWorker struct {
	ch      chan func()
	pending int
}

func newDispatcher() *dispatcher {
	return &dispatcher{
		sem:   make(chan struct{}, maxConcurrentJobs),
		convs: make(map[string]*convWorker),
	}
}

// submit enqueues fn to run on key's serial worker, creating the worker on first
// use. Ordering is per key. An idle worker is retired, so the goroutine/map
// entry don't accumulate across many short-lived conversations.
func (d *dispatcher) submit(ctx context.Context, key string, fn func()) {
	d.mu.Lock()
	w, ok := d.convs[key]
	if !ok {
		w = &convWorker{ch: make(chan func(), 64)}
		d.convs[key] = w
		go d.serve(ctx, key, w)
	}
	w.pending++
	d.mu.Unlock()

	select {
	case w.ch <- fn:
	case <-ctx.Done():
		d.mu.Lock()
		w.pending-- // never ran; keep the counter honest
		d.mu.Unlock()
	}
}

// serve runs one conversation's functions sequentially until ctx is cancelled or
// the queue drains. Each passes through the global semaphore so total
// concurrency stays bounded even across many conversations. When no work is
// pending the worker retires and deletes its map entry under the lock; a racing
// submit that already incremented pending re-creates a worker, so nothing is
// dropped.
func (d *dispatcher) serve(ctx context.Context, key string, w *convWorker) {
	for {
		select {
		case <-ctx.Done():
			return
		case fn := <-w.ch:
			select {
			case d.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			fn()
			<-d.sem
			d.mu.Lock()
			w.pending--
			if w.pending == 0 {
				delete(d.convs, key)
				d.mu.Unlock()
				return
			}
			d.mu.Unlock()
		}
	}
}
