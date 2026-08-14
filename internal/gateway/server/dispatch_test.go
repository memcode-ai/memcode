package server

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDispatcherOrdersWithinKey(t *testing.T) {
	d := newDispatcher()
	var mu sync.Mutex
	got := map[string][]int{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const n = 30
	for i := 0; i < n; i++ {
		i := i
		for _, key := range []string{"A", "B"} {
			key := key
			d.submit(ctx, key, func() {
				mu.Lock()
				got[key] = append(got[key], i)
				mu.Unlock()
			})
		}
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
	for _, key := range []string{"A", "B"} {
		if len(got[key]) != n {
			t.Fatalf("key %s ran %d/%d", key, len(got[key]), n)
		}
		for i, v := range got[key] {
			if v != i {
				t.Fatalf("key %s out of order at %d: got %d", key, i, v)
			}
		}
	}
}

func TestDispatcherBoundsConcurrency(t *testing.T) {
	const cap = 2
	d := &dispatcher{sem: make(chan struct{}, cap), convs: make(map[string]chan func())}

	var cur, max int32
	release := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Distinct keys so each gets its own worker; only the semaphore bounds how many
	// run at once.
	for i := 0; i < 8; i++ {
		d.submit(ctx, strconv.Itoa(i), func() {
			n := atomic.AddInt32(&cur, 1)
			for {
				old := atomic.LoadInt32(&max)
				if n <= old || atomic.CompareAndSwapInt32(&max, old, n) {
					break
				}
			}
			<-release
			atomic.AddInt32(&cur, -1)
		})
	}
	time.Sleep(80 * time.Millisecond) // let workers reach the barrier
	close(release)

	if got := atomic.LoadInt32(&max); got > cap {
		t.Errorf("max concurrent = %d, exceeds cap %d", got, cap)
	}
}
