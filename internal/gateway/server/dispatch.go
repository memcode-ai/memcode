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
	convs map[string]chan func()
}

func newDispatcher() *dispatcher {
	return &dispatcher{
		sem:   make(chan struct{}, maxConcurrentJobs),
		convs: make(map[string]chan func()),
	}
}

// submit enqueues fn to run on key's serial worker, creating the worker on first
// use. Ordering is per key.
func (d *dispatcher) submit(ctx context.Context, key string, fn func()) {
	d.mu.Lock()
	ch, ok := d.convs[key]
	if !ok {
		ch = make(chan func(), 64)
		d.convs[key] = ch
		go d.serve(ctx, ch)
	}
	d.mu.Unlock()

	select {
	case ch <- fn:
	case <-ctx.Done():
	}
}

// serve runs one conversation's functions sequentially until ctx is cancelled.
// Each passes through the global semaphore so total concurrency stays bounded
// even across many conversations.
func (d *dispatcher) serve(ctx context.Context, ch <-chan func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case fn := <-ch:
			select {
			case d.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			fn()
			<-d.sem
		}
	}
}
