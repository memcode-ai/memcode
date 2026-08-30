package autonomy

import (
	"testing"
	"time"
)

func TestPacingBurstCooldownBackoffAndWarningSuspension(t *testing.T) {
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	p := PacePolicy{BurstCap: 2, MinimumCooldown: time.Minute, BaseBackoff: time.Second, MaxBackoff: time.Hour, QuietStart: 22, QuietEnd: 6}
	s := PaceState{}
	if !s.Allow(now, p) {
		t.Fatal("initial action denied")
	}
	s = s.AfterSuccess(now, p)
	if s.Allow(now.Add(30*time.Second), p) {
		t.Fatal("cooldown ignored")
	}
	s.CooldownUntil = now
	s.Actions = 2
	if s.Allow(now, p) {
		t.Fatal("burst cap ignored")
	}
	s = PaceState{}.AfterFailure(now, p, false)
	first := s.CooldownUntil
	if !first.After(now) {
		t.Fatal("backoff missing")
	}
	s = s.AfterFailure(now, p, true)
	if !s.Suspended || s.Warning == "" {
		t.Fatal("warning did not suspend")
	}
	quiet := time.Date(2026, time.August, 30, 23, 0, 0, 0, time.UTC)
	if (PaceState{}).Allow(quiet, p) {
		t.Fatal("quiet hours ignored")
	}
}
