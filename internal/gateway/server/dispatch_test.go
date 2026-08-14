package server

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
)

// newTestDispatcher builds a dispatcher whose work function is supplied by the
// test (no subprocess handler).
func newTestDispatcher(cap int, run func(context.Context, channels.Inbound)) *dispatcher {
	return &dispatcher{
		sem:   make(chan struct{}, cap),
		convs: make(map[string]chan channels.Inbound),
		run:   run,
	}
}

func TestDispatcherOrdersWithinConversation(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]string{}
	d := newTestDispatcher(maxConcurrentJobs, func(_ context.Context, inb channels.Inbound) {
		mu.Lock()
		got[inb.Conversation] = append(got[inb.Conversation], inb.MessageID)
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const n = 30
	for i := 0; i < n; i++ {
		d.submit(ctx, channels.Inbound{Channel: "telegram", Conversation: "A", MessageID: strconv.Itoa(i)})
		d.submit(ctx, channels.Inbound{Channel: "telegram", Conversation: "B", MessageID: strconv.Itoa(i)})
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		done := len(got["A"]) == n && len(got["B"]) == n
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, conv := range []string{"A", "B"} {
		if len(got[conv]) != n {
			t.Fatalf("conversation %s processed %d/%d", conv, len(got[conv]), n)
		}
		for i, id := range got[conv] {
			if id != strconv.Itoa(i) {
				t.Fatalf("conversation %s out of order at %d: got %s", conv, i, id)
			}
		}
	}
}

func TestDispatcherBoundsConcurrency(t *testing.T) {
	const cap = 2
	var cur, max int32
	release := make(chan struct{})
	d := newTestDispatcher(cap, func(_ context.Context, _ channels.Inbound) {
		n := atomic.AddInt32(&cur, 1)
		for {
			old := atomic.LoadInt32(&max)
			if n <= old || atomic.CompareAndSwapInt32(&max, old, n) {
				break
			}
		}
		<-release // hold the slot until released
		atomic.AddInt32(&cur, -1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Distinct conversations so each gets its own worker; only the semaphore
	// bounds how many run at once.
	for i := 0; i < 8; i++ {
		d.submit(ctx, channels.Inbound{Channel: "telegram", Conversation: strconv.Itoa(i), MessageID: "m"})
	}
	time.Sleep(80 * time.Millisecond) // let workers reach the barrier
	close(release)

	if got := atomic.LoadInt32(&max); got > cap {
		t.Errorf("max concurrent = %d, exceeds cap %d", got, cap)
	}
}
