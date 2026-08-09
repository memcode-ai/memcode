package server

// compat.go — the turn surface of the gateway (the one-wire architecture):
// POST /v1/chat/completions + GET /v1/models. The OpenAI-compat wire IS the
// turn API, mounted at the natural paths — an ORDINARY OpenAI client
// configured with base_url https://code.memcode.ai/v1 just works. Both routes
// sit behind the existing h.auth door — a memcode_ org key is the API key.
//
// The gateway is a METERED SERVING ENDPOINT, not a routing authority
// (all-policy-client-side, 2026-08-08): the CLI is the agent — it picks the
// concrete model, escalates, falls back, and recovers; this handler serves
// exactly the requested catalog label or declines with a typed error. What
// lives here is enforcement and translation: auth/entitlement (402s that
// decline, never redirect), the strict label gate (400 unknown_model), BYOK
// key injection per the billing-lane extension, metering, sanitization.
//
// GET /v1/models is the hosted routing CONTROL PLANE: every server-side fact
// the CLI's selection policy needs (labels, vendor identity, capabilities,
// windows, byok coverage, credits state, role config) is served there —
// anything missing gets added there explicitly, never smuggled back into
// gateway routing.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/gateway/internal/compat"
	"github.com/memcode-ai/memcode/gateway/internal/identity"
	"github.com/memcode-ai/memcode/gateway/internal/llm"
	"github.com/memcode-ai/memcode/gateway/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// codeUnknownModel is the machine-readable 400 code for a model id the compat
// endpoint refuses (not a servable catalog label).
const codeUnknownModel = "unknown_model"

// codeModelCapability is the machine-readable 400 code for a turn the
// requested model cannot take (image on a no-vision model, PDF on a model
// without document input). The CLI pre-checks these from the shared catalog;
// this is the enforcement backstop.
const codeModelCapability = "model_capability"

// billingLanes is the memcode_billing extension's value set. The gateway
// ENFORCES the requested lane (byokroute.go laneByok); it never chooses one.
var billingLanes = map[string]bool{"": true, "byok_preferred": true, "byok_only": true, "credits": true}

// compatModels serves GET /v1/models — the control plane: the standard list
// shape whose ids are every catalog label this deployment can serve for the
// requesting user (byok-aware), each entry extended with an ignorable
// `memcode` object of selection facts (vendor/window/vision/pdf/reasoning/
// pinnable/byok), plus a list-level `memcode` object (credits_exhausted,
// backend, vendors — first entry is the deployment default — and the role
// config). Raw provider ids never appear — labels only.
func (h *handler) compatModels(w http.ResponseWriter, r *http.Request) {
	who := identity.From(r.Context())
	var entries []compat.ModelEntry
	for _, p := range provider.ServableModelsFor(who) {
		entries = append(entries, compat.ModelEntryFrom(p.Label, compat.ModelMeta{
			Name: p.Name, Desc: p.Desc, Group: p.Group,
			Vendor: p.Vendor, Window: p.Window, Vision: p.Vision, PDF: p.PDF,
			Reasoning: p.Reasoning, Pinnable: p.Pinnable, Byok: p.Byok}))
	}
	ext := &compat.ModelsExt{
		CreditsExhausted: who.LimitToKeyed,
		Backend:          h.cfg.BackendName,
		Vendors:          provider.ConfiguredVendors(),
	}
	for _, rm := range provider.ConfiguredModels() {
		ext.Roles = append(ext.Roles, compat.RoleEntry{
			Role: rm.Role, ID: rm.ID, Label: rm.Label, Window: rm.Window, Vision: rm.Vision,
		})
	}
	writeJSON(w, compat.ModelListFrom(entries, ext))
}

// compatChat serves POST /v1/chat/completions.
func (h *handler) compatChat(w http.ResponseWriter, r *http.Request) {
	var req compat.ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
		compatError(w, http.StatusBadRequest, "decoding request: "+err.Error(), "invalid_request_error", "")
		return
	}
	if len(req.Messages) == 0 {
		compatError(w, http.StatusBadRequest, "messages is required", "invalid_request_error", "")
		return
	}

	// The strict model gate: a servable catalog label, nothing else. "" is
	// unknown too — model in the body is a HARD requirement of the compat
	// contract. There is no server-side Automatic: the agent decides what to
	// ask for; the gateway serves it or declines.
	label := strings.TrimSpace(req.Model)
	rawID, ok := provider.LookupServable(label)
	if !ok {
		compatError(w, http.StatusBadRequest,
			fmt.Sprintf("unknown model %q — send a model id from GET /v1/models", label),
			"invalid_request_error", codeUnknownModel)
		return
	}

	// The billing-lane extension: enforced, never chosen (byokroute.go).
	lane := strings.TrimSpace(req.MemcodeBilling)
	if !billingLanes[lane] {
		compatError(w, http.StatusBadRequest,
			fmt.Sprintf("unknown memcode_billing %q — send byok_preferred, byok_only, or credits", lane),
			"invalid_request_error", "")
		return
	}

	turn, err := compat.ToTurn(req)
	if err != nil {
		compatError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return
	}

	ctx := r.Context()
	tr := turnRequest{Request: wire.Request{
		Model:          rawID,
		BillingLane:    lane,
		System:         turn.System,
		SystemVolatile: turn.SystemVolatile,
		Messages:       turn.Messages,
		Tools:          turn.Tools,
		ToolChoice:     turn.ToolChoice,
		Effort:         turn.Effort,
		MaxTokens:      turn.MaxTokens,
	}}

	// Session affinity: the standard `user` field — the emitters and the
	// cheap lane's sticky routing read it from the request.
	tr.Request.Session = strings.TrimSpace(req.User)

	// Global server prefix, prepended before the client's stable system text.
	// (The ONE piece of server-side prompt text left — an ops knob, not
	// doctrine; empty in prod.)
	if h.cfg.SystemPrefix != "" {
		if tr.Request.System != "" {
			tr.Request.System = h.cfg.SystemPrefix + "\n\n" + tr.Request.System
		} else {
			tr.Request.System = h.cfg.SystemPrefix
		}
	}

	if req.Stream {
		h.compatStream(w, r, tr)
		return
	}

	// Non-streamed: partial usage still bills on error, sanitize AFTER
	// metering.
	t0 := time.Now()
	resp, err := h.runner.Complete(ctx, llm.Purpose(tr.Purpose), tr.Request)
	if err != nil {
		if resp.InputTokens > 0 || resp.OutputTokens > 0 || resp.CacheReadTokens > 0 || resp.CacheWriteTokens > 0 {
			resp.RequestedModel = tr.Request.Model
			h.emitUsage(r, tr, resp, time.Since(t0))
		}
		h.emitError(r, tr, err, time.Since(t0))
		compatTurnError(w, err)
		return
	}
	resp.RequestedModel = tr.Request.Model
	h.emitUsage(r, tr, resp, time.Since(t0))
	provider.SanitizeResponse(&resp) // strip the provider path AFTER metering — the client never sees the vendor
	writeJSON(w, compat.ResponseFrom(resp, compatID(), time.Now().Unix()))
}

// compatStream writes the standard SSE chunk stream: a role chunk, content
// deltas as they arrive, tool-call deltas, a finish chunk, a final usage chunk
// carrying the memcode extension, then [DONE]. Tool calls stream as one full
// delta each (the internal wire assembles them in the final response) — a legal
// chunking that accumulates exactly like incremental argument deltas.
func (h *handler) compatStream(w http.ResponseWriter, r *http.Request, tr turnRequest) {
	fl, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	id, created := compatID(), time.Now().Unix()
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if canFlush {
			fl.Flush()
		}
	}
	done := func() {
		fmt.Fprint(w, "data: [DONE]\n\n")
		if canFlush {
			fl.Flush()
		}
	}
	chunk := func(model string, choices []compat.ChunkChoice) compat.ChatChunk {
		return compat.ChunkFrom(id, created, model, choices)
	}
	// Until the served model is known, chunks carry the requested model's
	// client-facing label (never the raw provider id).
	reqLabel := provider.SanitizeModelID(tr.Request.Model)

	empty := ""
	send(chunk(reqLabel, []compat.ChunkChoice{{Delta: compat.Delta{Role: "assistant", Content: &empty}}}))

	t0 := time.Now()
	resp, err := h.runner.Stream(r.Context(), llm.Purpose(tr.Purpose), tr.Request, wire.StreamHandler{
		Text: func(d string) {
			send(chunk(reqLabel, []compat.ChunkChoice{{Delta: compat.Delta{Content: &d}}}))
		},
	})
	if err != nil {
		// Partial-usage-on-error contract: a cut stream still bills whatever
		// the provider reported.
		if resp.InputTokens > 0 || resp.OutputTokens > 0 || resp.CacheReadTokens > 0 || resp.CacheWriteTokens > 0 {
			resp.RequestedModel = tr.Request.Model
			h.emitUsage(r, tr, resp, time.Since(t0))
		}
		h.emitError(r, tr, err, time.Since(t0))
		send(compat.ErrorFrom(err.Error(), "server_error", compatErrCode(err)))
		done()
		return
	}
	resp.RequestedModel = tr.Request.Model
	h.emitUsage(r, tr, resp, time.Since(t0))
	provider.SanitizeResponse(&resp)

	for i, tc := range compat.ToolCallsFrom(resp.ToolUses()) {
		fn := tc.Function
		send(chunk(resp.Model, []compat.ChunkChoice{{Delta: compat.Delta{ToolCalls: []compat.ToolCallDelta{
			{Index: i, ID: tc.ID, Type: "function", Function: &fn},
		}}}}))
	}
	// Reasoning blocks ride a delta too, so streaming clients can round-trip
	// them via memcode_opaque exactly like the non-streamed body.
	if opq := compat.OpaqueFrom(resp.Blocks); len(opq) > 0 {
		send(chunk(resp.Model, []compat.ChunkChoice{{Delta: compat.Delta{MemcodeOpaque: opq}}}))
	}
	fr := compat.FinishReasonFrom(resp.StopReason)
	send(chunk(resp.Model, []compat.ChunkChoice{{Delta: compat.Delta{}, FinishReason: &fr}}))
	// Final usage chunk (the include_usage shape: empty choices + usage) + the
	// memcode extension object.
	u := compat.UsageFrom(resp)
	final := chunk(resp.Model, []compat.ChunkChoice{})
	final.Usage = &u
	final.Memcode = compat.ExtFrom(resp)
	send(final)
	done()
}

// compatTurnError maps a provider error onto the standard error envelope with
// machine-readable codes, so the CLI's recovery policy keys on stable signals:
// a context-window overflow is 413 context_overflow (compact-then-retry), a
// capability mismatch 400 model_capability (pick a capable model), a BYOK key
// failure 422 byok_key_failed (the CLI decides — /apikeys, or an explicit
// consented credits retry via memcode_billing), a usage-limited 402
// insufficient_quota + insufficient_credits, everything else 502 (the
// upstream call failed — fallback-chain territory for the CLI).
func compatTurnError(w http.ResponseWriter, err error) {
	switch {
	case provider.IsContextOverflow(err):
		compatError(w, http.StatusRequestEntityTooLarge, err.Error(), "invalid_request_error", codeContextOverflow)
	case provider.AsCapabilityError(err) != nil:
		compatError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", codeModelCapability)
	case provider.AsByokError(err) != nil:
		compatError(w, http.StatusUnprocessableEntity, err.Error(), "invalid_request_error", codeByokKeyFailed)
	case provider.AsCreditsExhausted(err) != nil:
		compatError(w, http.StatusPaymentRequired, err.Error(), "insufficient_quota", codeInsufficientCredit)
	default:
		compatError(w, http.StatusBadGateway, err.Error(), "server_error", "")
	}
}

// compatErrCode is the mid-stream error-code twin of compatTurnError.
func compatErrCode(err error) string {
	switch {
	case provider.IsContextOverflow(err):
		return codeContextOverflow
	case provider.AsCapabilityError(err) != nil:
		return codeModelCapability
	case provider.AsByokError(err) != nil:
		return codeByokKeyFailed
	case provider.AsCreditsExhausted(err) != nil:
		return codeInsufficientCredit
	}
	return ""
}

// compatError writes the standard {"error":{message,type,code}} envelope.
func compatError(w http.ResponseWriter, status int, msg, typ, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(compat.ErrorFrom(msg, typ, code)); err != nil {
		fmt.Printf(`{"event":"encode_error","error":"%s"}`+"\n", err.Error())
	}
}

// compatID mints a chat-completion id.
func compatID() string { return "chatcmpl-" + randID() }
