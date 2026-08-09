package runtime

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/plan"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/llm"
)

// noteGather is called from the concurrent executeBatch goroutines (parallel read-only
// tools). Its read-modify-write on the gather map must be mutex-guarded or it's a
// `concurrent map writes` fatal — the crash that took down a read-heavy /plan turn. Run
// under -race; the count must be exact (no lost increments).
func TestNoteGatherConcurrentSafe(t *testing.T) {
	s := &Session{turn: newTurnState(), planCtl: &plan.Controller{}}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.noteGather(tools.ReadFile, raw(map[string]string{"path": "/x/f" + string(rune('a'+i%26)) + ".go"}))
		}(i)
	}
	wg.Wait()
	if s.turn.gather.total != 64 {
		t.Fatalf("all 64 concurrent reads must be counted exactly once, got %d", s.turn.gather.total)
	}
}

func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestGatherSignatureCountsReadsNotProductiveWork(t *testing.T) {
	gather := []struct {
		name  string
		input any
	}{
		{tools.ReadFile, map[string]string{"path": "x.go"}},
		{tools.Ripgrep, map[string]string{"query": "Foo", "path": "pkg/"}},
		{tools.Bash, map[string]string{"command": "grep -n Foo /go/pkg/mod/sdk/message.go"}},
		{tools.Bash, map[string]string{"command": "sed -n '10,20p' /go/pkg/mod/sdk/message.go"}},
		{tools.WebSearch, map[string]string{"query": "anthropic sdk"}},
	}
	for _, c := range gather {
		if _, ok := gatherSignature(c.name, raw(c.input)); !ok {
			t.Errorf("%s should count as gather", c.name)
		}
	}
	productive := []struct {
		name  string
		input any
	}{
		{tools.EditFile, map[string]string{"path": "x.go", "new_string": "y"}},
		{tools.Bash, map[string]string{"command": "go build ./..."}},
		{tools.Bash, map[string]string{"command": "go test ./internal/provider/"}},
		{tools.Bash, map[string]string{"command": "git diff"}},
		{tools.GitDiff, map[string]string{}},
		{tools.AskUser, map[string]string{"question": "which?"}},
	}
	for _, c := range productive {
		if _, ok := gatherSignature(c.name, raw(c.input)); ok {
			t.Errorf("%s must NOT count as gather (it's productive)", c.name)
		}
	}
}

// The same file read with different patterns/line-ranges maps to ONE target — that's the
// re-read the detector must catch.
func TestBashPathsNormalizesSameFile(t *testing.T) {
	a, _ := bashGatherTarget("grep -n OutputConfig /go/pkg/mod/sdk/message.go | head")
	b, _ := bashGatherTarget("sed -n '4200,4240p' /go/pkg/mod/sdk/message.go")
	if a != b {
		t.Fatalf("same file via grep vs sed should be one target: %q vs %q", a, b)
	}
}

// noteGather is telemetry-only: it counts every read — including re-reads of the same target
// far past budget + repeat limit — and NEVER short-circuits. The model decides when it has read
// enough; re-running a read whose output legitimately changed (git status after an edit, a
// re-test) must always work. (The old "skipped — already gathered this" block was removed.)
func TestNoteGatherCountsEveryReadNeverBlocks(t *testing.T) {
	s := &Session{turn: newTurnState(), planCtl: planCtlApplying()} // apply budget = 20
	rd := raw(map[string]string{"command": "grep -n X /sdk/message.go"})
	const hammer = applyGatherBudget + gatherRepeatLimit + 5 // well past every old threshold
	for i := 0; i < hammer; i++ {
		s.noteGather(tools.Bash, rd)
	}
	if s.turn.gather.total != hammer {
		t.Fatalf("every read must be counted, never short-circuited: got %d, want %d", s.turn.gather.total, hammer)
	}
}

// The tooled plan reviewer (read-only + purpose=review) gets its OWN gather mode and a
// bounded-but-generous budget (30–40): enough to verify a large plan's load-bearing claims,
// far above ChatGPT's tiny 6-file suggestion, but not the scout's open-ended sprawl.
func TestReviewGatherBudgetIsBounded(t *testing.T) {
	s := &Session{readOnly: true, purpose: llm.Review, turn: newTurnState()}
	mode, budget := s.gatherMode()
	if mode != "review" {
		t.Fatalf("a read-only review session must be %q mode, got %q", "review", mode)
	}
	if budget < 8 || budget > 20 {
		t.Fatalf("review budget %d must be a tight spot-check range [8,20] — a sanity gate, not a re-audit", budget)
	}
	if budget >= scoutGatherBudget {
		t.Fatalf("review budget %d must be well under the scout budget %d (it's a spot-check)", budget, scoutGatherBudget)
	}
	// A read-only session WITHOUT the review purpose is still the looser scout.
	scout := &Session{readOnly: true, turn: newTurnState()}
	if m, _ := scout.gatherMode(); m != "scout" {
		t.Fatalf("a plain read-only session must stay %q, got %q", "scout", m)
	}
}

func TestGatherBudgetIsTighterForApply(t *testing.T) {
	apply := &Session{planCtl: planCtlApplying()}
	plan := &Session{planCtl: planCtlResearching()}
	if apply.gatherBudget() >= plan.gatherBudget() {
		t.Fatalf("apply budget (%d) must be tighter than plan budget (%d)", apply.gatherBudget(), plan.gatherBudget())
	}
}

// Scout/explorer reads are still COUNTED for telemetry (the gather summary /analyze reads),
// under the looser scout budget — but never blocked, same as the main loop.
func TestScoutGatherIsCountedNeverBlocked(t *testing.T) {
	s := &Session{readOnly: true, turn: newTurnState()}
	if mode, budget := s.gatherMode(); mode != "scout" || budget != scoutGatherBudget {
		t.Fatalf("read-only session must be scout mode w/ scout budget, got %q/%d", mode, budget)
	}
	same := raw(map[string]string{"path": "/x/hammered.go"})
	const hammer = scoutGatherBudget + gatherRepeatLimit + 5
	for i := 0; i < hammer; i++ {
		s.noteGather(tools.ReadFile, same)
	}
	if s.turn.gather.total != hammer {
		t.Fatalf("scout reads must all be counted (never short-circuited): got %d, want %d", s.turn.gather.total, hammer)
	}
}

// summary is the deterministic substrate /analyze reads — lock its shape: re-reads are
// surfaced (count>1), single reads are not, and over-budget / over-repeat are flagged.
func TestGatherSummaryReportsReReads(t *testing.T) {
	g := newGatherState()
	g.total = 45
	g.byTarget = map[string]int{
		"read:/x/message.go": 40, // hammered — the SDK-migration pattern
		"read:/x/router.go":  2,  // a re-read
		"read:/x/once.go":    1,  // first read only — not a re-read
	}
	s := g.summary("apply", applyGatherBudget)

	if s["reads"].(int) != 45 || s["distinct"].(int) != 3 {
		t.Fatalf("reads/distinct wrong: %+v", s)
	}
	if s["over_budget"] != true {
		t.Errorf("45 reads over apply budget %d must be over_budget", applyGatherBudget)
	}
	if s["over_repeat_lim"].(int) != 1 { // only message.go exceeds gatherRepeatLimit
		t.Errorf("over_repeat_lim = %v, want 1", s["over_repeat_lim"])
	}
	rep := s["repeats"].(map[string]int)
	if rep["message.go"] != 40 || rep["router.go"] != 2 {
		t.Errorf("repeats must surface re-read targets by short name: %+v", rep)
	}
	if _, ok := rep["once.go"]; ok {
		t.Error("a target read only once must NOT appear in repeats")
	}
}
