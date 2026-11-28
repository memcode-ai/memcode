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
		{"gpt-5.6-sol", 2, 10}, // alias -> GPT-6.2 Sol
		{"gpt-6-sol", 2, 10},   // alias -> GPT-6.2 Sol
		{"gpt-6.2-sol", 2, 10},
		{"gpt-6.2-astra", 8, 40},
		{"gpt-6.1-terra", 1, 5},
		{"gpt-6-luna", 0.1, 0.5},
		{"gpt-5.6-terra", 1, 5},             // alias -> GPT-6.1 Terra
		{"gpt-5.6-luna", 0.1, 0.5},          // alias -> GPT-6 Luna
		{"gpt-6-astra", 8, 40},              // alias -> GPT-6.2 Astra
		{"gemini-4-carbon", 4, 20},          // standing card from 2026-12-01
		{"gemini-4-argon", 4, 20},           // fast frontier reasoning
		{"gemini-4-neon", 3, 15},            // fast end-to-end frontier
		{"gemini-3.1-pro-preview", 4, 20},   // alias -> Gemini 4 Carbon
		{"gemini-4-hydrogen", 1.5, 7.5},     // the Gemini everyday model
		{"gemini-3.8-flash", 1.5, 7.5},      // standalone standing card
		{"gemini-3.6-flash", 1.5, 7.5},      // alias -> Gemini 4 Hydrogen
		{"gemini-3.5-flash-lite", 0.3, 2.5}, // was billed 6x high → 1.5/9
		{"grok-4.7", 2, 6},
		{"grok-4.6", 2, 6},                                // alias -> 4.7, same card
		{"accounts/fireworks/models/glm-5p2", 1.40, 4.40}, // was $0
		{"accounts/fireworks/models/glm-5p1", 1.40, 4.40},
		{"accounts/fireworks/models/kimi-k3p1-pro", 2.00, 10.00}, // its own Fireworks card, NOT the kimi family rule
		{"accounts/fireworks/models/kimi-k3", 2.00, 10.00},       // alias -> K3.1 Pro
		{"accounts/fireworks/models/qwen3p8-max", 2.00, 6.00},
		{"accounts/fireworks/models/qwen3p8-2p4t-a95b", 2.00, 6.00}, // legacy path
		// Decommissioned 2026-09-25 and aliased onto V4.1 Flash, so they price as Flash.
		{"accounts/fireworks/models/deepseek-v4-pro-0813", 0.30, 1.20},
		{"deepseek-v4-pro", 0.30, 1.20},
		{"accounts/fireworks/models/deepseek-v4p1-flash", 0.30, 1.20}, // alias -> the 1017 build
		{"accounts/fireworks/models/deepseek-v4p1-flash-1017", 0.30, 1.20},
		// Superseded but still deployed on Fireworks: without its own rate row this
		// falls to the $1.40/$4.40 Fireworks floor and bills 6x.
		{"accounts/fireworks/models/deepseek-v4-flash-0731", 0.30, 1.20},
		{"accounts/fireworks/models/glm-5p3", 1.40, 4.40},
		{"accounts/fireworks/models/glm-5p3-flash", 0.15, 0.50},
		{"claude-opus-5-5", 4, 20},              // Opus 5.5 is cheaper than Opus 5
		{"claude-opus-5", 4, 20},                // alias -> Opus 5.5
		{"claude-opus-4-8", 5, 25},              // legacy Opus, family rule
		{"claude-fable-5", 10, 50},              // legacy Fable 5, fable family rule
		{"claude-fable-5-2", 8, 40},             // Fable 5.2 is cheaper than 5.1
		{"claude-fable-5-1", 8, 40},             // alias -> Fable 5.2
		{"claude-sonnet-5-5", 2, 10},            // same card as Sonnet 5
		{"claude-sonnet-5", 2, 10},              // alias -> Sonnet 5.5
		{"claude-haiku-5-5", 0.1, 0.5},          // prompts up to 100K
		{"claude-haiku-4-5-20251001", 0.1, 0.5}, // alias -> Haiku 5.5
		// Billing-only entries (embeddings/images) — priced so no metered call
		// is ever $0. Same pinned numbers as apps/www ai-models.test.js.
		{"gemini-embedding-2", 0.2, 0},
		{"gemini-embedding-001", 0.2, 0}, // alias -> embedding 2
		{"text-embedding-3-small", 0.02, 0},
		{"gpt-image-1", 8, 30}, // shut down 2026-10-23, alias -> gpt-image-2
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
		{"astra", 8}, {"sol", 2}, {"terra", 1}, {"luna", 0.1}, {"opus", 4}, {"grok-4.6", 2},
		{"glm-5p2", 1.40}, {"kimi-k3", 2.00}, {"kimi-k3p1-pro", 2.00}, {"qwen3p8-max", 2.00},
		{"fable", 8}, {"sonnet", 2}, {"haiku", 0.1}, {"gemini-pro", 4}, {"gemini-flash", 1.5},
		// deepseek-v4-pro is an alias onto V4.1 Flash as of the 2026-09-25 decommission.
		{"deepseek-v4-pro", 0.30}, {"deepseek-v4-flash", 0.30},
		{"gemini-flash-lite", 0.3},
	}
	for _, c := range cases {
		if p := ModelPricing(c.label); p.Input != c.in {
			t.Errorf("ModelPricing(%q).Input = %.2f, want %.2f", c.label, p.Input, c.in)
		}
	}
	// Windows resolve by label too (the footer meter sees labels, not raw ids).
	// glm-5p2 is an alias onto GLM 5.3, whose window is 1,048,576.
	if got := ContextWindow("glm-5p2"); got != 1_048_576 {
		t.Errorf("ContextWindow(label glm-5p2) = %d, want 1048576", got)
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
		{"accounts/fireworks/models/glm-5p2", 1_048_576},       // alias -> GLM 5.3 // was defaulting to 200K
		{"accounts/fireworks/models/kimi-k3p1-pro", 1_048_576}, // k3 ≠ the kimi-k2 262K case
		{"accounts/fireworks/models/kimi-k3", 1_048_576},       // alias -> K3.1 Pro
		{"accounts/fireworks/models/qwen3p8-max", 262_144},
		{"accounts/fireworks/models/qwen3p8-2p4t-a95b", 262_144}, // legacy path, still deployed
		{"accounts/fireworks/models/glm-5p3", 1_048_576},
		{"accounts/fireworks/models/glm-5p3-flash", 1_048_576},
		{"accounts/fireworks/models/deepseek-v4p1-flash", 1_048_576},
		{"accounts/fireworks/models/deepseek-v4p1-flash-1017", 1_048_576},
		{"accounts/fireworks/models/deepseek-v4-pro-0813", 1_048_576}, // alias -> V4.1 Flash
		{"accounts/fireworks/models/deepseek-v4-flash-0731", 1_048_576},
		{"gemini-3.1-pro-preview", 1_000_000},
		{"gemini-3.8-flash", 1_000_000},
		{"gemini-4-carbon", 1_000_000},
		{"gemini-4-hydrogen", 1_000_000},
		{"claude-haiku-5-5", 1_000_000},
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
		{"grok-4.7", 0.5, 2.5},
		{"claude-opus-5-5", 0.2, 5},   // 0.05x cache read
		{"claude-sonnet-5", 0.1, 2.5}, // alias -> Sonnet 5.5, 0.05x cache read
		{"claude-fable-5-2", 0.2, 10}, // 0.025x cache read
		{"claude-haiku-5-5", 0.01, 0.125},
		{"gemini-4-carbon", 0.2, 5}, // 0.05x cache read
		{"gemini-4-hydrogen", 0.075, 1.875},
		{"gpt-6.2-astra", 0.4, 10},                        // 0.05x cache read
		{"gpt-6.1-terra", 0.05, 1.25},                     // 0.05x cache read
		{"accounts/fireworks/models/glm-5p2", 0.26, 1.75}, // alias -> GLM 5.3 cache rate
		{"accounts/fireworks/models/kimi-k3", 0.20, 2.5},  // alias -> K3.1 Pro
		// Qwen 3.8 Max publishes $0.25 cached, NOT the 0.1x default this used to inherit.
		{"accounts/fireworks/models/qwen3p8-max", 0.25, 2.5},
		{"accounts/fireworks/models/qwen3p8-2p4t-a95b", 0.25, 2.5}, // legacy path prices the same
		{"accounts/fireworks/models/glm-5p3", 0.26, 1.75},
		{"accounts/fireworks/models/glm-5p3-flash", 0.03, 0.1875},
		{"accounts/fireworks/models/deepseek-v4p1-flash-1017", 0.006, 0.375},
		{"accounts/fireworks/models/deepseek-v4-pro-0813", 0.006, 0.375}, // alias -> V4.1 Flash
		{"accounts/fireworks/models/deepseek-v4-flash-0731", 0.006, 0.375},
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
	if p := ModelPricing("gpt-6.2-astra"); p.Input != 8 || p.Output != 40 || p.CacheRead != 0.4 {
		t.Fatalf("base card = %+v, want 8/40 with cache read 0.4", p)
	}
	// 300K prompt clears 272K: input 2x, output 1.5x.
	got := CostUSD("gpt-6.2-astra", 300_000, 1_000, 0, 0)
	want := (300_000*16.0 + 1_000*60.0) / 1e6
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("long-prompt cost = %v, want %v", got, want)
	}
	// Same model, short prompt, base rates.
	short := CostUSD("gpt-6.2-astra", 1_000, 1_000, 0, 0)
	if wantShort := (1_000*8.0 + 1_000*40.0) / 1e6; math.Abs(short-wantShort) > 1e-9 {
		t.Errorf("short-prompt cost = %v, want %v", short, wantShort)
	}
}

// The threshold measures the whole prompt, so cache reads and writes count
// toward it — billing a 280K prompt at base rates just because most of it was
// cached is exactly the underbill this mechanism exists to prevent.
func TestPriceTierThresholdCountsCachedPrompt(t *testing.T) {
	// 10K fresh + 280K cache read = 290K prompt → over the line.
	got := CostUSD("gpt-6.2-astra", 10_000, 0, 280_000, 0)
	want := (10_000*16.0 + 280_000*0.8) / 1e6
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("cached long prompt = %v, want %v", got, want)
	}
}

// The effort floor is a catalog FACT, not an adapter special case: Astra rejects
// "none" (so does GPT-6.2 Sol), GPT-6.1 Terra accepts it, and a model that
// declares no floor reports "".
func TestMinReasoningEffort(t *testing.T) {
	for _, m := range []string{"gpt-6.2-astra", "astra", "sol"} {
		if got := MinReasoningEffort(m); got != "low" {
			t.Errorf("%s floor = %q, want \"low\"", m, got)
		}
	}
	if got := MinReasoningEffort("terra"); got != "" {
		t.Errorf("terra floor = %q, want \"\" (no floor)", got)
	}
	if got := MinReasoningEffort("some-model-nobody-added"); got != "" {
		t.Errorf("unknown floor = %q, want \"\"", got)
	}
}

// Aliases exist so a vendor decommission migrates pins instead of breaking them:
// the retired name must resolve to the replacement entry, and a live label must
// always win over an alias.
func TestRetiredModelAliases(t *testing.T) {
	for _, c := range []struct{ alias, wantID string }{
		{"deepseek-v4-pro", "accounts/fireworks/models/deepseek-v4p1-flash-1017"},
		{"accounts/fireworks/models/deepseek-v4-pro-0813", "accounts/fireworks/models/deepseek-v4p1-flash-1017"},
		{"accounts/fireworks/models/deepseek-v4-flash-0731", "accounts/fireworks/models/deepseek-v4p1-flash-1017"},
		{"accounts/fireworks/models/deepseek-v4-flash-vision-exp", "accounts/fireworks/models/deepseek-v4p1-flash-1017"},
		{"accounts/fireworks/models/deepseek-v4p1-flash", "accounts/fireworks/models/deepseek-v4p1-flash-1017"},
		{"kimi-k3", "accounts/fireworks/models/kimi-k3p1-pro"},
		{"accounts/fireworks/models/kimi-k3", "accounts/fireworks/models/kimi-k3p1-pro"},
		{"claude-fable-5-1", "claude-fable-5-2"},
		{"claude-sonnet-5", "claude-sonnet-5-5"},
		{"claude-haiku-4-5-20251001", "claude-haiku-5-5"},
		{"claude-haiku-4-5", "claude-haiku-5-5"},
		{"gemini-3.1-pro-preview", "gemini-4-carbon"},
		{"gemini-3.6-flash", "gemini-4-hydrogen"},
		{"gpt-6-sol", "gpt-6.2-sol"},
		{"gpt-5.6-sol", "gpt-6.2-sol"},
		{"gpt-5.6-terra", "gpt-6.1-terra"},
		{"gpt-5.6-luna", "gpt-6-luna"},
		{"gpt-6-astra", "gpt-6.2-astra"},
		{"glm-5p2", "accounts/fireworks/models/glm-5p3"},
		{"accounts/fireworks/models/glm-5p2", "accounts/fireworks/models/glm-5p3"},
	} {
		m, ok := LookupModel(c.alias)
		if !ok || m.ID != c.wantID {
			t.Errorf("LookupModel(%q) = (%q, %v), want %q", c.alias, m.ID, ok, c.wantID)
		}
	}
	// A live label resolves to itself, never to an alias entry.
	if m, ok := LookupModel("glm-5p3"); !ok || m.ID != "accounts/fireworks/models/glm-5p3" {
		t.Errorf("live label glm-5p3 = (%q, %v)", m.ID, ok)
	}
	// No alias may name a model that is still live — mustLoadModelCatalog panics
	// on that, so this just pins the invariant for readers.
	for _, m := range CatalogModels() {
		for _, a := range m.Aliases {
			for _, other := range CatalogModels() {
				if other.ID == a || (other.Label != "" && other.Label == a) {
					t.Errorf("alias %q on %q collides with live model %q", a, m.ID, other.ID)
				}
			}
		}
	}
}

// Haiku 5.5 bills a prompt over 100K tokens at 5x every rate, cached tokens
// counting toward the threshold, the way Anthropic prices it.
func TestHaiku55LongPromptCost(t *testing.T) {
	short := CostUSD("claude-haiku-5-5", 100_000, 1_000_000, 0, 0)
	if want := (100_000*0.1 + 1_000_000*0.5) / 1e6; math.Abs(short-want) > 1e-9 {
		t.Errorf("100K prompt = $%.6f, want $%.6f (base card)", short, want)
	}
	long := CostUSD("claude-haiku-5-5", 10_000, 1_000_000, 100_000, 0)
	if want := (10_000*0.5 + 1_000_000*2.5 + 100_000*0.05) / 1e6; math.Abs(long-want) > 1e-9 {
		t.Errorf("110K prompt = $%.6f, want $%.6f (5x card)", long, want)
	}
}

// The two thinking-shape flags are catalog facts the adapters read.
func TestThinkingFlags(t *testing.T) {
	if !DropsUnboundThinking("claude-fable-5-2") || !DropsUnboundThinking("fable") {
		t.Error("Fable 5.2 enforces the history check on every account and must drop unbound blocks")
	}
	for _, m := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"} {
		if DropsUnboundThinking(m) {
			t.Errorf("%s must keep reading its thinking blocks (no drop_block)", m)
		}
	}
	if !UsesThinkingLevels("gemini-4-carbon") || !UsesThinkingLevels("gemini-flash") {
		t.Error("Gemini 4 takes thinking levels, not budgets")
	}
	if UsesThinkingLevels("gemini-3.5-flash-lite") {
		t.Error("Gemini 3.5 Flash-Lite still takes a thinking budget")
	}
}
