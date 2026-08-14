package catalog

import (
	"testing"
	"time"
)

// Real model ids must each hit their intended rate — a regression guard for the
// substring-drift bug where pinning "gemini-3-pro" silently missed every shipped
// gemini id (gemini-3.1-pro-preview / gemini-3.6-flash / gemini-3.5-flash-lite)
// and dropped them all onto the Flash rate. Match by tier word within the vendor.
func TestModelPricingRealIDs(t *testing.T) {
	cases := []struct {
		id      string
		in, out float64
	}{
		{"gpt-5.6-sol", 5, 30},
		{"gpt-5.6-terra", 2, 12},
		{"gpt-5.6-luna", 0.2, 1.2},
		{"gemini-3.1-pro-preview", 2, 12},   // was mispriced → 1.5/9
		{"gemini-3.6-flash", 0.75, 3.75},    // balanced default (promo through 2026-12-31)
		{"gemini-3.5-flash-lite", 0.3, 2.5}, // was billed 6x high → 1.5/9
		{"grok-4.6", 2, 6},
		{"accounts/fireworks/models/glm-5p2", 1.40, 4.40}, // was $0
		{"accounts/fireworks/models/glm-5p1", 1.40, 4.40},
		{"accounts/fireworks/models/kimi-k3", 3.00, 15.00},       // K3's own Fireworks headline card ($3/$15), NOT the kimi family rule
		{"accounts/fireworks/models/kimi-k2p7-code", 0.95, 4.00}, // pinnable via /model
		{"claude-opus-5", 5, 25},                                 // $5/$25 since Opus 4.5 repricing
		{"claude-fable-5", 10, 50},                               // Fable 5 flagship $10/$50 (explicit entry price)
		{"claude-sonnet-5", 2, 10},                               // explicit sonnet rule (was falling to $3/$15 defaults)
		// Billing-only entries (embeddings/images) — priced so no metered call
		// is ever $0. Same pinned numbers as apps/www ai-models.test.js.
		{"gemini-embedding-001", 0.15, 0},
		{"text-embedding-3-small", 0.02, 0},
		{"gpt-image-1", 5, 40},
		{"gpt-image-2", 8, 30},
	}
	for _, c := range cases {
		p := ModelPricing(c.id)
		if p.Input != c.in || p.Output != c.out {
			t.Errorf("ModelPricing(%q) = %.2f/%.2f, want %.2f/%.2f", c.id, p.Input, p.Output, c.in, c.out)
		}
	}
}

// Bare wire LABELS must price like their raw ids: the gateway sanitizes ids to
// labels ("terra", "glm-5p2") before the CLI's ledger prices them, and the old
// substring table silently dropped "terra"/"sol"/"luna" onto the default sonnet
// rate. The catalog's label index closes that hole.
func TestModelPricingByLabel(t *testing.T) {
	cases := []struct {
		label string
		in    float64
	}{
		{"sol", 5}, {"terra", 2}, {"luna", 0.2},
		{"glm-5p2", 1.40}, {"kimi-k2p6", 0.95},
		{"gemini-flash-lite", 0.3},
	}
	for _, c := range cases {
		if p := ModelPricing(c.label); p.Input != c.in {
			t.Errorf("ModelPricing(%q).Input = %.2f, want %.2f", c.label, p.Input, c.in)
		}
	}
	// Windows resolve by label too (the footer meter sees labels, not raw ids).
	if got := ContextWindow("glm-5p2"); got != 1_000_000 {
		t.Errorf("ContextWindow(label glm-5p2) = %d, want 1M", got)
	}
}

func TestContextWindowFireworks(t *testing.T) {
	cases := []struct {
		id   string
		want int
	}{
		{"accounts/fireworks/models/glm-5p2", 1_000_000}, // was defaulting to 200K
		{"accounts/fireworks/models/glm-5p1", 202_000},
		{"accounts/fireworks/models/kimi-k3", 1_000_000}, // k3 ≠ the kimi-k2 262K case
		{"accounts/fireworks/models/kimi-k2p6", 262_000},
		{"gemini-3.1-pro-preview", 1_000_000},
		{"gemini-3.6-flash", 1_000_000},
	}
	for _, c := range cases {
		if got := ContextWindow(c.id); got != c.want {
			t.Errorf("ContextWindow(%q) = %d, want %d", c.id, got, c.want)
		}
	}
}

// Cache rates: reads default to 0.1x input unless the catalog overrides (grok
// 0.25x, kimi per-tier, glm-5p2 0.1x explicit); writes are always 1.25x input.
// Pinned to the same numbers as apps/www/src/config/__tests__/ai-models.test.js.
func TestModelPricingCacheRates(t *testing.T) {
	cases := []struct {
		id                    string
		cacheRead, cacheWrite float64
	}{
		{"grok-4.6", 0.5, 2.5},
		{"claude-sonnet-5", 0.2, 2.5},
		{"gpt-5.6-terra", 0.2, 2.5},
		{"accounts/fireworks/models/glm-5p2", 0.14, 1.75},
		{"accounts/fireworks/models/kimi-k2p6", 0.16, 1.1875},
		{"gpt-image-2", 0.8, 10},
	}
	for _, c := range cases {
		p := ModelPricing(c.id)
		if p.CacheRead != c.cacheRead || p.CacheWrite != c.cacheWrite {
			t.Errorf("%s cache rates = read %.4f / write %.4f, want %.4f / %.4f",
				c.id, p.CacheRead, p.CacheWrite, c.cacheRead, c.cacheWrite)
		}
	}
}

// Native in-turn web search bills a PER-REQUEST fee upstream on top of tokens —
// the under-debit CostUSD alone never covered. models.json search_fees is USD
// per 1,000 searches keyed by serving vendor (Response.Backend). Pinned to the
// vendor cards verified 2026-08-06: OpenAI Responses web_search $10/1k calls
// (reasoning models), Anthropic web_search_20250305 $10/1k searches, xAI Agent
// Tools web_search $5/1k invocations. Vendors without a native-search fee
// (gemini grounding, the cheap lane, unknown backends) must price 0.
func TestSearchFeeUSD(t *testing.T) {
	cases := []struct {
		vendor   string
		searches int
		want     float64
	}{
		{"anthropic", 1, 0.01},
		{"anthropic", 5, 0.05}, // MaxUses 5 — the per-turn ceiling on the serving wire
		{"openai", 1, 0.01},
		{"openai", 3, 0.03},
		{"grok", 1, 0.005},
		{"grok", 2, 0.01},
		{"gemini", 4, 0}, // grounding is per-prompt with a free tier — not modeled
		{"cheap", 4, 0},  // the cheap lane has no native search
		{"fireworks", 4, 0},
		{"", 3, 0},        // unknown/legacy backend tag
		{"openai", 0, 0},  // no searches, no fee
		{"openai", -1, 0}, // defensive
	}
	for _, c := range cases {
		if got := SearchFeeUSD(c.vendor, c.searches); got != c.want {
			t.Errorf("SearchFeeUSD(%q, %d) = %v, want %v", c.vendor, c.searches, got, c.want)
		}
	}
}

// Sonnet 5's $2/$10 is INTRODUCTORY pricing through 2026-08-31
// (platform.claude.com/docs pricing, verified 2026-07-27) — $3/$15 takes
// effect 2026-09-01. This test is a deliberate time bomb: the moment the
// deadline passes, it fails until the root /models.json sonnet family rule
// (and the www pinned table) are flipped. Do NOT "fix" this test by changing
// the dates — fix the catalog.
func TestSonnet5IntroPricingDeadline(t *testing.T) {
	cutover := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	wantIn, wantOut := 2.0, 10.0
	if !time.Now().Before(cutover) {
		wantIn, wantOut = 3.0, 15.0
	}
	p := ModelPricing("claude-sonnet-5")
	if p.Input != wantIn || p.Output != wantOut {
		t.Fatalf("claude-sonnet-5 = %.2f/%.2f, want %.2f/%.2f — Sonnet 5 intro pricing ended 2026-09-01: update the sonnet family rule in the ROOT /models.json (copy it to catalog/models.json) and the pinned table in pricing_test.go", p.Input, p.Output, wantIn, wantOut)
	}
}
