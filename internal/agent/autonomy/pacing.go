package autonomy

import (
	"math/rand"
	"time"
)

type PacePolicy struct {
	BurstCap, PeriodLimit, Concurrency       int
	MinimumCooldown, BaseBackoff, MaxBackoff time.Duration
	QuietStart, QuietEnd                     int
}
type PaceState struct {
	PeriodStarted                time.Time
	Actions, ConsecutiveFailures int
	CooldownUntil                time.Time
	Suspended                    bool
	Warning                      string
}

func (s PaceState) Allow(now time.Time, p PacePolicy) bool {
	if s.Suspended || now.Before(s.CooldownUntil) {
		return false
	}
	hour := now.Hour()
	if p.QuietStart != p.QuietEnd {
		if p.QuietStart < p.QuietEnd && hour >= p.QuietStart && hour < p.QuietEnd {
			return false
		}
		if p.QuietStart > p.QuietEnd && (hour >= p.QuietStart || hour < p.QuietEnd) {
			return false
		}
	}
	if p.BurstCap > 0 && s.Actions >= p.BurstCap {
		return false
	}
	return true
}
func (s PaceState) AfterFailure(now time.Time, p PacePolicy, warning bool) PaceState {
	s.ConsecutiveFailures++
	backoff := p.BaseBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	for i := 1; i < s.ConsecutiveFailures; i++ {
		backoff *= 2
		if p.MaxBackoff > 0 && backoff >= p.MaxBackoff {
			backoff = p.MaxBackoff
			break
		}
	}
	if backoff < p.MinimumCooldown {
		backoff = p.MinimumCooldown
	}
	jitter := time.Duration(rand.Int63n(int64(backoff/10 + 1)))
	s.CooldownUntil = now.Add(backoff + jitter)
	if warning {
		s.Suspended = true
		s.Warning = "environment warning or challenge"
	}
	return s
}
func (s PaceState) AfterSuccess(now time.Time, p PacePolicy) PaceState {
	s.Actions++
	s.ConsecutiveFailures = 0
	s.CooldownUntil = now.Add(p.MinimumCooldown)
	return s
}
