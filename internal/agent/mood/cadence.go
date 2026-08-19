package mood

import (
	"strings"
	"sync"
	"time"
)

// Cadence is the timing shape of a turn — a subtle but real interaction signal.
// Three short messages fired off in five seconds is a different state than one
// calm, considered prompt. We capture only DERIVED aggregates (never raw
// keystrokes — that gets creepy fast): the gap since the last turn, size, and a
// few booleans the runtime uses for routing + friction.
type Cadence struct {
	InterMessageMs  int64 `json:"inter_message_ms"`           // gap since the previous turn (0 = first)
	Chars           int   `json:"chars"`                      // message length
	Words           int   `json:"words"`                      //
	Burst           bool  `json:"burst,omitempty"`            // arrived right after the previous turn
	RapidCorrection bool  `json:"rapid_correction,omitempty"` // a quick corrective follow-up → repair
	LongPause       bool  `json:"long_pause,omitempty"`       // big gap → possibly a new topic
}

// CadenceTracker derives per-turn timing features from message arrival times.
type CadenceTracker struct {
	mu   sync.Mutex // the engine goroutine and the TUI both touch the tracker (same posture as Tracker)
	last time.Time
	now  func() time.Time
}

// NewCadenceTracker returns a tracker using the wall clock.
func NewCadenceTracker() *CadenceTracker { return &CadenceTracker{now: time.Now} }

// thresholds for cadence classification.
const (
	burstWindow    = 3 * time.Second
	rapidWindow    = 6 * time.Second
	longPauseAfter = 60 * time.Second
)

// Observe records a turn's arrival and returns its cadence. corrective signals
// the turn is a correction/negation (from routing or friction), so a quick
// follow-up is flagged as a rapid correction (a strong "we're off track" cue).
func (c *CadenceTracker) Observe(text string, corrective bool) Cadence {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now()
	cad := Cadence{Chars: len(text), Words: len(strings.Fields(text))}
	if !c.last.IsZero() {
		gap := t.Sub(c.last)
		if gap < 0 {
			gap = 0
		}
		cad.InterMessageMs = gap.Milliseconds()
		cad.Burst = gap <= burstWindow
		cad.RapidCorrection = corrective && gap <= rapidWindow
		cad.LongPause = gap >= longPauseAfter
	}
	c.last = t
	return cad
}
