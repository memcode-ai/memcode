// Package provider defines the (model, embedding, edit-apply) boundaries the
// engine talks to, plus the default Claude model tiers. v1 wires NO
// implementations here — these are the seams the model-backed phases
// (understand, learn, agent) plug into. Defaults are recorded now so the tiered
// strategy is explicit and configurable.
package provider

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Tier names the role of a model call. Each maps to a sensible default model
// but is overridable via config.
type Tier string

const (
	TierPlanner     Tier = "planner"
	TierCoder       Tier = "coder"
	TierReviewer    Tier = "reviewer"
	TierSynthesizer Tier = "synthesizer"
	TierClassifier  Tier = "classifier"
)

// DefaultModel returns the default model id for a tier.
//
//	planner, reviewer        -> Sol    (hard reasoning — the frontier tier)
//	coder, synthesizer       -> Terra  (the everyday default)
//	classifier               -> Luna   (the reducer's cheap, frequent router)
func DefaultModel(t Tier) string {
	switch t {
	case TierPlanner, TierReviewer:
		return catalog.ModelSol
	case TierClassifier:
		return catalog.ModelLuna
	default:
		return catalog.ModelTerra
	}
}

// ShortModel maps a model id back to its short alias for display (the inverse of
// ResolveAlias). Unknown ids are returned unchanged.
func ShortModel(id string) string {
	switch id {
	case catalog.ModelOpus:
		return "opus"
	case catalog.ModelSonnet:
		return "sonnet"
	case catalog.ModelHaiku:
		return "haiku"
	case catalog.ModelSol:
		return "sol"
	case catalog.ModelTerra:
		return "terra"
	case catalog.ModelLuna:
		return "luna"
	default:
		// Provider-pathed ids (e.g. org/model) → last segment.
		if i := strings.LastIndexByte(id, '/'); i >= 0 {
			return id[i+1:]
		}
		return id
	}
}

// ResolveAlias maps the short aliases (opus|sonnet|haiku|sol|terra|luna) to model
// ids. The Claude aliases (opus|sonnet|haiku) are kept for backward compat with
// existing configs; the GPT-5.6 aliases (sol|terra|luna) are the current tiers.
// Any other value is treated as a literal model id and returned unchanged.
func ResolveAlias(s string) string {
	switch s {
	case "opus":
		return catalog.ModelOpus
	case "sonnet":
		return catalog.ModelSonnet
	case "haiku":
		return catalog.ModelHaiku
	case "sol":
		return catalog.ModelSol
	case "terra":
		return catalog.ModelTerra
	case "luna":
		return catalog.ModelLuna
	default:
		return s
	}
}

// The wire types (Request/Response/Block/Message/ToolDef/Effort/RoutingHint/…),
// their constructors (TextBlock/ImageBlock), and the error sentinels all live in
// the shared protocol package (github.com/memcode-ai/memcode/internal/common). This
// package references them as wire.X directly — one source of truth, no shims.

// Streamer is an optional capability: a provider that can stream a completion,
// emitting text/usage as they arrive while still returning the fully assembled
// Response. Callers type-assert for it and fall back to Complete otherwise.
//
// (Capability interfaces live at their consuming boundary — here, the CLI — not in
// the shared protocol package; the wire types they reference are common's.)
type Streamer interface {
	Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error)
}

// WebSearcher is an optional capability: a provider that can answer a query using
// server-side web search, returning a synthesized text answer. Type-assert for it.
type WebSearcher interface {
	WebSearch(ctx context.Context, query string) (string, error)
}

// WebFetcher is an optional capability: a provider that can fetch a specific URL
// server-side (text/PDF; not JS-rendered pages) and return its readable content.
type WebFetcher interface {
	WebFetch(ctx context.Context, url string) (string, error)
}

// Advisor is an optional capability: a provider that can ask a second-opinion
// model (a different vendor) to advise the best path forward. Type-assert for it.
type Advisor interface {
	Advise(ctx context.Context, question, effort string) (string, error)
}

// ModelProvider performs reasoning/generation calls (Claude in v1).
type ModelProvider interface {
	Complete(ctx context.Context, r wire.Request) (wire.Response, error)
}

// NOTE: there is intentionally NO embedding/vector provider here. memcode's NL
// recall (internal/recall) is local BM25 over prose — offline, free, no vendor.
// A hosted semantic provider would be added behind a new seam later, and only if
// a measured recall eval proves lexical recall insufficient.

// Edit is an anchored search/replace edit (see the Agent Runtime Contract).
type Edit struct {
	OldString  string
	NewString  string
	ReplaceAll bool
}

// ApplyResult reports the outcome of applying an Edit.
type ApplyResult struct {
	Diff    string
	Applied bool
}

// Applier merges an edit into a file. v1 = anchored search/replace; a
// fast-apply model can be swapped in later without touching callers.
type Applier interface {
	Apply(ctx context.Context, path string, edit Edit) (ApplyResult, error)
}

// --- gateway connection ---
//
// The CLI has exactly ONE backend: the memcode gateway (cli → api → llms).
// Hosted provider keys, BYOK storage, and metering live SERVER-side in the
// api module; the wire adapters and routing policy ship in this binary (the
// CLI is the agent — shared providers/*, llm selection). The endpoint defaults
// to production; MEMCODE_API_URL is a DEV OVERRIDE for pointing at a locally
// running gateway (`go run ./api`), never a requirement.
const (
	EnvAPIURL   = "MEMCODE_API_URL"
	EnvAPIToken = "MEMCODE_API_TOKEN"

	// TokenPrefix marks org-scoped gateway keys minted by /login — the ONLY
	// kind of credential that exists. Its presence in the stored token IS the
	// local logged-in signal (zero network at boot).
	TokenPrefix = "memcode_"

	// DefaultAPIURL is the production memcode gateway.
	DefaultAPIURL = "https://code.memcode.ai"
)

// APIURL resolves the gateway endpoint: the MEMCODE_API_URL override if set,
// otherwise production.
func APIURL() string {
	if url := os.Getenv(EnvAPIURL); url != "" {
		return url
	}
	return DefaultAPIURL
}

// NewFromEnv constructs the backend connection from the environment — the ONE
// place the connection story lives. Call it at the cmd boundary after
// LoadDotEnv. Backend selection: a memcode token → the hosted gateway at
// {api}/v1; else a configured endpoint (MEMCODE_ENDPOINT_URL, or a resolved
// config endpoint passed by the caller) → the same compat transport pointed
// at it; else the signed-out error. ONE turn transport either way (wire.go). The variadic endpoint lets callers that load project
// config pass its resolved endpoint without this package importing config.
func NewFromEnv(endpoints ...Endpoint) (ModelProvider, error) {
	if token := os.Getenv(EnvAPIToken); token != "" {
		return dial(APIURL(), token), nil
	}
	if ep, ok := resolveEndpoint(endpoints); ok {
		return dialEndpoint(ep), nil
	}
	// The FIRST thing a fresh install hits. Lead with the command that fixes
	// it — `memcode login` opens the browser and writes the token itself; the
	// env-file and local-endpoint details are the advanced paths, not the
	// onboarding path.
	return nil, fmt.Errorf("not connected to memcode.ai — run `memcode login` to sign in and set up this machine\n(advanced: set %s in %s, or point memcode at a local endpoint with %s)", EnvAPIToken, GlobalEnvPath(), EnvEndpointURL)
}

// resolveEndpoint picks the active custom endpoint: the caller's resolved one
// (config-aware callers pass exactly one) wins, else the env-configured one.
// Either way a keyless endpoint on a well-known provider host picks up the
// ecosystem-standard env var (ConventionalKey) — explicit keys always win.
func resolveEndpoint(endpoints []Endpoint) (Endpoint, bool) {
	pick := func(ep Endpoint) (Endpoint, bool) {
		if ep.Key == "" {
			ep.Key = ConventionalKey(ep.BaseURL)
		}
		return ep, true
	}
	for _, ep := range endpoints {
		if ep.BaseURL != "" {
			return pick(ep)
		}
	}
	if ep, ok := EndpointFromEnv(); ok {
		return pick(ep)
	}
	return Endpoint{}, false
}

// ErrNotLoggedIn is returned by the lazy provider for any model-backed call
// made before /login. The TUI shows its own gate before dispatch; this is the
// backstop for any path that slips through.
var ErrNotLoggedIn = fmt.Errorf("not signed in — run /login to connect to memcode.ai")

// Lazy is a ModelProvider whose backend connection may be ABSENT at
// construction: the TUI always opens (mandatory-login boot), and /login swaps
// real credentials in without a restart. All capability methods forward to the
// inner connection (the compat turn transport + the gateway side-channel
// client, or the compat transport alone in endpoint mode — see wire.go), or
// fail with ErrNotLoggedIn while signed out. Safe for concurrent use (atomic pointer
// swap).
type Lazy struct {
	c           atomic.Pointer[conn]
	fallback    atomic.Pointer[Endpoint] // configured custom endpoint — the signed-out backend
	retryNotify atomic.Value             // func(attempt int, err error, delay time.Duration)
}

// NewFromEnvLazy constructs the lazy provider. Backend selection (one-wire
// Phase C): a real login (a memcode_-prefixed org key — the local logged-in
// signal) → the hosted gateway; else a configured endpoint (the caller's
// resolved config endpoint, or MEMCODE_ENDPOINT_URL) → that endpoint on the
// compat transport; else signed out — the TUI opens on the sign-in card.
// Unlike NewFromEnv it never fails — signed-out is a valid state for the TUI.
func NewFromEnvLazy(endpoints ...Endpoint) *Lazy {
	l := &Lazy{}
	if ep, ok := resolveEndpoint(endpoints); ok {
		// Remembered even when a login wins right now: /logout falls back to
		// the endpoint instead of dead air (backend selection re-applies).
		l.fallback.Store(&ep)
	}
	if token := os.Getenv(EnvAPIToken); strings.HasPrefix(token, TokenPrefix) {
		l.c.Store(dial(APIURL(), token))
	} else if ep := l.fallback.Load(); ep != nil {
		l.c.Store(dialEndpoint(*ep))
	}
	return l
}

// Connected reports whether a usable backend is present — hosted gateway
// credentials OR a configured custom endpoint (Phase C widening: endpoint
// mode is a connected state; only token-less-and-endpoint-less is signed out).
func (l *Lazy) Connected() bool { return l.c.Load() != nil }

// Endpoint reports the ACTIVE custom endpoint, ok=false when hosted or signed
// out. This is the one backend-mode signal the runtime/TUI key on (via
// Session.Endpoint) — capability gating, the /model picker, cost display.
func (l *Lazy) Endpoint() (Endpoint, bool) {
	c := l.c.Load()
	if c == nil {
		return Endpoint{}, false
	}
	return c.Endpoint()
}

// SetCredentials swaps in a fresh gateway connection (the /login success
// path). A retry-notify callback registered before login is re-applied.
func (l *Lazy) SetCredentials(url, token string) {
	c := dial(url, token)
	if fn, ok := l.retryNotify.Load().(func(int, error, time.Duration)); ok && fn != nil {
		c.SetRetryNotify(fn)
	}
	l.c.Store(c)
}

// ClearCredentials drops the gateway client (the /logout path). With a custom
// endpoint configured the connection falls BACK to it — the same backend
// selection boot applies — so signing out of memcode returns to endpoint mode,
// not dead air; otherwise subsequent model calls fail with ErrNotLoggedIn
// until the next SetCredentials.
func (l *Lazy) ClearCredentials() {
	if ep := l.fallback.Load(); ep != nil {
		c := dialEndpoint(*ep)
		if fn, ok := l.retryNotify.Load().(func(int, error, time.Duration)); ok && fn != nil {
			c.SetRetryNotify(fn)
		}
		l.c.Store(c)
		return
	}
	l.c.Store(nil)
}

// SetRetryNotify satisfies the retryNotifier seam: applied to the current
// client if present, and remembered for the client /login constructs later.
func (l *Lazy) SetRetryNotify(fn func(attempt int, err error, delay time.Duration)) {
	l.retryNotify.Store(fn)
	if c := l.c.Load(); c != nil {
		c.SetRetryNotify(fn)
	}
}

func (l *Lazy) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	c := l.c.Load()
	if c == nil {
		return wire.Response{}, ErrNotLoggedIn
	}
	return c.Complete(ctx, r)
}

func (l *Lazy) Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	c := l.c.Load()
	if c == nil {
		return wire.Response{}, ErrNotLoggedIn
	}
	return c.Stream(ctx, r, h)
}

func (l *Lazy) WebSearch(ctx context.Context, query string) (string, error) {
	c := l.c.Load()
	if c == nil {
		return "", ErrNotLoggedIn
	}
	return c.WebSearch(ctx, query)
}

func (l *Lazy) WebFetch(ctx context.Context, url string) (string, error) {
	c := l.c.Load()
	if c == nil {
		return "", ErrNotLoggedIn
	}
	return c.WebFetch(ctx, url)
}

func (l *Lazy) Advise(ctx context.Context, question, effort string) (string, error) {
	c := l.c.Load()
	if c == nil {
		return "", ErrNotLoggedIn
	}
	return c.Advise(ctx, question, effort)
}

// Compile-time capability guarantees: the lazy provider must satisfy every
// interface the runtime type-asserts on the raw provider, or signed-in
// sessions would silently lose capabilities.
var (
	_ ModelProvider = (*Lazy)(nil)
	_ Streamer      = (*Lazy)(nil)
	_ WebSearcher   = (*Lazy)(nil)
	_ WebFetcher    = (*Lazy)(nil)
	_ Advisor       = (*Lazy)(nil)
	_ retryNotifier = (*Lazy)(nil)
	_ Endpointer    = (*Lazy)(nil)
)

// Connector is the credential-swap capability the TUI needs from a provider:
// present on *Lazy, absent on test fakes (which count as connected). This is
// the seam runtime.Session.Connected forwards through — and since Phase C,
// Connected means hosted-OR-endpoint (any usable backend); Endpointer (above)
// is the sibling seam that says WHICH.
type Connector interface {
	Connected() bool
	SetCredentials(url, token string)
}

// EffectiveModel resolves a configured tier value (alias or model id) to the
// model REQUESTED of the gateway. Model policy is the gateway's call — it may
// re-target a request at the self-hosted pool — but the requested id still
// matters: it names the tier intent and prices the counterfactual.
func EffectiveModel(s string) string {
	return ResolveAlias(s)
}

// retryNotifier is a structural interface satisfied by *client.Client — it lets
// the runtime wire a retry-notify callback into the gateway transport WITHOUT the
// provider package importing the client concrete type (the runtime holds a
// ModelProvider, not a *client.Client). The method signature matches
// client.Client.SetRetryNotify. This keeps the wiring non-breaking: if the
// provider is ever not a *client.Client (a test fake, a future local backend),
// SetRetryNotify is simply a no-op (the type assertion fails → silent retry).
type retryNotifier interface {
	SetRetryNotify(fn func(attempt int, err error, delay time.Duration))
}

// SetRetryNotify wires a retry-notify callback into the gateway transport (if the
// provider is the SDK client, which it is in production). No-op for any provider
// that doesn't support it (test fakes, a future local backend) — those just get
// silent retry. This is the seam the runtime uses to surface "⊙ retrying…" in the
// TUI without coupling itself to the SDK's concrete client type.
func SetRetryNotify(prov ModelProvider, fn func(attempt int, err error, delay time.Duration)) {
	if rn, ok := prov.(retryNotifier); ok {
		rn.SetRetryNotify(fn)
	}
}
