package runtime

import (
	"fmt"
	"io"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/plan"
)

// TestHotPathsDecayAcrossTurns: a path re-read in one turn becomes hot (pinned);
// after two quiet turns its score decays away and the pin expires.
func TestHotPathsDecayAcrossTurns(t *testing.T) {
	s := &Session{out: io.Discard, turn: newTurnState(), planCtl: &plan.Controller{}}

	// Turn 1: auth.go read 3x → hot; single reads and non-read targets don't pin.
	s.noteHotPaths(map[string]int{"read:auth.go": 3, "read:once.go": 1, "rg:auth.go": 5})
	pin := s.pinnedPredicate()
	if pin == nil || !pin("auth.go") {
		t.Fatal("a 3x re-read path must be pinned")
	}
	if pin("once.go") || pin("auth.go2") {
		t.Fatal("single reads and non-paths must not be pinned")
	}

	// Two quiet turns: 3 → 1 → gone.
	s.noteHotPaths(nil)
	if pin := s.pinnedPredicate(); pin != nil && pin("auth.go") {
		t.Fatal("after one quiet turn the score should have decayed below the pin threshold")
	}
	s.noteHotPaths(nil)
	if s.pinnedPredicate() != nil {
		t.Fatalf("after two quiet turns the hot set should be empty, got %v", s.hotPaths)
	}
}

// TestPinnedSetIsCapped: the hot set never exceeds hotPathsCap — pins protect a
// working set, they must not quietly defeat the budget.
func TestPinnedSetIsCapped(t *testing.T) {
	s := &Session{out: io.Discard, turn: newTurnState(), planCtl: &plan.Controller{}}
	counts := map[string]int{}
	for i := 0; i < hotPathsCap+8; i++ {
		counts[fmt.Sprintf("read:f%02d.go", i)] = 2 + i // increasing heat
	}
	s.noteHotPaths(counts)
	if len(s.hotPaths) != hotPathsCap {
		t.Fatalf("hot set = %d entries, want cap %d", len(s.hotPaths), hotPathsCap)
	}
	// The hottest survived the cap; the coldest were dropped.
	if _, ok := s.hotPaths[fmt.Sprintf("f%02d.go", hotPathsCap+7)]; !ok {
		t.Fatal("the hottest path must survive the cap")
	}
	if _, ok := s.hotPaths["f00.go"]; ok {
		t.Fatal("the coldest path should have been dropped at the cap")
	}
}

// TestCompactBackoffSuppressesRepeatPasses: after a pass that could not get
// under budget, compactWouldHelp suppresses re-compaction until the history has
// really regrown (≥20%); manual /compact resets the baseline.
func TestCompactBackoffSuppressesRepeatPasses(t *testing.T) {
	s := &Session{out: io.Discard, turn: newTurnState(), planCtl: &plan.Controller{}}

	if !s.compactWouldHelp(50_000) {
		t.Fatal("no baseline yet — the first pass must be allowed")
	}
	s.lastCompactAfter = 60_000 // last pass landed ABOVE a 45K budget (ineffective)
	if s.compactWouldHelp(61_000) {
		t.Fatal("barely past the last result — re-compacting would reproduce it; must back off")
	}
	if !s.compactWouldHelp(72_000) {
		t.Fatal("≥20% real regrowth — compaction is worth another pass")
	}
	s.lastCompactAfter = 0 // manual /compact resets
	if !s.compactWouldHelp(61_000) {
		t.Fatal("after a manual reset the next pass must be allowed")
	}
}
