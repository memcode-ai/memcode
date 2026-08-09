package catalog

// Model id constants — the ids Go code needs to NAME (role fallbacks, the
// advisor, tests). Everything ABOUT a model — window, vision, pricing, picker
// flags — lives in models.json (see catalog.go); never hardcode a model fact
// here. Adding a model is a models.json edit; a constant is only warranted when
// code must reference the id directly.

// Canonical Claude model IDs (current as of build). These back the ADVISOR
// (Sonnet) and per-vendor tier fallbacks; the default inference tiers use the
// GPT-5.6 ids below.
const (
	ModelOpus   = "claude-opus-5"             // anthropic frontier tier
	ModelSonnet = "claude-sonnet-5"           // advisor default (second-opinion, cross-vendor); anthropic balanced tier
	ModelHaiku  = "claude-haiku-4-5-20251001" // anthropic cheap tier
)

// GPT-5.6 model IDs — the default inference tiers (OpenAI Responses API). Sol is
// the frontier escalation target, Terra the balanced everyday default + fallback
// absorb target, Luna the fast/cheap classifier.
const (
	ModelSol   = "gpt-5.6-sol"   // frontier — escalation target
	ModelTerra = "gpt-5.6-terra" // balanced — everyday default + fallback absorb
	ModelLuna  = "gpt-5.6-luna"  // fast/cheap — classifier + reviewer
)

// Gemini 3 model IDs (Google, via the native genai SDK).
const (
	ModelGeminiPro       = "gemini-3.1-pro-preview" // frontier — escalation target
	ModelGeminiFlash     = "gemini-3.6-flash"       // balanced — everyday default + fallback absorb
	ModelGeminiFlashLite = "gemini-3.5-flash-lite"  // fast/cheap — classifier
)

// Grok model ID (xAI, via the OpenAI-compatible api.x.ai endpoint). Grok 4.5 is
// the single model for all three strong-tier roles, so the selector resolves
// every tier to this id.
const (
	ModelGrok45 = "grok-4.5" // single model — serves frontier/balanced/cheap tiers
)
