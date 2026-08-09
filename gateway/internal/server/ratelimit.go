package server

import (
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Per-org request-rate floor. Before this, a valid org token could hammer
// /v1/* unbounded — the only backpressure was the upstream provider quota at
// full speed. This is a fixed-window counter per org per minute: coarse, in
// memory, per instance (Cloud Run may run several; the effective ceiling is
// limit × instances, which is still a floor worth having — a real distributed
// limiter is a post-launch upgrade).
type orgRateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Time
	counts map[string]int
}

// defaultOrgRPM is deliberately generous: an agent loop makes a handful of
// model calls per turn; 300/min only trips on runaway automation or abuse.
const defaultOrgRPM = 300

func newOrgRateLimiter() *orgRateLimiter {
	limit := defaultOrgRPM
	if v := os.Getenv("MEMCODE_ORG_RPM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	return &orgRateLimiter{limit: limit, counts: map[string]int{}}
}

// allow counts one request for orgID in the current minute window. Nil-safe
// (tests build handler literals without a limiter; nil = unlimited).
func (l *orgRateLimiter) allow(orgID string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.window) >= time.Minute {
		l.window = now
		clear(l.counts)
	}
	l.counts[orgID]++
	return l.counts[orgID] <= l.limit
}

// deny writes the 429. Retry-After matters: the SDK honors it on backoff.
func (l *orgRateLimiter) deny(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "30")
	httpError(w, http.StatusTooManyRequests, "rate limit exceeded — slow down (this resets within a minute)")
}
