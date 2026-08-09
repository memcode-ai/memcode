package runtime

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

// TestLoopIterCap: the apply phase (executing an approved plan) gets the HIGH ceiling like
// allow-all — not the 200 soft cap that stranded big plans mid-execution. Plain auto/ask keep the
// soft cap; an explicit per-session override wins.
func TestLoopIterCap(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "sonnet", permissions.ModeAuto, io.Discard)

	if got := s.loopIterCap(); got != maxIterations {
		t.Fatalf("plain auto turn should keep the soft cap %d, got %d", maxIterations, got)
	}

	// Apply phase: not in plan mode, Applying set → high ceiling, even in auto.
	armApplyForTest(s, "1. step one\n2. step two")
	if got := s.loopIterCap(); got != maxIterationsYolo {
		t.Fatalf("the apply phase must get the high ceiling %d (a long approved run), got %d", maxIterationsYolo, got)
	}
	s.planCtl.ApplyDone()

	// allow-all also gets the high ceiling.
	s.mode = permissions.ModeAllowAll
	if got := s.loopIterCap(); got != maxIterationsYolo {
		t.Fatalf("allow-all should get the high ceiling, got %d", got)
	}
	s.mode = permissions.ModeAuto

	// An explicit per-session override wins over everything.
	s.iterCap = 7
	if got := s.loopIterCap(); got != 7 {
		t.Fatalf("explicit iterCap override should win, got %d", got)
	}
}
