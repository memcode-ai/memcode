package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/memcode-ai/memcode/gateway/internal/identity"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Hybrid is the gateway's backend: the strong-vendor providers (OpenAI /
// Anthropic / Gemini / Grok, whichever keys are configured) + the cheap lane
// (an OpenAI-compatible token API — Fireworks). Since the
// all-policy-client-side migration it is a pure SERVING map: the request names
// a concrete model (the compat gate resolved the catalog label to a raw id),
// route() hands it to the provider that owns it, and any mismatch — a vision
// turn on a text model, a document on a lane without PDF input, a prompt past
// the model's window — returns a TYPED error instead of being absorbed onto a
// different model. Model choice, escalation, and recovery are the CLI's
// (cli/internal/llm); the gateway serves exactly what was asked or declines
// with a machine-readable reason. BYOK key injection is serving, not routing:
// the same model, the user's key.
type Hybrid struct {
	strong     StrongTiers // vendor → strong provider (built once at NewFromEnv)
	cheap      *Fireworks  // the OpenAI-compatible cheap endpoint (Fireworks)
	cheapModel string      // the default cheap model (defensive: requests always name one)
	cheapURL   string      // the cheap endpoint base URL (BYOK builds user-keyed clients against it)
	cheapKey   string      // retained for symmetry/diagnostics; the gateway's own cheap-lane key

	// BYOK key-injection seam (see byokroute.go): nil byok = disabled.
	// byokProviders caches per-(org,user,vendor,version) constructed clients.
	byok          ByokKeys
	byokProviders sync.Map
}

// NewHybrid builds the serving map. strong is the vendor → StrongTier map (at
// least the default vendor should be present). cheapURL/cheapKey describe the
// OpenAI-compatible endpoint (Fireworks); cheapModel is the defensive default
// when a request arrives modelless (internal callers only — the compat gate
// always stamps one).
func NewHybrid(strong StrongTiers, cheapURL, cheapKey, cheapModel string) *Hybrid {
	return &Hybrid{
		strong:     strong,
		cheap:      NewFireworks(cheapURL, cheapKey, cheapModel),
		cheapModel: cheapModel,
		cheapURL:   cheapURL,
		cheapKey:   cheapKey,
	}
}

func (h *Hybrid) Complete(ctx context.Context, req wire.Request) (wire.Response, error) {
	return h.route(ctx, req, nil)
}

func (h *Hybrid) Stream(ctx context.Context, req wire.Request, sh wire.StreamHandler) (wire.Response, error) {
	return h.route(ctx, req, &sh)
}

// The router MUST satisfy the value-signature Streamer the metering runner
// type-asserts — a pointer-signature Stream compiles fine and then silently
// downgrades every streamed serve to Complete (billed, zero deltas, the text
// dropped by the SSE layer). That exact bug shipped once; never again.
var (
	_ ModelProvider = (*Hybrid)(nil)
	_ Streamer      = (*Hybrid)(nil)
)

// CapabilityError is the typed refusal for a turn the requested model cannot
// take: an image on a no-vision model, a document on a model without native
// PDF input. The CLI pre-checks these from the same catalog before sending —
// this is the enforcement backstop (and the honest answer for third-party
// clients). Mapped to HTTP 400 with code "model_capability"; never absorbed.
type CapabilityError struct {
	Capability string // "vision" | "document"
	Model      string // client-facing label
}

func (e *CapabilityError) Error() string {
	what := "this input"
	switch e.Capability {
	case "vision":
		what = "image input"
	case "document":
		what = "PDF/document input"
	}
	return fmt.Sprintf("model %s has no %s — pick a %s-capable model from GET /v1/models", e.Model, what, e.Capability)
}

// AsCapabilityError unwraps a turn error to its capability refusal, if that's
// what it is.
func AsCapabilityError(err error) *CapabilityError {
	var ce *CapabilityError
	if errors.As(err, &ce) {
		return ce
	}
	return nil
}

// route serves the turn on the backend that owns the requested model. The
// cheap lane buffers, so a stream just replays the assembled text.
func (h *Hybrid) route(ctx context.Context, req wire.Request, sh *wire.StreamHandler) (wire.Response, error) {
	model := req.Model
	if model == "" {
		model = h.cheapModel // defensive: internal callers only
		req.Model = model
	}
	spec := reg.spec(model)

	// Typed capability gates — the model serves the turn or declines; nothing
	// is retargeted. (The catalog is shared, so a well-behaved CLI never
	// reaches these.)
	if hasImage(req) && !spec.Vision {
		return wire.Response{}, &CapabilityError{Capability: "vision", Model: SanitizeModelID(model)}
	}
	if hasDocument(req) && !spec.PDF {
		return wire.Response{}, &CapabilityError{Capability: "document", Model: SanitizeModelID(model)}
	}
	// One pre-serve size estimate: the overflow gate reads it here, and it rides
	// the response so emitUsage's estimate_ratio (estimated / actual billed
	// input) keeps calibrating the estimator against reality.
	estimate := estimateTokens(req)
	if spec.Window > 0 && estimate > spec.Window {
		return wire.Response{}, &ContextOverflowError{Backend: owningVendor(model),
			Message: fmt.Sprintf("estimated prompt exceeds %s's %d-token window", SanitizeModelID(model), spec.Window)}
	}

	owner := owningVendor(model)
	if owner != "" && owner != "fireworks" {
		tier, ok := h.strong[owner]
		if !ok {
			// The model gate checks backendConfigured, so this is a config race
			// (key removed between gate and serve) — a plain serving error.
			return wire.Response{}, fmt.Errorf("no %s credentials configured for model %s", owner, SanitizeModelID(model))
		}
		resp, err := h.serveStrong(ctx, req, sh, tier)
		resp.EstimatedPromptTokens = estimate
		return resp, err
	}

	// Fireworks-owned id → the cheap lane, BYOK-first per the billing lane.
	resp, _, err := h.cheapComplete(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return wire.Response{}, ctx.Err()
		}
		return wire.Response{}, err // typed as-is: ByokError 422, credits 402, overflow 413, else 502
	}
	resp.Backend = "fireworks"
	resp.Pool = poolLabel(model) // short label (glm-5p2 / kimi-k3 / …) — the CLI's ServedBy tag
	resp.EstimatedPromptTokens = estimate
	stampServed(&resp, spec)
	if sh != nil && sh.Text != nil {
		if txt := resp.Text(); txt != "" {
			sh.Text(txt)
		}
	}
	return resp, nil
}

// serveStrong serves the turn on the strong provider that owns the requested
// model — BYOK-first per the billing lane: when the user brought a key for
// tier.Vendor (and the lane allows it), a user-keyed provider serves instead
// and any failure is turn-fatal (no memcode-key retry). The requested model id
// rides through untouched.
func (h *Hybrid) serveStrong(ctx context.Context, req wire.Request, sh *wire.StreamHandler, tier StrongTier) (wire.Response, error) {
	tier, isByok, berr := h.byokStrongTier(ctx, tier, req.BillingLane)
	if berr != nil {
		return wire.Response{}, berr
	}
	if !isByok {
		if err := requireCredits(ctx, tier.Vendor); err != nil {
			return wire.Response{}, err
		}
	}
	var resp wire.Response
	var err error
	if sh != nil {
		resp, err = tier.Provider.Stream(ctx, req, *sh)
	} else {
		resp, err = tier.Provider.Complete(ctx, req)
	}
	if err != nil {
		if isByok && ctx.Err() == nil {
			h.markInvalidOnAuth(ctx, identity.From(ctx), tier.Vendor, err)
			return wire.Response{}, byokCallError(tier.Vendor, err)
		}
		return resp, err
	}
	if isByok {
		resp.BYOK, resp.BYOKVendor = true, tier.Vendor
	}
	stampServed(&resp, reg.spec(req.Model))
	return resp, nil
}

// stampServed records the served model's engineering facts on the response: its
// window and usable INPUT budget (window − output reserve; small windows floor at
// window/2 so the budget never collapses). Stamped on EVERY serve so the CLI's
// adaptive compaction budget learns the real lane instead of falling back to a
// static default. Facts, not policy: the CLI layers its own cost-aware soft cap
// on top.
func stampServed(resp *wire.Response, spec ModelSpec) {
	if resp.ContextWindow == 0 {
		resp.ContextWindow = spec.Window
	}
	if resp.InputBudget == 0 && spec.Window > 0 {
		resp.InputBudget = max(spec.Window-inputBudgetReserve, spec.Window/2)
	}
}

// WebSearch / WebFetch delegate to the default strong provider (the
// side-channel has no per-turn model context).
func (h *Hybrid) WebSearch(ctx context.Context, query string) (string, wire.Response, error) {
	tier := h.strong.StrongTierFor("")
	if err := requireCredits(ctx, tier.Vendor); err != nil {
		return "", wire.Response{}, err // side-channel is always memcode-keyed (BYOK deferred)
	}
	return tier.Provider.WebSearch(ctx, query)
}
func (h *Hybrid) WebFetch(ctx context.Context, url string) (string, wire.Response, error) {
	tier := h.strong.StrongTierFor("")
	if err := requireCredits(ctx, tier.Vendor); err != nil {
		return "", wire.Response{}, err
	}
	return tier.Provider.WebFetch(ctx, url)
}

// --- serving helpers ---

// poolLabel is the short, human label for a cheap model id (its last path segment), e.g.
// "accounts/fireworks/models/kimi-k2p6" → "kimi-k2p6".
func poolLabel(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 {
		return m[i+1:]
	}
	return m
}

// hasBlockType reports whether any message block (top-level or inside a
// tool_result's ContentBlocks — a screenshot or a read PDF arrives there) has the
// given type.
func hasBlockType(req wire.Request, t string) bool {
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type == t {
				return true
			}
			for _, cb := range b.ContentBlocks {
				if cb.Type == t {
					return true
				}
			}
		}
	}
	return false
}

func hasImage(req wire.Request) bool    { return hasBlockType(req, "image") }
func hasDocument(req wire.Request) bool { return hasBlockType(req, "document") }

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// containsLower reports whether s contains substr after lowercasing s.
func containsLower(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), substr)
}

// Pre-call size estimate (no tokenizer): ~4 chars/token with a 1.25 safety margin, since
// code + JSON tool payloads tokenize denser than prose and a flat /4 under-counts. Biasing
// HIGH is the safe direction (an oversized turn errors typed BEFORE burning a vendor call).
// Calibrate against logged InputTokens before tuning.
const (
	estCharsPerToken = 4
	estSafetyNumer   = 125
	estSafetyDenom   = 100
)

// inputBudgetReserve is subtracted from the served model's window to stamp
// resp.InputBudget: room for the reply (32K max-output ceiling) plus estimator
// slack (the CLI's EstimateTokens runs a flat chars/4 with no safety margin,
// while this side biases ×1.25 — the reserve absorbs the drift). Small-window
// models floor at window/2 so the budget never collapses.
const inputBudgetReserve = 40_000

func estimateTokens(req wire.Request) int {
	n := len(req.System)
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			n += len(b.Text) + len(b.Content) + len(b.Input) + len(b.Thinking)
		}
	}
	return n / estCharsPerToken * estSafetyNumer / estSafetyDenom
}
