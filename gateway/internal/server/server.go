// Package server is the HTTP face of the memcode gateway: bearer-token auth in
// front of the metered LLM engine (router → Fireworks + the frontier vendor
// APIs). The wire protocol IS the OpenAI-compat surface (one-wire): POST
// /v1/chat/completions + GET /v1/models at the natural paths — one completion
// (or SSE stream) per call, with the memcode extensions riding as ignorable
// headers/fields (see internal/compat). The agent loop stays client-side; a
// future server-side loop (cloud workspaces, bots) can layer on without
// breaking CLI clients.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/gateway"
	"github.com/memcode-ai/memcode/gateway/internal/advisor"
	"github.com/memcode-ai/memcode/gateway/internal/identity"
	"github.com/memcode-ai/memcode/gateway/internal/llm"
	"github.com/memcode-ai/memcode/gateway/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Config is the service configuration, env-only (keys NEVER in files).
type Config struct {
	Port string // PORT (Cloud Run convention), default 8080
	// InternalToken (MEMCODE_INTERNAL_TOKEN) gates the /internal/* admin surface
	// (cooldown, audit log) — Cloud Scheduler + operators only, NEVER in the CLI.
	// The user token must NOT open these: it's distributed to every client, so a
	// user could otherwise stop the pool or read lifecycle data. Fails CLOSED:
	BackendName string // for the startup log: "hybrid" | "anthropic" | "fireworks"

	// SystemPrefix is the server-owned doctrine prepended to every system
	// prompt — the first concrete piece of "prompts live server-side". The full
	// prompt library migrates here incrementally; this proves the seam.
	SystemPrefix string // MEMCODE_SYSTEM_PREFIX (optional)

	// SelfHostToken is the shared bearer for the default self-host
	// composition (New): every request carrying it runs as one local
	// identity. A hosted composition supplies its own Authenticator instead.
	SelfHostToken string // MEMCODE_SELFHOST_TOKEN

	Provider provider.ModelProvider
}

// ConfigFromEnv builds the service config. Backend selection reuses the same
// env contract as the engine: MEMCODE_PROVIDER=anthropic|fireworks|hybrid plus
// ANTHROPIC_API_KEY / MEMCODE_FIREWORKS_URL / _MODEL / _KEY (legacy
// MEMCODE_VLLM_* names still honored).
func ConfigFromEnv() (Config, error) {
	prov, err := provider.NewFromEnv()
	if err != nil {
		return Config{}, err
	}
	backend := strings.TrimSpace(os.Getenv(provider.EnvProvider))
	if backend == "" {
		backend = "anthropic"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	return Config{
		Port:          port,
		BackendName:   backend,
		SystemPrefix:  os.Getenv("MEMCODE_SYSTEM_PREFIX"),
		SelfHostToken: strings.TrimSpace(os.Getenv("MEMCODE_SELFHOST_TOKEN")),
		Provider:      prov,
	}, nil
}

type handler struct {
	cfg     Config
	ext     gateway.Extensions // composition seams (auth/authz/usage/keys/routes)
	runner  *llm.Runner
	advisor *advisor.Advisor // second-opinion side-channel (OpenAI); nil-safe when unkeyed
	orgRate *orgRateLimiter  // per-principal request floor (MEMCODE_ORG_RPM)
}

// New builds the handler with no composition options — the self-host
// default: a local shared-token Authenticator, allow-everything
// authorization, stdout-only usage, environment provider keys.
func New(cfg Config) http.Handler { return NewWith(cfg, gateway.Extensions{}) }

// NewWith builds the HTTP handler from a config plus a composition. Health is
// unauthenticated (Cloud Run probes); every other route runs behind the
// Authenticator + Authorizer. A hosted operator supplies cloud
// implementations here; nothing hosted-specific lives in this package.
func NewWith(cfg Config, ext gateway.Extensions) http.Handler {
	if ext.Authenticator == nil && cfg.SelfHostToken != "" {
		ext.Authenticator = LocalTokenAuthenticator(cfg.SelfHostToken)
	}
	if ext.Authorizer == nil {
		ext.Authorizer = gateway.AllowAll{}
	}
	h := &handler{
		cfg: cfg, ext: ext, runner: llm.NewRunner(cfg.Provider),
		advisor: advisor.New(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("ANTHROPIC_ADVISOR_MODEL")),
		orgRate: newOrgRateLimiter(),
	}
	// Per-principal provider keys (the byok mechanism) — supplied by the
	// composition. Nil = the gateway's own environment keys are the only keys.
	if ext.KeySource != nil {
		if hy, ok := cfg.Provider.(*provider.Hybrid); ok {
			hy.SetByok(ext.KeySource)
		}
	}
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	// GFE reserves /healthz on direct *.run.app URLs; /health is the
	// externally-reachable liveness path, /healthz kept for fronted probes.
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /healthz", health)
	// The turn surface IS the OpenAI-compat wire, mounted at the natural paths.
	mux.Handle("POST /v1/chat/completions", h.auth(http.HandlerFunc(h.compatChat)))
	mux.Handle("GET /v1/models", h.auth(http.HandlerFunc(h.compatModels)))
	mux.Handle("POST /v1/websearch", h.auth(http.HandlerFunc(h.websearch)))
	mux.Handle("POST /v1/advisor", h.auth(http.HandlerFunc(h.advise)))
	mux.Handle("POST /v1/webfetch", h.auth(http.HandlerFunc(h.webfetch)))
	mux.Handle("GET /v1/status", h.auth(http.HandlerFunc(h.status)))
	// Composition-supplied routes (a hosted operator mounts its own surfaces,
	// e.g. managed key management). Authed routes run behind the same door.
	for _, rt := range ext.Routes {
		hdlr := rt.Handler
		if rt.Authed {
			hdlr = h.auth(hdlr)
		}
		mux.Handle(rt.Pattern, hdlr)
	}
	return mux
}

// auth gates a handler: the composition's Authenticator resolves the request
// to an identity, the Authorizer approves it (typed refusals carry their wire
// status/code via StatusError), then the per-principal rate limit applies. No
// hosted policy lives here — it is entirely in the injected implementations.
func (h *handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.ext.Authenticator == nil {
			httpError(w, http.StatusInternalServerError, "no authenticator configured")
			return
		}
		if bearerOf(r) == "" {
			httpError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		id, err := h.ext.Authenticator.Authenticate(r)
		if err != nil || id == nil {
			httpError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		if err := h.ext.Authorizer.Authorize(r, id); err != nil {
			if se := asStatusError(err); se != nil {
				httpErrorWithCode(w, se.Status, se.Msg, se.Code)
			} else {
				httpError(w, http.StatusForbidden, "not permitted")
			}
			return
		}
		acc := h.ext.Authorizer.Access(id)
		if !h.orgRate.allow(id.ID) {
			h.orgRate.deny(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id, acc)))
	})
}

// turnRequest pairs one turn's request with its telemetry labels for the
// usage/error emitters. Server-local: the one-wire protocol has no turn
// envelope (the compat request IS the wire), and purpose is only ever a
// synthetic side-channel label ("advisor" | "websearch" | …) — main-turn rows
// carry "".
type turnRequest struct {
	Purpose string
	Request wire.Request
}

// meterSideChannel records a metered call made OUTSIDE the main Runner path — the advisor and
// the web tools — into the shared Ledger AND emits a usage line + web-app report. Without this
// these endpoints moved money invisibly (the advisor especially, at frontier pricing). purpose
// is a synthetic label; model is what served the call.
func (h *handler) meterSideChannel(r *http.Request, purpose, model string, resp wire.Response, latency time.Duration) {
	resp.Model = model
	h.runner.Meter(llm.Purpose(purpose), model, resp, latency)
	// Side channels carry the session as a header (the sdk client sets it);
	// turns carry it in the request body — thread it so the emitters read ONE place.
	h.emitUsage(r, turnRequest{Purpose: purpose,
		Request: wire.Request{Model: model, Session: r.Header.Get("X-Memcode-Session")}}, resp, latency)
}

// errorLine is one failed turn as a structured stdout fact — the counterpart emitUsage
// never covered: until this existed the gateway logged ONLY successes, so a classifier
// mode timing out fleet-wide (a CLI-side deadline surfaces here as a canceled request)
// was invisible in Cloud Run logs.
type errorLine struct {
	Event          string `json:"event"` // always "turn_error"
	At             string `json:"at"`
	RequestID      string `json:"request_id"`
	SessionID      string `json:"session_id,omitempty"`
	AccountID      string `json:"account_id"`
	OrgID          string `json:"org_id,omitempty"`
	UserID         string `json:"user_id,omitempty"`
	Purpose        string `json:"purpose,omitempty"`
	RequestedModel string `json:"requested_model"`
	Error          string `json:"error"`
	LatencyMS      int64  `json:"latency_ms"`
	PoolID         string `json:"pool_id,omitempty"`
}

// emitError logs one failed turn. Same pure-JSON-to-stdout contract as emitUsage
// (jsonPayload in Cloud Run, queryable by mode/purpose/error). Doubles as the
// choke point where a BYOK auth rejection flips the key's metadata status to
// 'invalid' (best-effort, off the response path) so listings show the broken key.
func (h *handler) emitError(r *http.Request, tr turnRequest, err error, latency time.Duration) {
	who := identity.From(r.Context())
	line := errorLine{
		Event: "turn_error", At: time.Now().UTC().Format(time.RFC3339),
		RequestID: randID(), SessionID: tr.Request.Session, AccountID: accountID(r),
		OrgID: who.OrgID, UserID: who.UserID,
		Purpose: tr.Purpose, RequestedModel: tr.Request.Model,
		Error: err.Error(), LatencyMS: latency.Milliseconds(),
		PoolID: os.Getenv("MEMCODE_POOL_ID"),
	}
	b, _ := json.Marshal(line)
	fmt.Println(string(b))
}

// usageLine is the generic per-turn usage fact emitted as a stdout JSON line
// (Cloud Run → jsonPayload). It carries counts and routing telemetry only —
// no pricing or commercial accounting (a composition's UsageSink derives
// any commercial accounting on its own side).
type usageLine struct {
	Event                 string  `json:"event"`
	At                    string  `json:"at"`
	RequestID             string  `json:"request_id"`
	SessionID             string  `json:"session_id,omitempty"`
	AccountID             string  `json:"account_id"`
	OrgID                 string  `json:"org_id,omitempty"`
	UserID                string  `json:"user_id,omitempty"`
	Purpose               string  `json:"purpose,omitempty"`
	RequestedModel        string  `json:"requested_model"`
	ServedModel           string  `json:"served_model"`
	Backend               string  `json:"backend"`
	FallbackReason        string  `json:"fallback_reason,omitempty"`
	ToolOrigin            string  `json:"tool_origin,omitempty"`
	InputTokens           int     `json:"input_tokens"`
	OutputTokens          int     `json:"output_tokens"`
	CacheReadTokens       int     `json:"cache_read_tokens"`
	CacheWriteTokens      int     `json:"cache_write_tokens"`
	SearchCount           int     `json:"search_count,omitempty"`
	EstimatedPromptTokens int     `json:"estimated_prompt_tokens,omitempty"`
	EstimateRatio         float64 `json:"estimate_ratio,omitempty"`
	ContextWindow         int     `json:"context_window,omitempty"`
	InputBudget           int     `json:"input_budget,omitempty"`
	LatencyMS             int64   `json:"latency_ms"`
	PoolID                string  `json:"pool_id,omitempty"`
	Byok                  bool    `json:"byok,omitempty"`
	ByokVendor            string  `json:"byok_vendor,omitempty"`
}

// emitUsage logs one usage fact. Best-effort and non-blocking on the response.
func (h *handler) emitUsage(r *http.Request, tr turnRequest, resp wire.Response, latency time.Duration) {
	// Token cost PLUS the native web-search surcharge: each in-turn search bills a
	// per-request fee upstream (models.json search_fees, keyed by serving vendor)
	// Side channels (/v1/websearch, /v1/webfetch) funnel through here too via
	// meterSideChannel. Counts only — a composition's UsageSink derives cost.
	requestID := randID()
	who := identity.From(r.Context())
	line := usageLine{
		Event: "usage", At: time.Now().UTC().Format(time.RFC3339),
		RequestID: requestID, SessionID: tr.Request.Session, AccountID: accountID(r),
		OrgID: who.OrgID, UserID: who.UserID,
		Purpose: tr.Purpose, RequestedModel: tr.Request.Model, ServedModel: resp.Model,
		Backend: resp.Backend, FallbackReason: resp.FallbackReason, ToolOrigin: resp.ToolOrigin,
		InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens,
		CacheReadTokens: resp.CacheReadTokens, CacheWriteTokens: resp.CacheWriteTokens,
		SearchCount:           resp.SearchCount,
		EstimatedPromptTokens: resp.EstimatedPromptTokens, ContextWindow: resp.ContextWindow, InputBudget: resp.InputBudget,
		LatencyMS: latency.Milliseconds(),
		PoolID:    os.Getenv("MEMCODE_POOL_ID"),
		Byok:      resp.BYOK, ByokVendor: resp.BYOKVendor,
	}
	// estimate_ratio = estimated / actual-billed-input (cache reads are real input the
	// estimator should have counted). >1 = over-estimate (safe); <1 = under-estimate
	// (the dangerous direction — a turn we routed too low and had to absorb).
	if actualIn := resp.InputTokens + resp.CacheReadTokens; actualIn > 0 && resp.EstimatedPromptTokens > 0 {
		line.EstimateRatio = float64(resp.EstimatedPromptTokens) / float64(actualIn)
	}
	// Pure JSON to stdout (NO log prefix) so Cloud Run parses it into jsonPayload —
	// queryable by field in Logs Explorer and clean for a BigQuery sink. log.Println's
	// timestamp prefix would break that (lands as opaque textPayload).
	b, _ := json.Marshal(line)
	fmt.Println(string(b))

	// Hand the generic usage event to the composition's sink (a hosted
	// operator derives commercial accounting there — the core carries none).
	if h.ext.UsageSink != nil {
		h.ext.UsageSink.Record(&gateway.Identity{ID: who.OrgID, User: who.UserID}, gateway.UsageEvent{
			RequestID: requestID, Purpose: tr.Purpose,
			RequestedModel: tr.Request.Model, ServedModel: resp.Model, Backend: resp.Backend,
			InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens,
			CacheRead: resp.CacheReadTokens, CacheWrite: resp.CacheWriteTokens,
			SearchCount: resp.SearchCount, LatencyMS: latency.Milliseconds(),
			ViaKeySource: resp.BYOK, KeyVendor: resp.BYOKVendor,
		})
	}
}

// accountID is a stable, non-reversible id for the caller. Reads the org ID
// from context (set by auth()); the token-hash fallback only fires on paths
// that never passed auth (defensive — every real handler is auth-gated).
func accountID(r *http.Request) string {
	if orgID := orgIDFromContext(r.Context()); orgID != "" {
		return orgID
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		return "anon"
	}
	sum := sha256.Sum256([]byte(tok))
	return "tok_" + hex.EncodeToString(sum[:6])
}

// randID returns a UUIDv4. The id is BOTH the usage-log request_id and the credit
// idempotency key downstream sinks may use — a Postgres uuid column, so this
// MUST be a valid UUID (a sink may write it to a Postgres uuid column).
// Uniqueness is load-bearing: a REPEATED id would let an idempotent sink
// dedupe away real events.
func randID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand effectively never fails; a timestamp-derived fallback keeps
		// ids unique (a constant fallback would dedupe real events — see above).
		now := uint64(time.Now().UnixNano())
		binary.BigEndian.PutUint64(b[:8], now)
		binary.BigEndian.PutUint64(b[8:], now^0x9e3779b97f4a7c15)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// gateSideChannel refuses a side-channel call (advisor/websearch/webfetch) when
// serving is limited to keyed vendors. Side-channels always serve on the
// gateway's own keys — no keyed lane covers them — so a usage-limited
// identity cannot legitimately proceed. This closes the advisor hole: it
// bypasses provider.Hybrid (and therefore the keyed gate) entirely.
func (h *handler) gateSideChannel(w http.ResponseWriter, r *http.Request) bool {
	if identity.From(r.Context()).LimitToKeyed {
		httpErrorWithCode(w, http.StatusPaymentRequired,
			"credits exhausted — add credits at memcode.ai/account/billing", codeInsufficientCredit)
		return false
	}
	return true
}

// webCapability resolves the provider that can do server-side web work. The
// hybrid router and the Anthropic provider both implement the capabilities;
// a fireworks-only deployment doesn't — callers get a clean 501.
func (h *handler) webSearcher() (provider.WebSearcher, bool) {
	ws, ok := h.cfg.Provider.(provider.WebSearcher)
	return ws, ok
}

func (h *handler) webFetcher() (provider.WebFetcher, bool) {
	wf, ok := h.cfg.Provider.(provider.WebFetcher)
	return wf, ok
}

// websearch proxies the strong provider's server-side web_search capability.
// Metered like the advisor: the provider returns the usage it billed, and we
// record it even on error (a failed call still costs whatever ran).
func (h *handler) websearch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || in.Query == "" {
		httpError(w, http.StatusBadRequest, "query is required")
		return
	}
	ws, ok := h.webSearcher()
	if !ok {
		httpError(w, http.StatusNotImplemented, "web search is not available on this gateway's backend")
		return
	}
	if !h.gateSideChannel(w, r) {
		return
	}
	t0 := time.Now()
	text, usage, err := ws.WebSearch(r.Context(), in.Query)
	h.meterSideChannel(r, "websearch", usage.Model, usage, time.Since(t0))
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]string{"text": text})
}

// advise is the second-opinion side-channel: it forwards a question/plan to the
// Anthropic advisor (Claude Opus, adaptive thinking on) and returns the advice. effort
// defaults to high for /advisor; plan-mode reviews pass "low"/"medium".
func (h *handler) advise(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Question string `json:"question"`
		Effort   string `json:"effort,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil || strings.TrimSpace(in.Question) == "" {
		httpError(w, http.StatusBadRequest, "question is required")
		return
	}
	if h.advisor == nil || !h.advisor.Available() {
		httpError(w, http.StatusNotImplemented, "advisor is not configured (no ANTHROPIC_API_KEY on the gateway)")
		return
	}
	if !h.gateSideChannel(w, r) {
		return
	}
	t0 := time.Now()
	advice, usage, err := h.advisor.AdviseMetered(r.Context(), in.Question, in.Effort)
	// Meter whatever the advisor billed, even on error (a partial/failed frontier call still
	// costs) — this closes the biggest invisible-spend hole in the gateway.
	h.meterSideChannel(r, "advisor", usage.Model, usage, time.Since(t0))
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]string{"advice": advice})
}

// webfetch proxies the strong provider's server-side web_fetch capability.
// Metered like websearch/advisor (usage recorded even on error).
func (h *handler) webfetch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || in.URL == "" {
		httpError(w, http.StatusBadRequest, "url is required")
		return
	}
	wf, ok := h.webFetcher()
	if !ok {
		httpError(w, http.StatusNotImplemented, "web fetch is not available on this gateway's backend")
		return
	}
	if !h.gateSideChannel(w, r) {
		return
	}
	t0 := time.Now()
	text, usage, err := wf.WebFetch(r.Context(), in.URL)
	h.meterSideChannel(r, "webfetch", usage.Model, usage, time.Since(t0))
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]string{"text": text})
}

// status reports backend + ledger totals + the SHARED backend state (the
// durable control plane — same truth every instance sees), so routing behavior
// is inspectable independent of which Cloud Run instance answers.
func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	total := h.runner.Ledger().Total()
	out := map[string]any{
		"backend":    h.cfg.BackendName,
		"by_backend": h.runner.Ledger().ByBackend(),
		"total": map[string]any{
			"calls": total.Calls, "in": total.In, "out": total.Out,
			"cache_read": total.CacheRead, "cache_write": total.CacheWrite,
			"usd": total.USD,
		},
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Printf(`{"event":"encode_error","error":"%s"}`+"\n", err.Error())
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": msg}); err != nil {
		fmt.Printf(`{"event":"encode_error","error":"%s"}`+"\n", err.Error())
	}
}

// httpErrorWithCode writes a JSON error with a machine-readable code field, so the
// client SDK can map it to a sentinel error (e.g. insufficient_credits → ErrInsufficientCredit).
func httpErrorWithCode(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code}); err != nil {
		fmt.Printf(`{"event":"encode_error","error":"%s"}`+"\n", err.Error())
	}
}

// codeContextOverflow is the machine-readable error code the CLI keys on (HTTP 413
// body + mid-stream error event) to trigger compact-and-retry.
const codeContextOverflow = "context_overflow"

// codeInsufficientCredit is the machine-readable error code for an exhausted credit
// usage limit (HTTP 402 body), mapped to wire.ErrInsufficientCredit by the client SDK.
const codeInsufficientCredit = "insufficient_credits"

// codeByokKeyFailed marks a turn killed by the user's own provider key (422 —
// non-retryable; the fix is /apikeys, not another attempt).
const codeByokKeyFailed = "byok_key_failed"
