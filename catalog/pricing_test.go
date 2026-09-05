package catalog

import (
	"math"
	"testing"
)

// Real model ids must each hit their intended rate — a regression guard for the
// substring-drift bug where pinning "gemini-3-pro" silently missed every shipped
// gemini id (gemini-3.1-pro-preview / gemini-3.8-flash / gemini-3.5-flash-lite)
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
		{"gemini-3.8-flash", 0.75, 3.75},    // balanced default (promo through 2026-12-31)
		{"gemini-3.5-flash-lite", 0.3, 2.5}, // was billed 6x high → 1.5/9
		{"grok-4.6", 2, 6},
		{"accounts/fireworks/models/glm-5p2", 1.40, 4.40}, // was $0
		{"accounts/fireworks/models/glm-5p1", 1.40, 4.40},
		{"accounts/fireworks/models/kimi-k3", 3.00, 15.00}, // K3's own Fireworks headline card ($3/$15), NOT the kimi family rule
		{"accounts/fireworks/models/qwen3p8-2p4t-a95b", 2.00, 6.00},
		{"accounts/fireworks/models/deepseek-v4-pro-0813", 1.32, 3.96},
		{"accounts/fireworks/models/deepseek-v4-flash-0731", 0.22, 0.66},
		{"claude-opus-5", 5, 25},   // $5/$25 since Opus 4.5 repricing
		{"claude-fable-5", 10, 50}, // Fable 5 flagship $10/$50 (explicit entry price)
		{"claude-sonnet-5", 2, 10}, // explicit sonnet rule (was falling to $3/$15 defaults)
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
		{"glm-5p2", 1.40}, {"kimi-k3", 3.00}, {"qwen3p8-max", 2.00},
		{"deepseek-v4-pro", 1.32}, {"deepseek-v4-flash", 0.22},
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
	if got := ContextWindow("qwen3p8-max"); got != 262_144 {
		t.Errorf("ContextWindow(label qwen3p8-max) = %d, want 262144", got)
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
		{"accounts/fireworks/models/kimi-k3", 1_000_000},
		{"accounts/fireworks/models/qwen3p8-2p4t-a95b", 262_144},
		{"accounts/fireworks/models/deepseek-v4-pro-0813", 1_040_000},
		{"accounts/fireworks/models/deepseek-v4-flash-0731", 1_040_000},
		{"gemini-3.1-pro-preview", 1_000_000},
		{"gemini-3.8-flash", 1_000_000},
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
		{"accounts/fireworks/models/kimi-k3", 0.30, 3.75},
		{"accounts/fireworks/models/qwen3p8-2p4t-a95b", 0.2, 2.5},
		{"accounts/fireworks/models/deepseek-v4-pro-0813", 0.044, 1.32 * 1.25},
		{"accounts/fireworks/models/deepseek-v4-flash-0731", 0.007, 0.275},
		{"gpt-image-2", 0.8, 10},
	}
	for _, c := range cases {
		p := ModelPricing(c.id)
		if p.CacheRead != c.cacheRead || math.Abs(p.CacheWrite-c.cacheWrite) > 1e-12 {
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

// Sonnet 5 is $2/$10. This was a time bomb asserting a flip to $3/$15 on
// 2026-09-01, written when the launch announcement called $2/$10
// introductory through 2026-08-31. The increase was CANCELLED:
// platform.claude.com/docs/en/about-claude/pricing now states that rate "is
// now the standard price" and that "the previously scheduled increase to
// $3/$15 ... will not occur" (verified 2026-09-02). Obeying the old failure
// message would have raised the catalog 50% above provider cost, which the
// credits system bills through at exact cost — an overcharge, not a fix.
func TestSonnet5Pricing(t *testing.T) {
	p := ModelPricing("claude-sonnet-5")
	if p.Input != 2.0 || p.Output != 10.0 {
		t.Fatalf("claude-sonnet-5 = %.2f/%.2f, want 2.00/10.00", p.Input, p.Output)
	}
}

// Prompt-size tiers are generic mechanism, not an Astra special case: the base
// card holds under the threshold, every declared multiplier applies over it, and
// the bend covers the WHOLE request rather than the tokens past the line.
func TestPriceTiers(t *testing.T) {
	base := Pricing{Input: 10, Output: 50, CacheWrite: 12.5, CacheRead: 1}
	tiers := []PriceTier{{AbovePromptTokens: 272000, In: 2, Out: 1.5, CacheRead: 2, CacheWrite: 2}}

	if got := applyTiers(base, tiers, 272000); got != base {
		t.Errorf("at the threshold = %+v, want base %+v (strictly ABOVE bends)", got, base)
	}
	want := Pricing{Input: 20, Output: 75, CacheWrite: 25, CacheRead: 2}
	if got := applyTiers(base, tiers, 272001); got != want {
		t.Errorf("over the threshold = %+v, want %+v", got, want)
	}
	if got := applyTiers(base, nil, 10_000_000); got != base {
		t.Errorf("no tiers = %+v, want base %+v", got, base)
	}
}

// An omitted multiplier means 1x, so a tier states only what it bends.
func TestPriceTierPartialMultipliers(t *testing.T) {
	base := Pricing{Input: 10, Output: 50, CacheWrite: 12.5, CacheRead: 1}
	got := applyTiers(base, []PriceTier{{AbovePromptTokens: 100, Out: 3}}, 200)
	want := Pricing{Input: 10, Output: 150, CacheWrite: 12.5, CacheRead: 1}
	if got != want {
		t.Errorf("partial tier = %+v, want %+v", got, want)
	}
}

// Several bands are allowed and independent: the highest one the prompt clears
// wins, regardless of the order they appear in JSON.
func TestPriceTierHighestWinsRegardlessOfOrder(t *testing.T) {
	base := Pricing{Input: 10}
	tiers := []PriceTier{{AbovePromptTokens: 1_000_000, In: 4}, {AbovePromptTokens: 200_000, In: 2}}
	for _, c := range []struct {
		prompt int
		want   float64
	}{{100_000, 10}, {300_000, 20}, {2_000_000, 40}} {
		if got := applyTiers(base, tiers, c.prompt).Input; got != c.want {
			t.Errorf("prompt %d = %v, want %v", c.prompt, got, c.want)
		}
	}
}

// The catalog's Astra entry must actually carry the 272K band, and CostUSD must
// route a long request through it — this is the money path, not just the struct.
func TestAstraLongPromptCost(t *testing.T) {
	if p := ModelPricing("gpt-6-astra"); p.Input != 10 || p.Output != 50 || p.CacheRead != 1 {
		t.Fatalf("base card = %+v, want 10/50 with cache read 1", p)
	}
	// 300K prompt clears 272K: input 2x, output 1.5x.
	got := CostUSD("gpt-6-astra", 300_000, 1_000, 0, 0)
	want := (300_000*20.0 + 1_000*75.0) / 1e6
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("long-prompt cost = %v, want %v", got, want)
	}
	// Same model, short prompt, base rates.
	short := CostUSD("gpt-6-astra", 1_000, 1_000, 0, 0)
	if wantShort := (1_000*10.0 + 1_000*50.0) / 1e6; math.Abs(short-wantShort) > 1e-9 {
		t.Errorf("short-prompt cost = %v, want %v", short, wantShort)
	}
}

// The threshold measures the whole prompt, so cache reads and writes count
// toward it — billing a 280K prompt at base rates just because most of it was
// cached is exactly the underbill this mechanism exists to prevent.
func TestPriceTierThresholdCountsCachedPrompt(t *testing.T) {
	// 10K fresh + 280K cache read = 290K prompt → over the line.
	got := CostUSD("gpt-6-astra", 10_000, 0, 280_000, 0)
	want := (10_000*20.0 + 280_000*2.0) / 1e6
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cached long prompt = %v, want %v", got, want)
	}
}

// The effort floor is a catalog FACT, not an adapter special case: Astra rejects
// "none", GPT-5.6 accepts it, and a model that declares no floor reports "".
func TestMinReasoningEffort(t *testing.T) {
	if got := MinReasoningEffort("gpt-6-astra"); got != "low" {
		t.Errorf("astra floor = %q, want \"low\"", got)
	}
	if got := MinReasoningEffort("gpt-5.6-sol"); got != "" {
		t.Errorf("sol floor = %q, want \"\" (no floor)", got)
	}
	if got := MinReasoningEffort("some-model-nobody-added"); got != "" {
		t.Errorf("unknown floor = %q, want \"\"", got)
	}
}
