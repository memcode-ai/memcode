// Package compatwire is the OpenAI-compat chat/completions dialect — the ONE
// set of wire-type declarations (used by the CLI transport, the gateway's
// inbound surface, the lane client, and the conformance suite) and the ONE
// client engine for the dialect (encode/decode/SSE/retry, with the optional
// memcode extensions, tool-call salvage, and the lane error contract as
// configuration). Extracted from the gateway's internal/compat (types) + the
// CLI's transport + the gateway's Fireworks lane client — one implementation
// per protocol, shared by every consumer.
// Package compat is the OpenAI-compat wire: the chat-completions request/
// response/chunk shapes the gateway's {prefix}/chat/completions surface speaks,
// plus the pure translation between that wire and the internal common.Request/
// common.Response protocol (translate.go).
//
// This IS the one-wire architecture (plans/flickering-soaring-falcon): the
// memcode base URL behaves exactly like an OpenAI-compatible endpoint — an
// ordinary OpenAI client pointed at https://api.memcode.ai/v1 works with zero
// memcode-specific transport branches. The turn surface is POST
// /v1/chat/completions + GET /v1/models; there is no memcode-shaped turn wire
// anymore. The memcode extensions are all optional and ignorable by
// third-party tooling:
//
//  1. system-messages convention: first system message = cacheable stable
//     prefix, second = volatile suffix (3+ concatenate into volatile);
//  2. `memcode_billing` on the request — the billing-lane the gateway ENFORCES
//     (byok_preferred | byok_only | credits; never chosen server-side). The
//     standard `user` field carries session/cache affinity;
//  3. assistant messages may carry a memcode_opaque array (vendor reasoning
//     blocks round-tripped verbatim; the gateway re-expands them);
//  4. a `memcode` object on the final response/chunk (byok, fallback_reason,
//     search_count, context_window, input_budget, pool, session_phase);
//  5. a `memcode` object per GET /v1/models entry + on the list itself — the
//     routing CONTROL PLANE the CLI's selection policy runs on (vendor,
//     capabilities, byok coverage, credits_exhausted, vendors, roles).
//
// The same types serve both directions: the gateway decodes inbound requests
// with them, and the conformance suite (compat/conformance) marshals outbound
// requests with them against arbitrary endpoints — so a shape drift from the
// real ecosystem fails conformance, not production.
package compat

import (
	"bytes"
	"encoding/json"
)

// ── request ─────────────────────────────────────────────────────────────────

// ChatRequest is POST {prefix}/chat/completions. Decode is deliberately loose:
// standard knobs the gateway doesn't honor (temperature, top_p, n, …) are
// accepted and ignored — the gateway owns sampling — rather than rejected.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`

	Tools []Tool `json:"tools,omitempty"`
	// ToolChoice is the standard union: "auto" | "none" | "required" |
	// {"type":"function","function":{"name":…}} — kept raw and interpreted in
	// translate.go (the forced-tool form is what the classifiers depend on).
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`

	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	// MaxCompletionTokens is the current spelling; MaxTokens the deprecated one.
	// The newer field wins when both are set.
	MaxTokens           int `json:"max_tokens,omitempty"`
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`

	// ReasoningEffort maps onto the abstract common.Effort (the thinking knob).
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// User is the standard end-user/session affinity field — mapped onto
	// Request.Session (Fireworks sticky routing already keys on `user`).
	User string `json:"user,omitempty"`

	// MemcodeBilling is the billing-lane extension (memcode backend only):
	// "" | "byok_preferred" (default — the user's key serves when present, else
	// credits), "byok_only" (fail if the serving vendor isn't user-keyed —
	// never touch credits), "credits" (skip BYOK injection; an explicit,
	// consented, debited serve — the CLI's "retry this turn on credits" path).
	// The gateway ENFORCES the lane; it never chooses one: byok-preferred
	// serving still never falls back to credits server-side (the doctrine's
	// no-silent-billing invariant, now enforcement rather than policy).
	MemcodeBilling string `json:"memcode_billing,omitempty"`

	// Accepted-and-ignored standard fields (the gateway owns sampling; one
	// choice is always served). Declared so intent is documented, not enforced.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	N           int      `json:"n,omitempty"`
}

// StreamOptions is the standard stream_options object. The gateway always sends
// the final usage chunk (a superset of include_usage:false — clients that did
// not ask simply ignore the extra chunk).
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatMessage is one request-side message. Roles: system | developer (treated
// as system) | user | assistant | tool.
type ChatMessage struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content,omitzero"`

	// assistant
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// MemcodeOpaque is extension (3): vendor reasoning blocks (Anthropic
	// thinking signatures, OpenAI rs_ items) round-tripped verbatim. Each
	// element is one common.Block in its wire form; the gateway re-expands
	// them ahead of the message's text/tool_use blocks.
	MemcodeOpaque []json.RawMessage `json:"memcode_opaque,omitempty"`

	// tool
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// MessageContent is the standard `content` union: a plain string or an array of
// typed parts. IsParts records which form arrived (and which to emit).
type MessageContent struct {
	Text    string
	Parts   []ContentPart
	IsParts bool
}

// StringContent builds the plain-string form.
func StringContent(s string) MessageContent { return MessageContent{Text: s} }

// PartsContent builds the array form.
func PartsContent(parts ...ContentPart) MessageContent {
	return MessageContent{Parts: parts, IsParts: true}
}

// IsZero makes `content` omittable (omitzero) for messages that carry only
// tool_calls.
func (m MessageContent) IsZero() bool { return !m.IsParts && m.Text == "" && m.Parts == nil }

func (m MessageContent) MarshalJSON() ([]byte, error) {
	if m.IsParts {
		return json.Marshal(m.Parts)
	}
	return json.Marshal(m.Text)
}

func (m *MessageContent) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		*m = MessageContent{}
		return nil
	}
	if t[0] == '[' {
		m.IsParts = true
		m.Text = ""
		return json.Unmarshal(b, &m.Parts)
	}
	m.IsParts = false
	m.Parts = nil
	return json.Unmarshal(b, &m.Text)
}

// ContentPart is one element of the array content form.
type ContentPart struct {
	Type     string        `json:"type"` // "text" | "image_url" | "file"
	Text     string        `json:"text,omitempty"`
	ImageURL *ImageURLPart `json:"image_url,omitempty"`
	File     *FilePart     `json:"file,omitempty"`
}

// TextPart builds a text content part.
func TextPart(s string) ContentPart { return ContentPart{Type: "text", Text: s} }

// ImageURLPart carries a vision input. The gateway accepts data: URLs only (it
// never fetches remote images on the user's behalf).
type ImageURLPart struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// FilePart carries a document input (OpenAI's own `file` content part). The
// gateway accepts inline file_data data: URLs; file_id has no file store behind
// it and is rejected.
type FilePart struct {
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// Tool is the standard function-tool definition envelope.
type Tool struct {
	Type     string      `json:"type"` // "function"
	Function FunctionDef `json:"function"`
}

// FunctionDef is the function payload of a tool definition.
type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

// ToolCall is one function call (request-side history and response-side output).
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`

	// MemcodeSignature carries opaque provider state that belongs to THIS call
	// and must come back verbatim when the call is replayed — Gemini issues a
	// thoughtSignature with every functionCall and rejects the replay with a 400
	// without it. The standard tool_calls shape has nowhere to put that, so it
	// rides a namespaced extension, the same way reasoning blocks ride
	// memcode_opaque. Ignored by any server that does not know it.
	MemcodeSignature string `json:"memcode_signature,omitempty"`
}

// FunctionCall is a call's name + JSON-encoded arguments string.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ── response ────────────────────────────────────────────────────────────────

// ChatResponse is the non-streamed chat completion.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
	// Memcode is extension (4) — loose decoders ignore it.
	Memcode *MemcodeExt `json:"memcode,omitempty"`
}

// Choice is one completion choice (the gateway always serves exactly one).
type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

// ResponseMessage is the assistant message of a completion. Content is null
// (not "") when the message is tool-calls only, per the standard shape.
type ResponseMessage struct {
	Role          string            `json:"role"`
	Content       *string           `json:"content"`
	ToolCalls     []ToolCall        `json:"tool_calls,omitempty"`
	MemcodeOpaque []json.RawMessage `json:"memcode_opaque,omitempty"`
}

// Usage is the standard usage object. NOTE the semantics conversion: the
// internal protocol counts Anthropic-style (input_tokens EXCLUDES cache
// reads/writes), while prompt_tokens here INCLUDES them, with the cache-read
// subset reported under prompt_tokens_details.cached_tokens.
type Usage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// PromptTokensDetails carries the cached-token subset of prompt_tokens.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// MemcodeExt is extension (4): the response metadata the CLI footer/compaction
// feed on, attached to the final body/chunk. All fields optional; third-party
// clients never need it.
type MemcodeExt struct {
	Byok           bool   `json:"byok,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`
	SearchCount    int    `json:"search_count,omitempty"`
	ContextWindow  int    `json:"context_window,omitempty"`
	InputBudget    int    `json:"input_budget,omitempty"`
	Pool           string `json:"pool,omitempty"`
}

// ── streaming ───────────────────────────────────────────────────────────────

// ChatChunk is one SSE `data:` payload of a streamed completion. The final
// usage chunk carries empty choices + Usage (+ the memcode extension), then the
// stream terminates with `data: [DONE]`.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"` // "chat.completion.chunk"
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
	Memcode *MemcodeExt   `json:"memcode,omitempty"`
}

// ChunkChoice is one delta frame.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is the incremental message fragment of a chunk.
type Delta struct {
	Role          string            `json:"role,omitempty"`
	Content       *string           `json:"content,omitempty"`
	ToolCalls     []ToolCallDelta   `json:"tool_calls,omitempty"`
	MemcodeOpaque []json.RawMessage `json:"memcode_opaque,omitempty"`
}

// ToolCallDelta is a streamed tool-call fragment, accumulated by Index.
type ToolCallDelta struct {
	Index    int           `json:"index"`
	ID       string        `json:"id,omitempty"`
	Type     string        `json:"type,omitempty"`
	Function *FunctionCall `json:"function,omitempty"`

	// MemcodeSignature is the streaming half of ToolCall.MemcodeSignature —
	// sent once on the delta that opens the call.
	MemcodeSignature string `json:"memcode_signature,omitempty"`
}

// ── models + errors ─────────────────────────────────────────────────────────

// ModelList is GET {prefix}/models: the standard list shape, extended with an
// ignorable top-level `memcode` object (extension 5) so one call carries
// everything the CLI's /model picker needs.
type ModelList struct {
	Object string       `json:"object"` // "list"
	Data   []ModelEntry `json:"data"`
	// Memcode is the list-level extension: org/routing facts that aren't
	// per-model. Strict OpenAI clients decode {object,data} and ignore it.
	Memcode *ModelsExt `json:"memcode,omitempty"`
}

// ModelsExt is the list-level memcode extension on GET {prefix}/models.
type ModelsExt struct {
	// CreditsExhausted reports the org's empty-wallet state so the CLI can
	// frame BYOK-only routing honestly.
	CreditsExhausted bool `json:"credits_exhausted"`
	// Backend names the gateway's provider mode ("hybrid" in prod).
	Backend string `json:"backend,omitempty"`
	// Vendors lists the strong-tier vendors the gateway has keys for — the
	// /model vendor selector's roster.
	Vendors []string `json:"vendors,omitempty"`
	// Roles (which model plays each routing role) is no longer decoded: the
	// ladder that consumed it is gone. The gateway may still send the field;
	// unknown JSON is ignored.
}

// ModelEntry is one listed model. The ids are the catalog LABELS — raw
// provider ids never leave the server.
type ModelEntry struct {
	ID      string     `json:"id"`
	Object  string     `json:"object"` // "model"
	Created int64      `json:"created,omitempty"`
	OwnedBy string     `json:"owned_by,omitempty"`
	Memcode *ModelMeta `json:"memcode,omitempty"`
}

// ModelMeta is the ignorable per-model extension. This is the hosted ROUTING
// CONTROL PLANE (all-policy-client-side): every server-side fact the CLI's
// selection policy reads must appear here — anything missing gets added
// explicitly, never smuggled back into gateway routing.
type ModelMeta struct {
	Name      string `json:"name,omitempty"`
	Desc      string `json:"desc,omitempty"`   // one-line picker description
	Group     string `json:"group,omitempty"`  // display family — presentation only
	Vendor    string `json:"vendor,omitempty"` // authoritative serving vendor — the selection/steering identity
	Window    int    `json:"window,omitempty"`
	Vision    bool   `json:"vision,omitempty"`
	PDF       bool   `json:"pdf,omitempty"`       // native PDF/document input — the document-turn pre-check
	Reasoning bool   `json:"reasoning,omitempty"` // exposes a thinking/reasoning knob
	Pinnable  bool   `json:"pinnable,omitempty"`  // offered in the /model picker (serving accepts every listed label)
	// Byok marks a model served by a vendor the requesting user brought their
	// own key for.
	Byok bool `json:"byok,omitempty"`
}

// ErrorResponse is the standard error envelope: {"error":{...}}.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the standard error object. Code carries the machine-readable
// memcode codes ("unknown_model", "context_overflow", …) the CLI keys on.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}
