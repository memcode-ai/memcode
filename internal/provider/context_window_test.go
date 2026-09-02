package provider

import (
	"testing"

	"github.com/memcode-ai/memcode/catalog"
)

func TestContextWindow(t *testing.T) {
	cases := map[string]int{
		// Current Claude tiers are 1M NATIVELY (no beta header / variant) — the bare
		// ids resolve to 1M, not the stale 200K. Haiku stays 200K; unknown ids default
		// to the safe 200K baseline. The legacy [1m]/-1m suffixes still resolve to 1M.
		"claude-opus-5":      1_000_000,
		"claude-opus-5[1m]":  1_000_000,
		"claude-sonnet-5":    1_000_000,
		"claude-haiku-4-5":   200_000,
		"some-unknown-model": 200_000, // safe default
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
	// Sonnet 5 is $2/$10. The launch announcement called that introductory
	// through 2026-08-31 with a flip to $3/$15, and this assertion used to be
	// date-aware for it; the increase was CANCELLED (platform.claude.com
	// pricing, verified 2026-09-02), so it is a flat pin now.
	if got := catalog.CostUSD("claude-sonnet-5", 0, 1_000_000, 0, 0); got != 10 {
		t.Errorf("sonnet-5 1M output = $%.2f, want $10", got)
	}
	if got := catalog.CostUSD("claude-haiku-4-5", 0, 1_000_000, 0, 0); got != 5 {
		t.Errorf("haiku 1M output = $%.2f, want $5", got)
	}
	// Cache read is 0.1x input: sonnet-5 $2 input -> $0.20.
	if got := catalog.CostUSD("claude-sonnet-5", 0, 0, 1_000_000, 0); got < 0.199 || got > 0.201 {
		t.Errorf("sonnet-5 1M cache-read = $%.4f, want $0.20", got)
	}
	// Fable 5.1 reads cache at 0.025x, not 0.1x: $10 input -> $0.25.
	if got := catalog.CostUSD("claude-fable-5-1", 0, 0, 1_000_000, 0); got < 0.249 || got > 0.251 {
		t.Errorf("fable-5-1 1M cache-read = $%.4f, want $0.25", got)
	}
	// Unknown model falls back to sonnet rates.
	if catalog.CostUSD("mystery", 0, 1_000_000, 0, 0) != 15 {
		t.Error("unknown model should fall back to sonnet rates")
	}
}
