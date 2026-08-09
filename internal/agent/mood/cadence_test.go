package mood

import (
	"testing"
	"time"
)

func TestCadence(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &CadenceTracker{now: func() time.Time { return now }}

	// First turn: no gap.
	first := c.Observe("hello there", false)
	if first.InterMessageMs != 0 || first.Words != 2 || first.Chars != 11 {
		t.Fatalf("first cadence = %+v", first)
	}

	// A quick corrective follow-up → burst + rapid correction.
	now = now.Add(2 * time.Second)
	quick := c.Observe("no that's wrong", true)
	if !quick.Burst || !quick.RapidCorrection {
		t.Fatalf("expected burst + rapid correction, got %+v", quick)
	}
	if quick.InterMessageMs != 2000 {
		t.Fatalf("inter-message = %d, want 2000", quick.InterMessageMs)
	}

	// A long pause, not corrective → long pause, no burst/rapid.
	now = now.Add(90 * time.Second)
	slow := c.Observe("ok now let's think about the architecture", false)
	if slow.Burst || slow.RapidCorrection {
		t.Fatalf("did not expect burst/rapid after a long pause: %+v", slow)
	}
	if !slow.LongPause {
		t.Fatalf("expected long pause, got %+v", slow)
	}

	// Quick but NOT corrective → burst without rapid correction.
	now = now.Add(1 * time.Second)
	q2 := c.Observe("and also add tests", false)
	if !q2.Burst || q2.RapidCorrection {
		t.Fatalf("expected burst without rapid-correction: %+v", q2)
	}
}
