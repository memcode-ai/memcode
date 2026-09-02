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
	Match          []string `json:"match,omitempty"`
	MatchAll       []string `json:"match_all,omitempty"`
	Window         int      `json:"window,omitempty"`
	PriceIn        float64  `json:"price_in,omitempty"`
	PriceOut       float64  `json:"price_out,omitempty"`
	PriceCacheRead float64  `json:"price_cache_read,omitempty"`
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

// Pricing is APPROXIMATE per-model rates in USD per MILLION tokens. Cache write ≈
// 1.25× base input; cache read ≈ 0.1× base input unless the catalog overrides it.
type Pricing struct{ Input, Output, CacheWrite, CacheRead float64 }

func makePricing(in, out, cacheRead float64) Pricing {
	if cacheRead == 0 {
		cacheRead = in * 0.1
	}
	return Pricing{Input: in, Output: out, CacheWrite: in * 1.25, CacheRead: cacheRead}
}

// ModelPricing returns the rate card for a model id or label: the catalog entry
// if it declares one, else the first matching family rule, else the default.
// Both ledgers price against this (the CLI's and the gateway's), so every id
// that can serve must resolve to a non-zero card — the family floor guarantees
// Fireworks-served ids are never $0.
func ModelPricing(model string) Pricing {
	if m, ok := LookupModel(model); ok && m.PriceIn > 0 {
		return makePricing(m.PriceIn, m.PriceOut, m.PriceCacheRead)
	}
	for _, r := range modelCatalog.file.Families {
		if r.PriceIn > 0 && r.matches(model) {
			return makePricing(r.PriceIn, r.PriceOut, r.PriceCacheRead)
		}
	}
	return makePricing(modelCatalog.file.Defaults.PriceIn, modelCatalog.file.Defaults.PriceOut, 0)
}

// CostUSD prices one response's token usage under its model's rate card.
func CostUSD(model string, inTok, outTok, cacheRead, cacheWrite int) float64 {
	p := ModelPricing(model)
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
