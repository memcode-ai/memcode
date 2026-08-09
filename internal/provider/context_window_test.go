package provider

import (
	"testing"
	"time"

	"github.com/memcode-ai/memcode/catalog"
)

func TestContextWindow(t *testing.T) {
	cases := map[string]int{
		// Current Claude tiers are 1M NATIVELY (no beta header / variant) — the bare
		// ids resolve to 1M, not the stale 200K. Haiku stays 200K; unknown ids default
		// to the safe 200K baseline. The legacy [1m]/-1m suffixes still resolve to 1M.
		"claude-opus-5":        1_000_000,
		"claude-opus-5[1m]":    1_000_000,
		"claude-sonnet-5":      1_000_000,
		"claude-sonnet-4-6":    1_000_000,
		"claude-haiku-4-5":     200_000,
		"some-unknown-model":   200_000, // safe default
		"claude-sonnet-4-6-1m": 1_000_000,
	}
	for model, want := range cases {
		if got := catalog.ContextWindow(model); got != want {
			t.Errorf("ContextWindow(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestCostUSD(t *testing.T) {
	// 1M output tokens on opus ($25/Mtok) = $25; sonnet ($10) = $10; haiku ($5) = $5.
	// (Opus 5 $5/$25 and Sonnet 5 $2/$10 per the published cards, 2026-07-27.)
	if got := catalog.CostUSD("claude-opus-5", 0, 1_000_000, 0, 0); got != 25 {
		t.Errorf("opus 1M output = $%.2f, want $25", got)
	}
	// Sonnet intro pricing ($10/M out) ends 2026-08-31; $15/M from Sept 1.
	// Date-aware so this stays consistent with the catalog time bomb in
	// common/pricing_test.go (TestSonnet5IntroPricingDeadline).
	wantSonnetOut := 10.0
	if !time.Now().Before(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		wantSonnetOut = 15.0
	}
	if got := catalog.CostUSD("claude-sonnet-5", 0, 1_000_000, 0, 0); got != wantSonnetOut {
		t.Errorf("sonnet-5 1M output = $%.2f, want $%.2f", got, wantSonnetOut)
	}
	if got := catalog.CostUSD("claude-sonnet-4-6", 0, 1_000_000, 0, 0); got != wantSonnetOut {
		t.Errorf("sonnet 1M output = $%.2f, want $%.2f", got, wantSonnetOut)
	}
	if got := catalog.CostUSD("claude-haiku-4-5", 0, 1_000_000, 0, 0); got != 5 {
		t.Errorf("haiku 1M output = $%.2f, want $5", got)
	}
	// Cache read is ~10% of input: 1M cache-read on sonnet ($2 input) ≈ $0.20.
	wantCache := wantSonnetOut / 50 // 0.1x input; input = out/5 on both cards
	if got := catalog.CostUSD("claude-sonnet-5", 0, 0, 1_000_000, 0); got < wantCache-0.001 || got > wantCache+0.001 {
		t.Errorf("sonnet-5 1M cache-read = $%.4f, want ≈$%.2f", got, wantCache)
	}
	if got := catalog.CostUSD("claude-sonnet-4-6", 0, 0, 1_000_000, 0); got < wantCache-0.001 || got > wantCache+0.001 {
		t.Errorf("sonnet 1M cache-read = $%.4f, want ≈$%.2f", got, wantCache)
	}
	// Unknown model falls back to sonnet rates.
	if catalog.CostUSD("mystery", 0, 1_000_000, 0, 0) != 15 {
		t.Error("unknown model should fall back to sonnet rates")
	}
}
