// Package provider defines the model boundaries the gateway talks to, plus the
// model-selection doctrine (ResolveModel, ResolveAlias, EffectiveModel). The
// wire types live in the shared protocol package (common); the vendor clients
// (anthropic, fireworks, openai, gemini, grok) and the hybrid router live here
// alongside the routing rules.
package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/wire"
)

// ResolveAlias maps the short aliases (opus|sonnet|haiku) to model ids. Any
// other value is treated as a literal model id and returned unchanged.
func ResolveAlias(s string) string {
	switch s {
	case "opus":
		return catalog.ModelOpus
	case "sonnet":
		return catalog.ModelSonnet
	case "haiku":
		return catalog.ModelHaiku
	default:
		return s
	}
}

// The wire types (Request/Response/Block/Message/ToolDef/Effort/RoutingHint/…),
// their constructors, and the prompt-cache breakpoint logic all live in the shared
// protocol package (github.com/memcode-ai/memcode/internal/common). The anthropic /
// vllm / router code references them as wire.X directly — one source of truth.

// Streamer is an optional capability: a provider that can stream a completion,
// emitting text/usage as they arrive while still returning the fully assembled
// Response. Callers type-assert for it and fall back to Complete otherwise.
//
// (Capability interfaces live at their consuming boundary — here, the gateway —
// not in the shared protocol package; the wire types they reference are common's.)
type Streamer interface {
	Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error)
}

// WebSearcher is an optional capability: a provider that can answer a query using
// server-side web search. Returns the synthesized text AND the usage the call
// billed (model + token counts) so the gateway can meter side-channel spend —
// these calls moved real money invisibly when they returned text alone.
type WebSearcher interface {
	WebSearch(ctx context.Context, query string) (string, wire.Response, error)
}

// webSearchToolName is the CLI's web_search FUNCTION tool. A strong provider with a
// native server-side search swaps this function def for its built-in search at request
// build time, so the SERVING model searches in-request — results land in the turn's own
// (cached) context instead of bouncing through a cold side-channel model call. Who does
// what: OpenAI → the Responses built-in web_search tool; Anthropic → web_search_20250305;
// Grok → xAI Live Search (a request-level search_parameters field, not a tool). The cheap
// lane (Fireworks) keeps the function def — no native search there, the CLI executes it
// via the gateway's /v1/websearch side channel — and so does Gemini, whose google_search
// grounding cannot coexist with function declarations in one request.
// WebFetcher is an optional capability: a provider that can fetch a specific URL
// server-side (text/PDF; not JS-rendered pages) and return its readable content,
// plus the usage billed (see WebSearcher).
type WebFetcher interface {
	WebFetch(ctx context.Context, url string) (string, wire.Response, error)
}

// ModelProvider performs reasoning/generation calls (Claude in v1).
type ModelProvider interface {
	Complete(ctx context.Context, r wire.Request) (wire.Response, error)
}

// StrongProvider is the capability surface a strong-tier backend must satisfy: a
// ModelProvider that can also stream, answer web searches, fetch URLs, and report
// its default model id. The Hybrid router holds one of these (swappable per-turn
// via Intent.Vendor) instead of a concrete *OpenAI — so Anthropic, Gemini, and
// Grok can each serve as the strong tier behind the same router. Model() returns
// the vendor's default (balanced-tier) model id, used for display and fallback.
type StrongProvider interface {
	ModelProvider
	Streamer
	WebSearcher
	WebFetcher
	Model() string
}

// NOTE: there is intentionally NO embedding/vector provider here. memcode's NL
// recall (internal/recall) is local BM25 over prose — offline, free, no vendor.
// A hosted semantic provider would be added behind a new seam later, and only if
// a measured recall eval proves lexical recall insufficient.

// --- credentials + backend selection (env-only) ---

// EnvGrokKey is the environment variable holding the xAI (Grok) API key.

// gcpSAKey reads the service-account key env var and returns the raw JSON bytes,
// honoring the documented "base64 or raw JSON" contract: multi-line JSON is
// commonly stored base64-encoded in an env var / secret, and passing that encoded
// blob straight to the credential detector fails (the Gemini vendor was silently
// dead while still advertised). If the value base64-decodes to something that looks
// like JSON, use the decoded bytes; otherwise treat it as raw JSON.
func gcpSAKey() []byte {
	raw := strings.TrimSpace(os.Getenv(EnvGCPSAKey))
	if raw == "" {
		return nil
	}
	if dec, err := base64.StdEncoding.DecodeString(raw); err == nil {
		if t := strings.TrimSpace(string(dec)); strings.HasPrefix(t, "{") {
			return dec
		}
	}
	// Only accept a raw value that actually looks like a JSON key. This rejects the
	// deploy.sh "placeholder" seed, so the gemini vendor isn't advertised (and then
	// 502'd) when no real key is configured.
	if strings.HasPrefix(raw, "{") {
		return []byte(raw)
	}
	return nil
}

// HasGeminiCreds reports whether real Gemini credentials are configured (a valid
// SA key or a Developer API key) — used by ConfiguredVendors so the /model selector
// never offers a vendor the gateway can't actually serve.
func HasGeminiCreds() bool {
	return len(gcpSAKey()) > 0 || strings.TrimSpace(os.Getenv(EnvGeminiKey)) != ""
}

// Backend-selection environment. memcode has ONE cheap lane — Fireworks, a
// hosted OpenAI-compatible token API — plus the frontier vendor APIs (OpenAI,
// Anthropic, Gemini, Grok). Hybrid routes between them per turn (the prod
// architecture); the pure single-vendor modes run the WHOLE session on one
// backend, chosen by env. (The self-hosted vLLM/RunPod era ended at the
// 2026-06-12 Fireworks cutover; there is no self-hosted backend.)
const (
	// EnvProvider selects the backend: "" | "anthropic" (default) | "openai" |
	// "gemini" | "grok" | "fireworks" (cheap lane only) | "hybrid" (OpenAI
	// strong tier + the cheap lane — the target architecture; needs both
	// backends' env). Hybrid routes per-turn and can switch the strong tier
	// via Intent.Vendor.
	EnvProvider = "MEMCODE_PROVIDER"
	// EnvFireworksURL is the cheap lane's OpenAI-compatible /v1 root, e.g.
	// https://api.fireworks.ai/inference/v1. Required for fireworks/hybrid.
	EnvFireworksURL = "MEMCODE_FIREWORKS_URL"
	// EnvFireworksKey is the Fireworks API key. Required for fireworks/hybrid.
	EnvFireworksKey = "MEMCODE_FIREWORKS_KEY"
	// EnvFireworksModel is the served model id backing requests that arrive
	// without one; Hybrid retargets per resolved role.
	EnvFireworksModel = "MEMCODE_FIREWORKS_MODEL"
	// Legacy env names from the self-hosted-vLLM era — still honored (second
	// in precedence) so an un-migrated deploy keeps working. Remove once the
	// Cloud Run service env is migrated (RUNBOOK).
	envLegacyVLLMURL   = "MEMCODE_VLLM_URL"
	envLegacyVLLMKey   = "MEMCODE_VLLM_KEY"
	envLegacyVLLMModel = "MEMCODE_VLLM_MODEL"
)

// envFirst returns the first non-empty environment value among names — the
// new-name-first, legacy-fallback lookup for renamed env vars.
func envFirst(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// APIKeyFromEnv returns the Anthropic API key from the environment.
//
// Keys are read from the environment ONLY — never from .memcode/config.json or
// the state database. A project-root .env (gitignored) may be loaded into the
// environment by the CLI before this is read; OS keychain / credential-helper
// support can be added later.
func APIKeyFromEnv() string {
	return os.Getenv(EnvAPIKey)
}

// NewFromEnv constructs the active ModelProvider from the environment — the ONE
// place backend selection and its credential story live. Call it at the cmd
// boundary after LoadDotEnv; the error text tells the user exactly what to set.
func NewFromEnv() (ModelProvider, error) {
	switch backend := strings.ToLower(strings.TrimSpace(os.Getenv(EnvProvider))); backend {
	case "", "anthropic":
		// Pure Anthropic backend. Pin the ANTHROPIC tier ids for every role (planner=Opus,
		// reviewer/classify=Haiku, standard=Sonnet) and point the fallback vendor at
		// anthropic — otherwise unset roles + the direct strongFallback paths (self_heal,
		// re-plan) resolved to gpt-5.6-* and were sent to the Anthropic API, which can't
		// serve them (every turn failed model-not-found).
		SetDefaultVendor("anthropic")
		SetModels(catalog.ModelOpus, catalog.ModelHaiku, catalog.ModelSonnet, catalog.ModelHaiku)
		key := APIKeyFromEnv()
		if key == "" {
			return nil, fmt.Errorf("no %s set — export it or add it to a .env at the repo root", EnvAPIKey)
		}
		return NewAnthropic(key), nil
	case "fireworks":
		return fireworksFromEnv()
	case "hybrid":
		// One always-warm cheap lane (a token-billed OpenAI endpoint — Fireworks) +
		// a swappable strong tier (OpenAI by default; Anthropic/Gemini/Grok when
		// their keys are present and the CLI picks them via Intent.Vendor). No
		// pod/pool/registry lifecycle. Model ROLES come from config.json (catalog);
		// per-model props from models.json — not env.
		model := reg.role("standard")
		if model == "" {
			return nil, fmt.Errorf("config.json roles.standard is required (the standard lane's served model id)")
		}
		url := envFirst(EnvFireworksURL, envLegacyVLLMURL)
		if url == "" {
			return nil, fmt.Errorf("%s is required for hybrid (the cheap lane's OpenAI /v1 URL)", EnvFireworksURL)
		}
		// Build the strong-tier map: a vendor is available ONLY when its key is
		// set. OpenAI is the default and MUST be present (the fallback when
		// Intent.Vendor is "" or names an unconfigured vendor).
		oaiKey := os.Getenv(EnvOpenAIKey)
		if oaiKey == "" {
			return nil, fmt.Errorf("no %s set — hybrid needs OpenAI as the default strong tier + fallback", EnvOpenAIKey)
		}
		strong := StrongTiers{
			"openai": {Vendor: "openai", Provider: NewOpenAI(oaiKey)},
		}
		if k := os.Getenv(EnvAPIKey); k != "" {
			strong["anthropic"] = StrongTier{Vendor: "anthropic", Provider: NewAnthropic(k)}
		}
		// Gemini: prefer Vertex AI (GCP service account, paid quota) when the
		// SA key is set; fall back to the Gemini Developer API (free-tier) only
		// when the SA isn't available.
		if sa := gcpSAKey(); len(sa) > 0 {
			strong["gemini"] = StrongTier{Vendor: "gemini", Provider: NewGeminiVertex(sa,
				os.Getenv("GOOGLE_CLOUD_PROJECT"), "global")}
		} else if k := os.Getenv(EnvGeminiKey); k != "" {
			strong["gemini"] = StrongTier{Vendor: "gemini", Provider: NewGemini(k)}
		}
		if k := os.Getenv(EnvGrokKey); k != "" {
			strong["grok"] = StrongTier{Vendor: "grok", Provider: NewGrok(k)}
		}
		// Pin the role→model ids the /v1/models control plane reports (the CLI's
		// ladder maps role lanes onto these labels).
		SetDefaultVendor("openai") // hybrid's default strong tier + side-channel provider
		SetModels(reg.role("planner"), reg.role("reviewer"), model, reg.role("classify"))
		return NewHybrid(strong, url, envFirst(EnvFireworksKey, envLegacyVLLMKey), model), nil
	case "openai":
		// Pure OpenAI backend (no cheap lane). Requires the OpenAI API key.
		key := os.Getenv(EnvOpenAIKey)
		if key == "" {
			return nil, fmt.Errorf("no %s set — openai mode needs the OpenAI API key", EnvOpenAIKey)
		}
		// classify=Luna (4th arg) — without it the classifier collapsed onto standard
		// (Terra, 2.5x the cost) for every followup-fold / plan-intent / risk judge.
		SetDefaultVendor("openai")
		SetModels(catalog.ModelSol, catalog.ModelLuna, catalog.ModelTerra, catalog.ModelLuna)
		return NewOpenAI(key), nil
	case "gemini":
		// Pure Gemini backend (no cheap lane). Uses Vertex AI when a GCP service
		// account key is set; otherwise the Gemini Developer API with an API key.
		// classify=Flash-Lite (4th arg): keep the classifier on the cheapest tier
		// instead of collapsing onto standard (Flash). Fallbacks resolve to Gemini.
		SetDefaultVendor("gemini")
		if sa := gcpSAKey(); len(sa) > 0 {
			SetModels(catalog.ModelGeminiPro, catalog.ModelGeminiFlashLite, catalog.ModelGeminiFlash, catalog.ModelGeminiFlashLite)
			return NewGeminiVertex(sa, os.Getenv("GOOGLE_CLOUD_PROJECT"), "global"), nil
		}
		key := os.Getenv(EnvGeminiKey)
		if key == "" {
			return nil, fmt.Errorf("no %s or %s set — gemini mode needs Google AI credentials", EnvGeminiKey, EnvGCPSAKey)
		}
		SetModels(catalog.ModelGeminiPro, catalog.ModelGeminiFlashLite, catalog.ModelGeminiFlash, catalog.ModelGeminiFlashLite)
		return NewGemini(key), nil
	case "grok":
		// Pure Grok backend (no cheap lane). Requires the xAI API key.
		key := os.Getenv(EnvGrokKey)
		if key == "" {
			return nil, fmt.Errorf("no %s set — grok mode needs the xAI API key", EnvGrokKey)
		}
		SetDefaultVendor("grok")
		SetModels(catalog.ModelGrok45, catalog.ModelGrok45, catalog.ModelGrok45, catalog.ModelGrok45)
		return NewGrok(key), nil
	default:
		return nil, fmt.Errorf("unknown %s %q (anthropic|openai|gemini|grok|fireworks|hybrid)", EnvProvider, backend)
	}
}

// fireworksFromEnv builds the Fireworks client from its env triplet (URL+key+
// model required — Fireworks is a billed, hosted endpoint).
func fireworksFromEnv() (*Fireworks, error) {
	url := envFirst(EnvFireworksURL, envLegacyVLLMURL)
	if url == "" {
		return nil, fmt.Errorf("%s is required (the Fireworks base URL, e.g. https://api.fireworks.ai/inference/v1)", EnvFireworksURL)
	}
	key := envFirst(EnvFireworksKey, envLegacyVLLMKey)
	if key == "" {
		return nil, fmt.Errorf("%s is required for fireworks (the Fireworks API key)", EnvFireworksKey)
	}
	model := envFirst(EnvFireworksModel, envLegacyVLLMModel)
	if model == "" {
		return nil, fmt.Errorf("%s is required (the served model id)", EnvFireworksModel)
	}
	SetModels(model, model, model)
	return NewFireworks(url, key, model), nil
}

// EffectiveModel resolves a configured tier value (alias or model id) to the wire
// model for the ACTIVE backend. On Anthropic (the default) it is ResolveAlias;
// on fireworks every tier collapses to the single served model. Pricing follows
// the returned id, so the ledger stays honest whichever backend is live.
func EffectiveModel(s string) string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvProvider))) {
	case "fireworks":
		if m := envFirst(EnvFireworksModel, envLegacyVLLMModel); m != "" {
			return m
		}
	case "openai":
		// OpenAI-only: every tier collapses to the single served model family. Pricing
		// follows the returned id; the CLI's tier aliases (opus/sonnet/haiku) still resolve
		// via ResolveAlias to Claude ids (used by the advisor), so map them to the GPT-5.6
		// equivalents here.
		return ResolveAlias(s)
	}
	return ResolveAlias(s)
}
