package runtime

import (
	"io"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/compaction"
	"github.com/memcode-ai/memcode/internal/agent/plan"
	"github.com/memcode-ai/memcode/internal/wire"
)

func evictFixture(turns int) []wire.Message {
	big := strings.Repeat("z", 12_000) // ~3k est tokens per result
	var msgs []wire.Message
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "do a thing"}}},
			wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "t", Name: "ripgrep", Input: []byte(`{"query":"q"}`)}}},
			wire.Message{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: "t", Content: big}}},
			wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "text", Text: "done"}}},
		)
	}
	return msgs
}

// TestEvictOnTurnStart: below the trigger nothing changes; above it, stale tool
// dumps from earlier turns become pointers while the newest K stay raw. The
// budget is pinned via env so the fixture sizes stay meaningful (trigger and
// keep counts scale with the budget since the real-windows change).
func TestEvictOnTurnStart(t *testing.T) {
	t.Setenv("MEMCODE_COMPACT_BUDGET", "45000") // trigger 22.5k, keep floor 8
	s := &Session{out: io.Discard, turn: newTurnState(), planCtl: &plan.Controller{}}

	small := evictFixture(2) // ~6k tokens — under the 20k trigger
	before := compaction.EstimateTokens(small)
	s.evictOnTurnStart(&small)
	if got := compaction.EstimateTokens(small); got != before {
		t.Errorf("under-trigger transcript must be untouched: %d -> %d", before, got)
	}

	big := evictFixture(12) // 12 results ≈ 36k tokens — over the trigger; the 4 oldest evict
	before = compaction.EstimateTokens(big)
	s.evictOnTurnStart(&big)
	after := compaction.EstimateTokens(big)
	// 4 evicted results × ~3k tokens each ≈ 12k reclaimed.
	if after > before-10_000 {
		t.Errorf("stale dumps should collapse: %d -> %d", before, after)
	}
	raw := 0
	for _, m := range big {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" && strings.Contains(b.Content, "zzzz") {
				raw++
			}
		}
	}
	if want := keepToolResults(45_000); raw != want {
		t.Errorf("newest %d results should stay raw, got %d", want, raw)
	}
}

// TestKeepCountsScaleWithBudget locks the scaling: ~1 kept tool output per 10K
// budget, floored at 8 (the old constant), capped at 32 — so a 250K-budget
// session keeps a 9-10-file working set resident instead of thrashing it.
func TestKeepCountsScaleWithBudget(t *testing.T) {
	cases := []struct{ budget, want int }{
		{0, 8}, {45_000, 8}, {80_000, 8}, {150_000, 15}, {250_000, 25}, {1_000_000, 32},
	}
	for _, c := range cases {
		if got := keepToolResults(c.budget); got != c.want {
			t.Errorf("keepToolResults(%d) = %d, want %d", c.budget, got, c.want)
		}
	}
}

// TestCompactBudgetFollowsWindow: budgets are RELATIVE — the learned lane
// capacity ×85% once the gateway teaches it, else the serving model's catalog
// window ×80%. No absolute token constants (every prior magic number aged into
// a silent clip); an optional env soft cap lowers the ceiling for cost-capped
// setups.
func TestCompactBudgetFollowsWindow(t *testing.T) {
	t.Setenv("MEMCODE_COMPACT_BUDGET", "")
	t.Setenv("MEMCODE_CONTEXT_SOFT_CAP", "")
	s := &Session{out: io.Discard, turn: newTurnState(), planCtl: &plan.Controller{}, model: "glm-5p2"}

	// Nothing learned → the MODEL's window governs (glm-5p2 = 1M → 800K).
	if got, want := s.compactBudget(), 1_000_000*windowFallbackPct/100; got != want {
		t.Fatalf("pre-learning budget = %d, want %d (window-relative)", got, want)
	}
	// Learned 1M lane (960K usable) → the lane governs: 816K, no built-in clip.
	s.recordServed(func(v *servedState) { v.inputBudget = 960_000 })
	if got, want := s.compactBudget(), 960_000*compactBudgetPct/100; got != want {
		t.Fatalf("lane budget = %d, want %d (the whole window works)", got, want)
	}
	// A smaller lane governs too.
	s.recordServed(func(v *servedState) { v.inputBudget = 162_000 })
	if got, want := s.compactBudget(), 162_000*compactBudgetPct/100; got != want {
		t.Fatalf("small-lane budget = %d, want %d", got, want)
	}
	// The OPTIONAL env soft cap lowers the ceiling when set.
	t.Setenv("MEMCODE_CONTEXT_SOFT_CAP", "120000")
	s.recordServed(func(v *servedState) { v.inputBudget = 960_000 })
	if got, want := s.compactBudget(), 120_000*compactBudgetPct/100; got != want {
		t.Fatalf("env-capped budget = %d, want %d", got, want)
	}
}

// TestEarlyEvictTriggerTracksBudget: the cross-turn eviction trigger is half the
// active budget (preserving the historical 20K/45K ratio), not a fixed 20K.
func TestEarlyEvictTriggerTracksBudget(t *testing.T) {
	t.Setenv("MEMCODE_COMPACT_BUDGET", "45000")
	s := &Session{out: io.Discard, turn: newTurnState(), planCtl: &plan.Controller{}}
	if got := s.earlyEvictTrigger(); got != 22_500 {
		t.Fatalf("trigger at 45K budget = %d, want 22500", got)
	}
	t.Setenv("MEMCODE_COMPACT_BUDGET", "250000")
	if got := s.earlyEvictTrigger(); got != 125_000 {
		t.Fatalf("trigger at 250K budget = %d, want 125000", got)
	}
}

// TestEvictOnTurnStartPlanModeNoop: research stays raw for synthesis.
func TestEvictOnTurnStartPlanModeNoop(t *testing.T) {
	s := &Session{out: io.Discard, turn: newTurnState(), planCtl: planCtlResearching()}
	big := evictFixture(12)
	before := compaction.EstimateTokens(big)
	s.evictOnTurnStart(&big)
	if got := compaction.EstimateTokens(big); got != before {
		t.Errorf("plan mode must not evict: %d -> %d", before, got)
	}
}
