package wire

import (
	"encoding/base64"
	"encoding/json"
)

// This file is the gateway wire contract — the Anthropic-Messages-shaped types
// that cross the cli↔api boundary, declared ONCE. The cli and api each redeclared
// them before; the drift caused production outages (tool input_schema, lane bypass).

// MediaSource carries base64 image/document data for vision blocks.
type MediaSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // e.g. image/png, application/pdf
	Data      string `json:"data"`
}

// CacheControl marks a prompt-cache breakpoint. Everything from the start of the
// prompt up to and including the marked block is cached (5-min ephemeral TTL); later
// requests sharing that prefix read it instead of re-paying input tokens, and cache
// reads are excluded from the input-tokens/min rate limit.
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
	// TTL is the cache lifetime: "" (default 5m) or "1h". The long TTL is used for the
	// STABLE doctrine+tools prefix, which is byte-identical across turns even as the
	// volatile facts (room/personality) change — so it survives interactive gaps >5min.
	TTL string `json:"ttl,omitempty"`
}

// Block is one content block in a message (text, image/document, a tool call, or a
// tool result, or a thinking block).
type Block struct {
	Type string `json:"type"` // "text" | "image" | "document" | "tool_use" | "tool_result"
	Text string `json:"text,omitempty"`

	// image / document
	Source *MediaSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// ContentBlocks carries STRUCTURED content for a tool_result — one or more
	// content blocks (text and/or image) instead of the flat Content string. This
	// is the path for tool results that include vision (e.g. a browser screenshot
	// returned to the model as an image block). When non-empty, providers emit the
	// structured content union (Anthropic: text+image parts in the tool_result;
	// OpenAI: multi-part tool message). When empty, Content (flat string) is used
	// — backwards compatible with every text-only tool result.
	ContentBlocks []Block `json:"content_blocks,omitempty"`

	// thinking (extended/adaptive reasoning). These MUST round-trip UNMODIFIED: the
	// API requires the assistant's thinking blocks to be passed back verbatim on the
	// next tool-use turn (it verifies them via Signature) or it rejects the request.
	// So we parse them off responses and re-send them; we just don't display them.
	// Data carries a redacted_thinking block's opaque payload, if one ever appears.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`

	// prompt caching (set on the wire only, never on shared history — see the gateway)
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// MarshalJSON serializes a Block for the wire. Thinking blocks MUST carry the
// "thinking" field even when its text is EMPTY: adaptive thinking frequently returns a
// block that is just a signature (the reasoning text omitted), and on the next tool-use
// turn Anthropic rejects a thinking block without the field — "thinking.thinking: Field
// required". The struct tag is `omitempty` (correct for every OTHER block type, which
// must NOT carry a stray empty "thinking"), so we special-case thinking / redacted_
// thinking here and let the alias handle the rest with default tags.
func (b Block) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "thinking":
		// id round-trips the provider's reasoning-item id (OpenAI's "rs_…"): the
		// Responses API requires each reasoning item to come back with its ORIGINAL,
		// UNIQUE id in stateless mode, so it must survive the CLI↔gateway hop. Harmless
		// for Anthropic (its buildWire builds thinking from thinking/signature only and
		// never reads id).
		return json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id,omitempty"`
			Thinking  string `json:"thinking"` // NOT omitempty — must be present even when ""
			Signature string `json:"signature,omitempty"`
		}{b.Type, b.ID, b.Thinking, b.Signature})
	case "redacted_thinking":
		return json.Marshal(struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{b.Type, b.Data})
	}
	type alias Block
	return json.Marshal(alias(b))
}

// ImageBlock builds a vision block from raw image bytes.
func ImageBlock(mediaType string, data []byte) Block {
	return Block{Type: "image", Source: &MediaSource{
		Type: "base64", MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data),
	}}
}

// DocumentBlock builds a document block from raw file bytes (application/pdf is
// the interoperable case: Anthropic document source, OpenAI Responses input_file,
// Gemini inline blob). Models without native document input absorb the turn to a
// capable tier — the catalog's pdf flag gates that in the router.
func DocumentBlock(mediaType string, data []byte) Block {
	return Block{Type: "document", Source: &MediaSource{
		Type: "base64", MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data),
	}}
}

// TextBlock builds a text block.
func TextBlock(text string) Block { return Block{Type: "text", Text: text} }

// ToolResultBlocks builds a tool_result block carrying structured content blocks
// (text and/or image) instead of a flat string. This is the path for tool results
// that include vision — e.g. a browser screenshot returned as an image block.
// When blocks is empty, falls back to an empty text result.
func ToolResultBlocks(toolUseID string, blocks []Block, isError bool) Block {
	return Block{Type: "tool_result", ToolUseID: toolUseID, ContentBlocks: blocks, IsError: isError}
}

// Message is one turn (a role plus its content blocks).
type Message struct {
	Role   string  `json:"role"` // "user" | "assistant"
	Blocks []Block `json:"content"`
}

// ToolDef describes a tool the model may call.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`

	// prompt caching — set on the LAST tool only, on the wire (see the gateway)
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// Effort is an abstract reasoning-depth setting. It exists so call sites express
// INTENT ("this is a judgment call, think harder") without hardcoding a provider
// shape that differs by model — e.g. Opus 5 rejects manual budget_tokens and
// requires adaptive thinking + an effort hint, while Opus 4.5 wants budget_tokens.
type Effort string

const (
	EffortOff    Effort = ""       // no extended thinking (default; cheap paths)
	EffortLow    Effort = "low"    // light reasoning
	EffortMedium Effort = "medium" // judgment calls (edits, reflection)
	EffortHigh   Effort = "high"   // hardest reasoning (planning, review)
)

// RoutingHint carries the escalation signal only the CLI can observe — the
// Risk input to the CLI's OWN semantic ladder (cli/internal/llm/lane.go):
// "self_heal" | "agent_frontier" | "agent_strong" | "user_friction_high" |
// "high_risk_surface" | the plan escalation reasons. It never rides any wire;
// it is a request-local field the Runner's selection policy reads. (The old
// PreferredPath/Force fields were read by nothing — deleted.)
type RoutingHint struct {
	Reason string `json:"reason,omitempty"`
}

// Request is a model completion request — the provider-neutral shape every
// adapter encodes from. Model selection is the CLI's job (llm/lane.go +
// llm/resolve.go): the Runner's policy resolves a catalog label into Pin, and
// the transport writes it into the wire `model` field. Server-side, Model
// carries the gateway's raw provider id after the label gate resolves it.
type Request struct {
	Model    string    `json:"model,omitempty"` // resolved id at the serving edge; clients carry their choice in Pin
	System   string    `json:"system,omitempty"`
	Messages []Message `json:"messages"`

	// SystemVolatile is the per-turn-variable doctrine suffix (room/personality/
	// extra-mile/nudge + the turn-scoped extra) split OUT of System so it sits
	// OUTSIDE the cached prefix. The CLI's doctrine composer fills it (directly,
	// or via the transport's compose hook from Mode+Facts); adapters place it
	// cache-safely — Anthropic as a second, uncached system block, chat/
	// completions as a trailing system message, so the stable prefix still
	// auto-caches.
	SystemVolatile string    `json:"system_volatile,omitempty"`
	Tools          []ToolDef `json:"tools,omitempty"`
	MaxTokens      int       `json:"max_tokens,omitempty"`

	// ToolChoice, when set to a tool name, FORCES the model to call that tool — the
	// reliable cross-provider path to structured output (Anthropic tool_choice / OpenAI
	// tool_choice). Used by the plan reviewer's verdict so the JSON comes back in a tool_use
	// block instead of best-effort prose-JSON. "" = the model chooses (the common case).
	ToolChoice string `json:"tool_choice,omitempty"`

	// Mode + Facts select a doctrine prompt: the transport's compose hook
	// (cli/internal/doctrine) renders the mode's doctrine with these gathered
	// facts (root, platform, shell, overview/pack, room, nudge) into System/
	// SystemVolatile just before encoding. Mode "" = raw transport (no
	// composition; System passes through as-is).
	Mode  string            `json:"mode,omitempty"`
	Facts map[string]string `json:"facts,omitempty"`

	// Effort is the ABSTRACT reasoning-depth knob. The provider maps it to whatever
	// thinking control the target model actually supports (adaptive+effort, manual
	// budget_tokens, or nothing). Zero value (EffortOff) means no extended thinking.
	Effort Effort `json:"effort,omitempty"`

	// RoutingHint is the session layer's escalation signal (ROUTING.md): user
	// friction/mood, a self-healing retry after the agent's own edit broke, a
	// high-risk surface. The Runner folds its Reason into Intent.Risk for the
	// selection ladder. nil = no opinion (the common case).
	RoutingHint *RoutingHint `json:"routing_hint,omitempty"`

	// Purpose + Session are transport-local labels (json:"-": never marshaled on
	// this type). Purpose labels the call for the client ledger and the lane
	// leak-log. Session rides the compat wire as the standard `user` field for
	// serving affinity (prefix-cache locality) and gateway telemetry. Stamped by
	// the CLI's llm.Runner; never set by hand.
	Purpose string `json:"-"`
	Session string `json:"-"`

	// Difficulty is the turn_intent judge's tier verdict ("lookup" | "standard"
	// | "deep"), a CLI-side selection input (json:"-", read by the CLI's own
	// resolution policy — it never rides the wire).
	Difficulty string `json:"-"`

	// Pin is the CLI-side carrier for the session's model choice: the resolved
	// catalog LABEL the transport puts in the wire `model` field. Under
	// all-policy-client-side the CLI's selection policy stamps it on EVERY
	// call (Automatic is client behavior); "" is only ever seen by test fakes.
	Pin string `json:"-"`

	// BillingLane is the requested billing lane: "" / "byok_preferred" |
	// "byok_only" | "credits". Client-side it is set by policy (the consented
	// credits retry after a BYOK key failure) and rides the wire as the
	// memcode_billing extension; server-side the compat handler re-stamps it
	// from that extension and the gateway ENFORCES it — it never silently
	// reroutes between the user's keys and credits. NOT marshaled on this type.
	BillingLane string `json:"-"`
	// LaneBypass forces a turn OFF its family lane after an explicit,
	// consented exhaustion choice: "gateway" serves it on the hosted base.
	// Client-side routing state only — never serialized to any wire.
	LaneBypass string `json:"-"`
}

// Response is a model completion result, plus the serving telemetry the footer/
// ledger read. On the hosted backend the gateway stamps the serving facts (via
// the memcode response extension); in direct-endpoint mode the CLI's recovery
// policy stamps them itself (finalize) — same fields, one reader.
type Response struct {
	StopReason   string  `json:"stop_reason"` // "end_turn" | "tool_use" | "max_tokens"
	Blocks       []Block `json:"blocks"`      // assistant content (text and/or tool_use)
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`

	// prompt-cache telemetry (tokens written to / read from the cache this call)
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`

	// SearchCount is the number of native server-side web searches the serving
	// vendor billed on this call (Anthropic usage.server_tool_use.web_search_requests;
	// OpenAI/Grok Responses web_search_call output items). Searches carry a
	// PER-REQUEST fee on top of tokens — the gateway prices it via
	// common.SearchFeeUSD(Backend, SearchCount) when it emits cost_usd.
	SearchCount int `json:"search_count,omitempty"`

	// ToolOrigin records how a tool call was obtained: "structured" (the model's native
	// tool_calls — the intended path) or "salvaged_*" (the gateway recovered it from
	// text a brittle parser missed). Surfaced so salvage stays OBSERVABLE, never silently
	// the main contract.
	ToolOrigin string `json:"tool_origin,omitempty"`

	// Serving telemetry — the ledger's ground truth for WHO served this call.
	// Model is the model that actually ran (a fallback-chain hop can differ from
	// the requested label). Backend is the serving vendor: "cheap" (the hosted
	// cheap lane, vendor-neutral — the inference vendor never rides the wire) |
	// "anthropic" | "openai" | "gemini" | "grok" | "unknown" (label missing from
	// the client catalog); "vllm" is the legacy cheap-lane tag an un-redeployed
	// gateway sends (clients accept both). FallbackReason names why the served
	// model differs from the primary choice: a client-side absorb ("vision" |
	// "pdf" | "window") or a recovery hop ("model_error: …").
	Model          string `json:"model,omitempty"`
	Backend        string `json:"backend,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`

	// RequestedModel is the label the selection policy chose BEFORE any absorb/
	// fallback — the basis for the ledger's counterfactual "what the primary
	// would have cost". The CLI's finalize stamps it (the gateway echoes the
	// requested id on hosted turns); it equals Model when nothing intervened.
	RequestedModel string `json:"requested_model,omitempty"`

	// ContextWindow is the serving backend's real input window in tokens, when
	// the backend reports one (0 when the client's catalog already knows it).
	ContextWindow int `json:"context_window,omitempty"`

	// InputBudget is the serving lane's usable INPUT budget (window − output reserve −
	// safety margin) — the size a prompt must fit under to land on THIS lane. The CLI
	// learns the cheap lane's budget from this and aims compaction at it. 0 when not
	// lane-served.
	InputBudget int `json:"input_budget,omitempty"`

	// Pool is the cheap lane's served-model short label ("glm-5p2" | "kimi-k3"),
	// the CLI's ⇄ ServedBy tag for lane serves. EstimatedPromptTokens is the
	// server's pre-call size estimate (stamped per serve so the usage log's
	// EstimateRatio can calibrate the estimator against real InputTokens).
	Pool                  string `json:"pool,omitempty"`
	EstimatedPromptTokens int    `json:"estimated_prompt_tokens,omitempty"`

	// BYOK telemetry. BYOK (whether this call ran on the user's OWN provider
	// key) rides the wire — it is the key owner's own information, shown by the
	// CLI footer strictly per-turn. BYOKVendor stays SERVER-INTERNAL (json:"-"):
	// the cheap lane's inference vendor never leaves the server (SanitizeResponse
	// zeroes it defensively). The gateway reads both for metering (usage line +
	// zero-debit policy) before sanitizing.
	BYOK       bool   `json:"byok,omitempty"`
	BYOKVendor string `json:"-"`
}

// Text returns the concatenated text blocks of a response.
func (r Response) Text() string {
	var out string
	for _, bl := range r.Blocks {
		if bl.Type == "text" && bl.Text != "" {
			if out != "" {
				out += "\n"
			}
			out += bl.Text
		}
	}
	return out
}

// ToolUses returns the tool_use blocks of a response.
func (r Response) ToolUses() []Block {
	var out []Block
	for _, bl := range r.Blocks {
		if bl.Type == "tool_use" {
			out = append(out, bl)
		}
	}
	return out
}

// StreamHandler receives incremental events during a streamed completion. Both
// callbacks are optional. Text fires for each chunk of assistant text as it arrives;
// Usage fires when the API reports token counts (input at the start, authoritative
// output near the end — NOT per delta, so a live "tokens so far" display must estimate
// from streamed text between Usage calls and reconcile when Usage fires).
//
// This is a data type (a struct of callbacks), part of the wire contract's shape.
// The capability INTERFACES (ModelProvider, Streamer, WebSearcher, WebFetcher, Advisor)
// are intentionally NOT declared here — each consumer declares the structural interface
// it needs, referencing these common types; an implementation merely satisfies it.
type StreamHandler struct {
	Text  func(delta string)
	Usage func(input, output int)
}
