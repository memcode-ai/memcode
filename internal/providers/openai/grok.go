package openai

import (
	"github.com/memcode-ai/memcode/catalog"
)

// EnvGrokKey is the environment variable holding the xAI API key.
const EnvGrokKey = "XAI_API_KEY"

// Grok is the ModelProvider for xAI's Grok API — the OpenAI adapter pointed at
// api.x.ai. xAI's /v1/responses endpoint is OpenAI-Responses-compatible and its
// documented Go path is exactly this (xAI ships Python/TS SDKs, no Go SDK), so
// Grok embeds the OpenAI adapter with vendor fields overridden instead of
// duplicating the dialect. That puts the Agent Tools web_search built-in INSIDE
// agentic serving turns (xAI retired Live Search — search_parameters → 410,
// 2026-07-18 — and Agent Tools on /v1/responses is its replacement), plus the
// same for the WebSearch/WebFetch side channel.
//
// Vendor differences carried by the embedded adapter's fields:
//   - backend "grok" — its own wire/ledger tag, distinct from "openai".
//   - reasoning.effort clamped to low|high (xAI's vocabulary; xhigh 400s).
//   - no encrypted-reasoning includable (OpenAI-only round-trip).
//
// Search-fee metering rides the embedded adapter too: Agent Tools bills
// web_search PER INVOCATION ($5/1k, docs.x.ai/developers/pricing), and each
// invocation surfaces as a web_search_call output item — so the shared
// SearchCount counter prices grok turns via models.json search_fees["grok"].
// (Live Search's per-source num_sources_used card is dead alongside
// search_parameters; there is nothing per-source left to read.)
type Grok struct {
	*OpenAI
}

// NewGrok returns a provider for the xAI Grok API. apiKey is the xAI API key
// (XAI_API_KEY); the default served model is grok-4.6 (the single model for
// all three strong-tier roles).
func NewGrok(apiKey string) *Grok {
	o := NewOpenAI(apiKey)
	o.baseURL = "https://api.x.ai/v1"
	o.backend = "grok"
	o.defaultModel = catalog.ModelGrok46
	o.keyEnv = EnvGrokKey
	o.clampEffort = true
	o.includeEnc = false
	return &Grok{OpenAI: o}
}
