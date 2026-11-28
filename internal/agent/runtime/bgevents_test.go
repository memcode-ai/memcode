package runtime

import (
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// A background result posted mid-turn folds into the running turn once, framed as a
// runtime event (never as user input), and is gone from the pending list afterwards.
func TestBackgroundResultsFoldIntoRunningTurn(t *testing.T) {
	s := &Session{}
	s.PostBackground("[background agent job_1 finished] task: eval\n\nResult:\nok")
	s.PostBackground("   ") // blank results are dropped
	s.PostBackground("background shell finished: go test ./... exit 0")

	var msgs []wire.Message
	if n := s.foldBackground(&msgs); n != 2 {
		t.Fatalf("folded %d results, want 2", n)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("want one appended message, got %+v", msgs)
	}
	text := msgs[0].Blocks[0].Text
	for _, want := range []string{"not from the user", "job_1 finished", "go test ./... exit 0"} {
		if !strings.Contains(text, want) {
			t.Errorf("folded note missing %q:\n%s", want, text)
		}
	}
	if n := s.foldBackground(&msgs); n != 0 || len(msgs) != 1 {
		t.Errorf("second fold delivered again: n=%d msgs=%d", n, len(msgs))
	}
}

// Results the running turn never reached are handed back to the frontend at turn end.
func TestBackgroundLeftoversTakenAtTurnEnd(t *testing.T) {
	s := &Session{}
	s.PostBackground("late result")
	if got := s.TakeBackground(); len(got) != 1 || got[0] != "late result" {
		t.Fatalf("TakeBackground = %q", got)
	}
	if got := s.TakeBackground(); len(got) != 0 {
		t.Errorf("leftovers delivered twice: %q", got)
	}
}
