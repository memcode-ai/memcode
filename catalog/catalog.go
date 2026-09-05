package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// catalog.go loads models.json — the SINGLE SOURCE OF TRUTH for model facts
// (windows, vision, pricing, picker flags), shared by every module through this
// SDK package. Nothing about a model is hardcoded in Go: the gateway's registry
// and the CLI's ledger/meter all read this one embedded config file, so a model
// swap or a price change is an edit to models.json, never a code change.

// The repo root's /models.json is a generated copy of this package's
// models.json (go:embed cannot reach outside the package, so the embedded
// file here is the one Go code reads). Edit catalog/models.json, then run
// `go generate ./catalog` to sync the root copy; catalog_root_test.go fails
// on any drift.
//go:generate cp models.json ../models.json

//go:embed models.json
var catalogData []byte

// CatalogModel is one models.json entry: a model's identity and its static facts.
// A zero Window or zero pricing means "not declared here" — lookups fall through
// to the family rules, then the defaults.
type CatalogModel struct {
	ID             string  `json:"id"`
	Label          string  `json:"label"`          // client-facing short name — the only id the CLI sees
	Vendor         string  `json:"vendor"`         // authoritative serving vendor ("openai" | "anthropic" | "gemini" | "grok" | "fireworks") — selection/steering identity, distinct from the display Group
	Name           string  `json:"name,omitempty"` // friendly display name ("Sonnet 5", "Grok 4.5") — the /model picker's name column
	Desc           string  `json:"desc,omitempty"` // one-line picker description ("1M context · Efficient for routine tasks")
	Window         int     `json:"window"`
	MaxOutput      int     `json:"max_output,omitempty"` // model's max output tokens; consumed where the provider REQUIRES max_tokens (Anthropic) to mean "uncapped"
	Vision         bool    `json:"vision"`
	PDF            bool    `json:"pdf,omitempty"` // accepts PDFs natively on the LLM call (document block / input_file / inline blob)
	Reasoning      bool    `json:"reasoning"`
	Pinnable       bool    `json:"pinnable,omitempty"` // offered in the /model picker
	Group          string  `json:"group,omitempty"`    // picker display family ("OpenAI", "Kimi", …)
	PriceIn        float64 `json:"price_in,omitempty"` // $/M tokens
	PriceOut       float64 `json:"price_out,omitempty"`
	PriceCacheRead float64 `json:"price_cache_read,omitempty"` // optional override (default in×0.1)

	// PriceTiers bends the rate card by PROMPT SIZE. Several vendors now price a
	// long prompt differently from a short one (GPT-6 Astra doubles input and
	// adds half again to output past 272K), and that is a per-request fact, not a
	// per-model one. Expressing it as ordered multiplier data — rather than a
	// special case in Go — keeps the rule where every consumer already reads it:
	// this file is shared verbatim with the gateway and with apps/www's own
	// calculateCost, and only one of those three can run Go.
	PriceTiers []PriceTier `json:"price_tiers,omitempty"`

	// MinReasoningEffort is the LOWEST reasoning effort this model accepts, in the
	// vendor's own vocabulary. Vendors disagree about the bottom of the range —
	// GPT-5.6 takes "none", GPT-6 Astra's floor is "low" and 400s below it — and
	// that is a model fact, so it lives here rather than as another adapter bool.
	// Empty means "no floor": send whatever the effort maps to.
	MinReasoningEffort string `json:"min_reasoning_effort,omitempty"`

	// Fallback is the model's mid-turn failure chain, in LABELS: who covers
	// when this model errors after transport retries, walked IN ORDER by the
	// CLI's recovery executor (availability/billing filtering happens at walk
	// time, client-side). Data, not Go — the routing doctrine lives here.
	Fallback []string `json:"fallback,omitempty"`
}

// familyRule is one ordered substring rule for ids the catalog doesn't list
// exactly — drifted versions, aliases, `[1m]` suffixes, bare vendor families.
// Match = any substring matches; MatchAll = every substring must match (scopes a
// tier word to its vendor, e.g. gemini+flash-lite). First matching rule that
// DECLARES a field wins for that field.
type familyRule struct {
	Match          []string    `json:"match,omitempty"`
	MatchAll       []string    `json:"match_all,omitempty"`
	Window         int         `json:"window,omitempty"`
	PriceIn        float64     `json:"price_in,omitempty"`
	PriceOut       float64     `json:"price_out,omitempty"`
	PriceCacheRead float64     `json:"price_cache_read,omitempty"`
	PriceTiers     []PriceTier `json:"price_tiers,omitempty"`
}

func (r familyRule) matches(model string) bool {
	if len(r.MatchAll) > 0 {
		for _, m := range r.MatchAll {
			if !strings.Contains(model, m) {
				return false
			}
		}
		return true
	}
	for _, m := range r.Match {
		if strings.Contains(model, m) {
			return true
		}
	}
	return false
}

type catalogFile struct {
	Models   []CatalogModel `json:"models"`
	Families []familyRule   `json:"families"`
	// DefaultModel is the SEED for a brand-new install: the label a session
	// starts on when no session override, workspace pin, or user pin exists. It
	// is persisted on first use and thereafter behaves like any other user
	// choice. It is NOT a fallback, NOT consulted per turn, and NOT a
	// substitution target — the pin resolver is its only caller.
	DefaultModel string `json:"default_model"`
	// UtilityModel serves internal plumbing ONLY: the structured classifiers
	// (classify, which authorize rides), compaction, and shrinkwrap. Utility
	// inference supports execution; it may never select, substitute, escalate,
	// downgrade, or steer the pinned model.
	UtilityModel string `json:"utility_model"`
	// SearchFees is the vendor-level per-request web-search surcharge, USD per
	// 1,000 searches, keyed by the serving vendor (Response.Backend: "anthropic" |
	// "openai" | "grok"). Native in-turn search bills this upstream ON TOP of
	// tokens; a vendor absent here (gemini, the cheap lane) has no per-search fee.
	SearchFees map[string]float64 `json:"search_fees"`
	Defaults   struct {
		Window   int     `json:"window"`
		PriceIn  float64 `json:"price_in"`
		PriceOut float64 `json:"price_out"`
	} `json:"defaults"`
}

var modelCatalog = mustLoadModelCatalog(catalogData)

type loadedCatalog struct {
	file    catalogFile
	byID    map[string]CatalogModel
	byLabel map[string]CatalogModel
}

func mustLoadModelCatalog(data []byte) *loadedCatalog {
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		panic(fmt.Sprintf("common: bad models.json: %v", err))
	}
	c := &loadedCatalog{
		file:    f,
		byID:    make(map[string]CatalogModel, len(f.Models)),
		byLabel: make(map[string]CatalogModel, len(f.Models)),
	}
	for _, m := range f.Models {
		c.byID[m.ID] = m
		if m.Label == "" {
			continue
		}
		// Labels are the wire (and pin) namespace — a duplicate would make the
		// label→id mapping ambiguous. Fail at load, not at serve time.
		if _, dup := c.byLabel[m.Label]; dup {
			panic(fmt.Sprintf("common: duplicate model label %q in models.json", m.Label))
		}
		c.byLabel[m.Label] = m
	}
	return c
}

// CatalogModels returns every models.json entry in file order (the display order).
func CatalogModels() []CatalogModel { return modelCatalog.file.Models }

// ModelVendor returns the authoritative serving vendor for a model id or label
// ("openai" | "anthropic" | "gemini" | "grok" | "fireworks"), "" when unknown.
func ModelVendor(idOrLabel string) string {
	if m, ok := LookupModel(idOrLabel); ok {
		return m.Vendor
	}
	return ""
}

// DefaultModel returns the seed label for an install with no pin anywhere. Its
// ONLY legitimate caller is the pin resolver, and only when the
// session/workspace/user chain came up empty; the result is persisted so the
// next run reads a concrete pin instead of re-deriving this.
func DefaultModel() string {
	return modelCatalog.file.DefaultModel
}

// UtilityModel returns the label for internal plumbing (classify/authorize,
// compact, shrinkwrap). Never user-facing, never in the picker, never a
// substitute for the pinned model.
func UtilityModel() string {
	return modelCatalog.file.UtilityModel
}

// VendorTier / TierVendors / TierAltitude are DELETED along with the `tiers`
// block they read. They named which model played frontier/balanced/cheap for
// each vendor — the fallback half of every Automatic ladder verdict. Nothing
// picks a model by tier any more: the user picks one, and it serves the session.

// FallbackChain returns the mid-turn failure chain (labels) for a model id or
// label. Nil when the catalog declares none.
func FallbackChain(idOrLabel string) []string {
	if m, ok := LookupModel(idOrLabel); ok {
		return m.Fallback
	}
	return nil
}

// LookupModel resolves a raw id or a client-facing label to its catalog entry.
func LookupModel(idOrLabel string) (CatalogModel, bool) {
	if m, ok := modelCatalog.byID[idOrLabel]; ok {
		return m, true
	}
	m, ok := modelCatalog.byLabel[idOrLabel]
	return m, ok
}

// ContextWindow returns the input context-window size (tokens) for a model id or
// label: the catalog entry if it declares one, else the first matching family
// rule, else the default. Used by the footer's "ctx N%" meter and the overflow
// ceiling when a backend doesn't report its window on the wire.
func ContextWindow(model string) int {
	if m, ok := LookupModel(model); ok && m.Window > 0 {
		return m.Window
	}
	for _, r := range modelCatalog.file.Families {
		if r.Window > 0 && r.matches(model) {
			return r.Window
		}
	}
	return modelCatalog.file.Defaults.Window
}

// MaxOutputTokens returns the model's max output tokens from the catalog, or 0 when
// the catalog doesn't declare one. Callers that must send a max_tokens value on the
// wire (Anthropic requires the field) use this to translate "uncapped" (request
// MaxTokens 0) into the largest value the model accepts.
func MaxOutputTokens(model string) int {
	if m, ok := LookupModel(model); ok {
		return m.MaxOutput
	}
	return 0
}

// MinReasoningEffort returns the lowest reasoning effort a model accepts, or ""
// when the catalog declares no floor. Adapters clamp against it so a cheap
// classifier turn can't 400 on a model whose range starts above "none".
func MinReasoningEffort(model string) string {
	if m, ok := LookupModel(model); ok {
		return m.MinReasoningEffort
	}
	return ""
}

// Pricing is APPROXIMATE per-model rates in USD per MILLION tokens. Cache write ≈
// 1.25× base input; cache read ≈ 0.1× base input unless the catalog overrides it.
type Pricing struct{ Input, Output, CacheWrite, CacheRead float64 }

// PriceTier is one prompt-size pricing band: once a request's prompt exceeds
// AbovePromptTokens, every rate on the card is multiplied for the WHOLE request
// (not just the tokens past the line — that is how vendors actually bill it).
//
// A multiplier left at 0 means 1× (unchanged), so a tier states only what it
// bends. Tiers are independent: the highest threshold a prompt clears wins, so
// order in JSON doesn't matter and a vendor can declare as many bands as it likes.
type PriceTier struct {
	AbovePromptTokens int     `json:"above_prompt_tokens"`
	In                float64 `json:"in,omitempty"`
	Out               float64 `json:"out,omitempty"`
	CacheRead         float64 `json:"cache_read,omitempty"`
	CacheWrite        float64 `json:"cache_write,omitempty"`
}

// scale applies one multiplier, treating 0 (absent) as 1×.
func scale(v, mult float64) float64 {
	if mult == 0 {
		return v
	}
	return v * mult
}

// apply bends a base card by every tier the prompt clears, highest threshold winning.
func applyTiers(p Pricing, tiers []PriceTier, promptTokens int) Pricing {
	var win *PriceTier
	for i := range tiers {
		t := &tiers[i]
		if promptTokens > t.AbovePromptTokens && (win == nil || t.AbovePromptTokens > win.AbovePromptTokens) {
			win = t
		}
	}
	if win == nil {
		return p
	}
	return Pricing{
		Input:      scale(p.Input, win.In),
		Output:     scale(p.Output, win.Out),
		CacheWrite: scale(p.CacheWrite, win.CacheWrite),
		CacheRead:  scale(p.CacheRead, win.CacheRead),
	}
}

func makePricing(in, out, cacheRead float64) Pricing {
	if cacheRead == 0 {
		cacheRead = in * 0.1
	}
	return Pricing{Input: in, Output: out, CacheWrite: in * 1.25, CacheRead: cacheRead}
}

// rateCard resolves the BASE card plus any prompt-size tiers for a model id or
// label: the catalog entry if it declares one, else the first matching family
// rule, else the default. Both ledgers price against this (the CLI's and the
// gateway's), so every id that can serve must resolve to a non-zero card — the
// family floor guarantees Fireworks-served ids are never $0.
func rateCard(model string) (Pricing, []PriceTier) {
	if m, ok := LookupModel(model); ok && m.PriceIn > 0 {
		return makePricing(m.PriceIn, m.PriceOut, m.PriceCacheRead), m.PriceTiers
	}
	for _, r := range modelCatalog.file.Families {
		if r.PriceIn > 0 && r.matches(model) {
			return makePricing(r.PriceIn, r.PriceOut, r.PriceCacheRead), r.PriceTiers
		}
	}
	return makePricing(modelCatalog.file.Defaults.PriceIn, modelCatalog.file.Defaults.PriceOut, 0), nil
}

// ModelPricing returns a model's BASE rate card — the short-prompt rates, before
// any prompt-size tier applies. Estimates and rate displays want this; anything
// billing a real request must use ModelPricingAt so a long prompt prices right.
func ModelPricing(model string) Pricing {
	p, _ := rateCard(model)
	return p
}

// ModelPricingAt returns the rate card that actually governs a request whose
// prompt is promptTokens long — base card with the winning tier's multipliers
// folded in. promptTokens is the WHOLE prompt: fresh input plus cache reads plus
// cache writes, since vendors measure the threshold against everything they read.
func ModelPricingAt(model string, promptTokens int) Pricing {
	p, tiers := rateCard(model)
	return applyTiers(p, tiers, promptTokens)
}

// CostUSD prices one response's token usage under its model's rate card. Token
// counts here are cache-EXCLUSIVE (see compat.applyUsage), so the prompt that
// decides the tier is the sum of all three input components.
func CostUSD(model string, inTok, outTok, cacheRead, cacheWrite int) float64 {
	p := ModelPricingAt(model, inTok+cacheRead+cacheWrite)
	return (float64(inTok)*p.Input + float64(outTok)*p.Output +
		float64(cacheRead)*p.CacheRead + float64(cacheWrite)*p.CacheWrite) / 1e6
}

// SearchFeeUSD prices the PER-REQUEST native web-search surcharge for a call:
// the serving vendor's catalog fee (models.json search_fees, USD per 1,000
// searches) times the number of searches the vendor billed
// (Response.SearchCount). Tokens alone under-billed searched turns — the
// upstream per-request fee never entered cost_usd. A vendor without a
// search_fees entry (gemini, the cheap lane, unknown backends) returns 0.
func SearchFeeUSD(vendor string, searches int) float64 {
	if searches <= 0 {
		return 0
	}
	return modelCatalog.file.SearchFees[vendor] * float64(searches) / 1000
}
