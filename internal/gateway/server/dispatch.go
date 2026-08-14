package server

import (
	"context"
	"io"
	"sync"

	"github.com/memcode-ai/memcode/internal/channels"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/gateway/state"
)

// maxConcurrentJobs caps how many agent jobs run at once across all
// conversations. A flood of inbound messages must not spawn an unbounded number
// of agent subprocesses; excess work queues behind this. (Jobs also serialize on
// the repo's single-writer lock, so this mostly bounds how many subprocesses
// wait at once.)
const maxConcurrentJobs = 8

// dispatcher routes each inbound message to a per-conversation worker so a single
// conversation's messages are handled one at a time, in order — replies can't
// interleave and a conversation can't double-spend on two overlapping turns.
// Different conversations still proceed in parallel, bounded by a global
// concurrency semaphore.
type dispatcher struct {
	root     string
	st       *state.Store
	settings gwconfig.Settings
	byName   map[string]replySender
	out      io.Writer
	sem      chan struct{}
	// run does the work for one message; a field so tests can substitute it for
	// the real (subprocess-spawning) handler.
	run func(ctx context.Context, inb channels.Inbound)

	mu    sync.Mutex
	convs map[string]chan channels.Inbound
}

func newDispatcher(root string, st *state.Store, settings gwconfig.Settings, byName map[string]replySender, out io.Writer) *dispatcher {
	d := &dispatcher{
		root:     root,
		st:       st,
		settings: settings,
		byName:   byName,
		out:      out,
		sem:      make(chan struct{}, maxConcurrentJobs),
		convs:    make(map[string]chan channels.Inbound),
	}
	d.run = func(ctx context.Context, inb channels.Inbound) {
		handle(ctx, d.root, d.st, d.settings, d.byName[inb.Channel], inb, d.out)
	}
	return d
}

// submit hands an inbound message to its conversation's worker, creating the
// worker on first sighting. Ordering is per (channel, conversation).
func (d *dispatcher) submit(ctx context.Context, inb channels.Inbound) {
	key := inb.Channel + ":" + inb.Conversation
	d.mu.Lock()
	ch, ok := d.convs[key]
	if !ok {
		ch = make(chan channels.Inbound, 64)
		d.convs[key] = ch
		go d.serve(ctx, ch)
	}
	d.mu.Unlock()

	select {
	case ch <- inb:
	case <-ctx.Done():
	}
}

// serve processes one conversation's messages sequentially until ctx is
// cancelled. Each job passes through the global semaphore so total concurrency
// stays bounded even across many conversations.
func (d *dispatcher) serve(ctx context.Context, ch <-chan channels.Inbound) {
	for {
		select {
		case <-ctx.Done():
			return
		case inb := <-ch:
			select {
			case d.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			d.run(ctx, inb)
			<-d.sem
		}
	}
}
