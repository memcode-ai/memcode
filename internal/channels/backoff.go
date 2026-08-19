package channels

import (
	"context"
	"math/rand/v2"
	"time"
)

// The one reconnect ladder every polling/streaming adapter shares: exponential backoff with
// jitter, capped, reset once the connection proves healthy again. One implementation so the
// adapters can't drift apart (a copy in signal once lost its reset and never recovered from
// its first bad night).

// Jitter scales d by a random factor in [0.75, 1.25) so reconnecting gateways don't retry in
// lockstep against a recovering server, and no fixed period resonates with a server-side
// session TTL.
func Jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.75 + rand.Float64()*0.5))
}

// Backoff is a jittered exponential reconnect ladder. The zero value is not usable; construct
// with NewBackoff.
type Backoff struct {
	floor time.Duration
	max   time.Duration
	cur   time.Duration
}

// NewBackoff returns a ladder starting at floor and doubling up to max.
func NewBackoff(floor, max time.Duration) *Backoff {
	return &Backoff{floor: floor, max: max, cur: floor}
}

// Sleep waits the current (jittered) delay — or until ctx is cancelled, returning its error —
// then doubles the delay toward the cap.
func (b *Backoff) Sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(Jitter(b.cur)):
	}
	b.cur = min(b.cur*2, b.max)
	return nil
}

// Reset drops the ladder back to its floor — call it once the connection proves healthy
// (a frame/update actually arrived), not merely when a dial succeeded.
func (b *Backoff) Reset() { b.cur = b.floor }
