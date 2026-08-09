package provider

// providers_bridge.go — the seam to the SHARED provider-wire packages
// (packages/sdk/go/providers/*). Every protocol is implemented exactly ONCE
// out there — OpenAI Responses (+ the Grok variant), Anthropic Messages,
// Gemini (Developer API + Vertex), the generic chat/completions engine
// (compat) — and consumed by both this gateway and the CLI's direct endpoint
// mode. The aliases and thin forwarders here keep the rest of this package
// (and its tests) written against the local names it always used; nothing
// below implements wire behavior.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/memcode-ai/memcode/internal/providers/anthropic"
	"github.com/memcode-ai/memcode/internal/providers/gemini"
	"github.com/memcode-ai/memcode/internal/providers/openai"
	"github.com/memcode-ai/memcode/internal/providers/provcore"
	"github.com/memcode-ai/memcode/internal/wire"
)

// The OpenAI Responses adapter — shared implementation, gateway construction.
type OpenAI = openai.OpenAI

// Grok — the same Responses dialect pointed at api.x.ai.
type Grok = openai.Grok

// NewOpenAI returns the shared Responses adapter on the given key.
func NewOpenAI(apiKey string) *OpenAI { return openai.NewOpenAI(apiKey) }

// NewGrok returns the shared Grok variant on the given key.
func NewGrok(apiKey string) *Grok { return openai.NewGrok(apiKey) }

// The Anthropic Messages adapter — shared implementation.
type Anthropic = anthropic.Anthropic

// NewAnthropic returns the shared Messages adapter on the given key.
func NewAnthropic(apiKey string) *Anthropic { return anthropic.NewAnthropic(apiKey) }

// The Gemini adapter — shared implementation, both backends.
type Gemini = gemini.Gemini

// NewGemini returns the shared Gemini adapter on a Developer-API key.
func NewGemini(apiKey string) *Gemini { return gemini.NewGemini(apiKey) }

// NewGeminiVertex returns the shared Gemini adapter on Vertex AI credentials
// (the service-account JSON is RESOLVED here, gateway-side — the adapter just
// takes bytes).
func NewGeminiVertex(serviceAccountJSON []byte, project, location string) *Gemini {
	return gemini.NewGeminiVertex(serviceAccountJSON, project, location)
}

// Key env vars ride with the shared adapters (one definition).
const (
	EnvOpenAIKey = openai.EnvOpenAIKey
	EnvGrokKey   = openai.EnvGrokKey
	EnvAPIKey    = anthropic.EnvAnthropicKey
	EnvGeminiKey = gemini.EnvGeminiKey
	EnvGCPSAKey  = gemini.EnvGCPSAKey
)

// ContextOverflowError is the shared overflow signal (provcore) — aliased so
// errors.As matches across the gateway and the shared adapters identically.
type ContextOverflowError = provcore.ContextOverflowError

// IsContextOverflow reports whether err is (or wraps) a context-window
// overflow from any backend — the shared overflow error, or a lane 4xx
// flagged Overflow. The server maps it to 413 context_overflow.
func IsContextOverflow(err error) bool {
	var co *ContextOverflowError
	if errors.As(err, &co) {
		return true
	}
	var ve *LaneRequestError
	return errors.As(err, &ve) && ve.Overflow
}

// Kernel forwarders for the not-yet-extracted adapters (Anthropic, Gemini,
// the Fireworks compat client). Same names they always called.
func withRetry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	return provcore.WithRetry(ctx, fn)
}

func apiErrorInfo(err error) (int, http.Header, bool) { return provcore.APIErrorInfo(err) }
func isRetryable(code int) bool                       { return provcore.IsRetryable(code) }
func retryAfter(h http.Header) time.Duration          { return provcore.RetryAfter(h) }
func backoff(attempt int) time.Duration               { return provcore.Backoff(attempt) }
func newTurnHTTPClient() *http.Client                 { return provcore.NewTurnHTTPClient() }
func isContextOverflow(msg string) bool               { return provcore.IsOverflowMessage(msg) }

func splitWebSearchTool(tools []wire.ToolDef) ([]wire.ToolDef, bool) {
	return provcore.SplitWebSearchTool(tools)
}
