// Package anthropicwire is the Anthropic Messages API adapter — the ONE
// implementation of the Messages dialect (cache_control placement, adaptive
// thinking, streaming decode, tool calls, native web search, usage parsing),
// shared by the hosted gateway (its own or the user's BYOK key) and the CLI's
// direct endpoint mode (api.anthropic.com). Transport encoding only — no
// routing policy, no metering.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/providers/provcore"
	"github.com/memcode-ai/memcode/internal/wire"
)

// EnvAnthropicKey is the environment variable holding the Anthropic API key.
const EnvAnthropicKey = "ANTHROPIC_API_KEY"

func init() {
	// Teach the shared retry kernel this vendor's API error shape.
	provcore.RegisterErrorInfo(func(err error) (int, http.Header, bool) {
		var anthErr *anthropicsdk.Error
		if errors.As(err, &anthErr) {
			var hdr http.Header
			if anthErr.Response != nil {
				hdr = anthErr.Response.Header
			}
			return anthErr.StatusCode, hdr, true
		}
		return 0, nil, false
	})
}

// Anthropic is a minimal ModelProvider backed by the Claude Messages API.
// The struct fields are kept exactly as before so tests can override baseURL.
type Anthropic struct {
	apiKey  string
	http    *http.Client
	baseURL string
	// oauth turns on the Claude Code compatibility mode (Bearer auth + betas +
	// UA, system-identity prepend, mcp__ tool renaming). Set from the token
	// shape at construction — a subscription OAuth token, never a console key.
	oauth bool
}

// NewAnthropic returns a client using the given credential. A normal
// sk-ant-api* key takes the clean x-api-key path; a Claude subscription OAuth
// token (cc-*/eyJ*/sk-ant-oat*) turns on the Claude Code compatibility mode
// (see oauth.go). baseURL is left empty so the SDK defaults to
// https://api.anthropic.com; tests override it via a.baseURL = srv.URL.
func NewAnthropic(apiKey string) *Anthropic {
	return &Anthropic{
		apiKey: apiKey,
		http:   provcore.NewTurnHTTPClient(),
		oauth:  isOAuthToken(apiKey),
	}
}

// SetBaseURL points the adapter at a different Messages-API host (tests,
// proxies, enterprise gateways). "" restores the SDK default.
func (a *Anthropic) SetBaseURL(u string) { a.baseURL = u }

// BaseURL reports the configured override ("" = the SDK default).
func (a *Anthropic) BaseURL() string { return a.baseURL }

// client builds a per-call SDK client from the struct fields.
// Building per-call is cheap and is what makes the baseURL test override work.
func (a *Anthropic) client(extraOpts ...option.RequestOption) anthropicsdk.Client {
	opts := []option.RequestOption{
		option.WithMaxRetries(0), // we run our own bounded retry loop
		option.WithHTTPClient(a.http),
	}
	if a.oauth {
		// Bearer auth (NOT x-api-key), the OAuth betas ADDED so the cache-TTL
		// beta survives, and the Claude Code client identity.
		opts = append(opts, option.WithAuthToken(a.apiKey))
		for _, b := range oauthOnlyBetas {
			opts = append(opts, option.WithHeaderAdd("anthropic-beta", b))
		}
		opts = append(opts,
			option.WithHeader("user-agent", claudeCodeUserAgent()),
			option.WithHeader("x-app", "cli"),
		)
	} else {
		opts = append(opts, option.WithAPIKey(a.apiKey))
	}
	if a.baseURL != "" {
		opts = append(opts, option.WithBaseURL(a.baseURL))
	}
	opts = append(opts, extraOpts...)
	return anthropicsdk.NewClient(opts...)
}

// Model returns the default model id (for display / StrongProvider). The Anthropic
// provider serves whatever model resolve.go chose; this is the strong-tier default
// (Sonnet — the balanced tier), matching OpenAI.Model()'s Terra default.
func (a *Anthropic) Model() string { return catalog.ModelSonnet }

// wire types mirror the Anthropic Messages API shape.
type wireRequest struct {
	Model        string            `json:"model"`
	MaxTokens    int               `json:"max_tokens"`
	System       any               `json:"system,omitempty"` // string, or []sysBlock with cache_control
	Messages     []wire.Message    `json:"messages"`
	Tools        []wire.ToolDef    `json:"tools,omitempty"`
	ToolChoice   string            `json:"tool_choice,omitempty"` // force this tool (structured output); "" = auto
	Stream       bool              `json:"stream,omitempty"`
	Thinking     *wireThinking     `json:"thinking,omitempty"`
	OutputConfig *wireOutputConfig `json:"output_config,omitempty"`
	// NativeWebSearch: the request carried the CLI's web_search function def, which
	// buildWire strips — wireToParams appends the server-side web_search_20250305 tool
	// instead (a ToolUnionParam member, not a ToolDef, so it can't ride w.Tools).
	NativeWebSearch bool `json:"-"`
}

// wireThinking is the Messages API `thinking` object. Current Opus/Sonnet use
// adaptive thinking ({type:"adaptive"}) and take depth from output_config.effort;
// interleaved (between-tool) thinking is automatic, no beta header. (Opus 4.7/4.8
// are adaptive-ONLY — manual budget_tokens 400s — so we don't implement it.)
type wireThinking struct {
	Type string `json:"type"` // "adaptive"
}

// wireOutputConfig carries the top-level `effort` hint (NOT nested under thinking).
type wireOutputConfig struct {
	Effort string `json:"effort,omitempty"` // low | medium | high
}

// supportsAdaptiveThinking reports whether a model accepts adaptive thinking +
// effort. Conservative: an unrecognized model (or the cheap Haiku classifier tier)
// returns false so we omit thinking and never risk a 400 — cheap paths stay cheap.
func supportsAdaptiveThinking(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "opus-5") || strings.Contains(m, "fable") ||
		strings.Contains(m, "opus-4-7") || strings.Contains(m, "opus-4-6") ||
		strings.Contains(m, "sonnet-5")
}

// thinkingFor maps an abstract Effort onto the concrete wire controls for a model.
// EffortOff or a non-adaptive model → nil/nil, so the request is byte-for-byte what
// it was before thinking existed.
func thinkingFor(model string, e wire.Effort) (*wireThinking, *wireOutputConfig) {
	if e == wire.EffortOff || !supportsAdaptiveThinking(model) {
		return nil, nil
	}
	return &wireThinking{Type: "adaptive"}, &wireOutputConfig{Effort: string(e)}
}

// sysBlock is a system-prompt text block carrying an optional cache breakpoint.
type sysBlock struct {
	Type         string             `json:"type"` // "text"
	Text         string             `json:"text"`
	CacheControl *wire.CacheControl `json:"cache_control,omitempty"`
}

// ephemeral is the default (5-min) cache-breakpoint marker, used for the
// per-turn conversation prefix (which changes every turn anyway).
var ephemeral = &wire.CacheControl{Type: "ephemeral"}

// ephemeral1h marks the STABLE doctrine+tools prefix with the 1-hour TTL: that prefix
// is byte-identical across turns even when the volatile facts change, so the long TTL
// lets it survive interactive gaps >5min instead of silently expiring.
var ephemeral1h = &wire.CacheControl{Type: "ephemeral", TTL: "1h"}

// cacheParam maps a provider-neutral CacheControl onto the SDK's ephemeral param,
// carrying the optional 1h TTL (default/"" → the SDK's 5-min ephemeral).
func cacheParam(cc *wire.CacheControl) anthropicsdk.CacheControlEphemeralParam {
	p := anthropicsdk.NewCacheControlEphemeralParam()
	if cc != nil && cc.TTL == "1h" {
		p.TTL = anthropicsdk.CacheControlEphemeralTTLTTL1h
	}
	return p
}

// buildWire assembles the wire request for r, decorating cache breakpoints on
// CLONES so the caller's shared system/tools/history are never mutated:
//   - system+tools prefix: a 1h breakpoint on the STABLE system block (system follows
//     tools in cache order, so it covers both) plus one on the last tool def. The
//     VOLATILE system block (room/personality/etc.) follows the stable one WITHOUT a
//     breakpoint, so a per-turn fact change never busts the cached doctrine prefix.
//   - conversation prefix: a 5-min breakpoint on the last block of the last message, so
//     each turn caches the full prior history for the next turn to read.
func buildWire(r wire.Request, maxTok int, stream bool, ccIdentity string) wireRequest {
	w := wireRequest{Model: r.Model, MaxTokens: maxTok, Stream: stream}
	var sys []sysBlock
	// OAuth/Claude-Code path: the identity is its OWN leading block, verbatim. The
	// filter keys on system[0] being EXACTLY the identity string, so it must not be
	// fused with the doctrine (see oauth.go). It rides uncached — the 1h breakpoint
	// on the doctrine block below still covers it (a breakpoint caches everything up
	// to and including it), so the stable prefix stays cached.
	if ccIdentity != "" {
		sys = append(sys, sysBlock{Type: "text", Text: ccIdentity})
		// Restore the product identity so the model doesn't call itself "Claude Code"
		// (the required line above is otherwise the first, most explicit name it sees).
		sys = append(sys, sysBlock{Type: "text", Text: claudeCodeIdentityClarification})
	}
	if r.System != "" {
		// Stable doctrine prefix carries the 1h breakpoint; the volatile suffix rides as a
		// SEPARATE, uncached block AFTER it (so it's still sent, just never cached).
		sys = append(sys, sysBlock{Type: "text", Text: r.System, CacheControl: ephemeral1h})
		if r.SystemVolatile != "" {
			sys = append(sys, sysBlock{Type: "text", Text: r.SystemVolatile})
		}
	}
	if len(sys) > 0 {
		w.System = sys
	}
	// The web_search function def is stripped BEFORE cache decoration, so the 1h
	// breakpoint lands on the last REAL function tool; wireToParams re-adds the native
	// search tool AFTER it (still covered by the system breakpoint — cache order is
	// tools→system — and stable within a session, so no cache churn).
	fnTools, nativeSearch := provcore.SplitWebSearchTool(r.Tools)
	w.NativeWebSearch = nativeSearch
	if n := len(fnTools); n > 0 {
		tools := make([]wire.ToolDef, n)
		copy(tools, fnTools)
		tools[n-1].CacheControl = ephemeral1h // tools are part of the stable prefix → long TTL
		w.Tools = tools
	}
	w.ToolChoice = r.ToolChoice
	w.Messages = withConversationBreakpoint(r.Messages)
	w.Thinking, w.OutputConfig = thinkingFor(r.Model, r.Effort)
	return w
}

// withConversationBreakpoint returns a copy of msgs with cache_control set on the
// last block of the last message — cloning the touched slices/block so the
// caller's history is left untouched.
func withConversationBreakpoint(msgs []wire.Message) []wire.Message {
	if len(msgs) == 0 {
		return msgs
	}
	out := make([]wire.Message, len(msgs))
	copy(out, msgs)
	last := &out[len(out)-1]
	if len(last.Blocks) == 0 {
		return out
	}
	blocks := make([]wire.Block, len(last.Blocks))
	copy(blocks, last.Blocks)
	blocks[len(blocks)-1].CacheControl = ephemeral
	last.Blocks = blocks
	return out
}

// toolInputSchema maps memcode's full JSON-Schema (a map with type/properties/required/…)
// onto the SDK's ToolInputSchemaParam, which models `properties` and `required` as separate
// fields (and `type` as a constant). Putting the WHOLE schema into .Properties produces a
// double-nested, draft-2020-12-invalid schema that Anthropic rejects — so split it out, and
// carry any other JSON-Schema keywords ($defs, additionalProperties, …) via ExtraFields.
func toolInputSchema(m map[string]any) anthropicsdk.ToolInputSchemaParam {
	s := anthropicsdk.ToolInputSchemaParam{}
	if props, ok := m["properties"]; ok {
		s.Properties = props
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if rs, ok := r.(string); ok {
				s.Required = append(s.Required, rs)
			}
		}
	} else if req, ok := m["required"].([]string); ok {
		s.Required = req
	}
	for k, v := range m {
		switch k {
		case "type", "properties", "required": // modeled explicitly by the SDK param
		default:
			if s.ExtraFields == nil {
				s.ExtraFields = map[string]any{}
			}
			s.ExtraFields[k] = v
		}
	}
	return s
}

// wireToParams translates the already-cache-decorated wireRequest into SDK params.
// It reuses the tested buildWire output (cache breakpoints, thinking, output_config)
// rather than re-deriving anything.
func wireToParams(w wireRequest) anthropicsdk.MessageNewParams {
	p := anthropicsdk.MessageNewParams{
		Model:     anthropicsdk.Model(w.Model),
		MaxTokens: int64(w.MaxTokens),
	}

	// System — buildWire emits []sysBlock when r.System != "".
	if blocks, ok := w.System.([]sysBlock); ok {
		sys := make([]anthropicsdk.TextBlockParam, len(blocks))
		for i, sb := range blocks {
			tb := anthropicsdk.TextBlockParam{Text: sb.Text}
			if sb.CacheControl != nil {
				tb.CacheControl = cacheParam(sb.CacheControl)
			}
			sys[i] = tb
		}
		p.System = sys
	}

	// Tools
	if len(w.Tools) > 0 || w.NativeWebSearch {
		tools := make([]anthropicsdk.ToolUnionParam, 0, len(w.Tools)+1)
		for _, td := range w.Tools {
			tp := anthropicsdk.ToolParam{
				Name:        td.Name,
				InputSchema: toolInputSchema(td.InputSchema),
			}
			if td.Description != "" {
				tp.Description = param.NewOpt(td.Description)
			}
			if td.CacheControl != nil {
				tp.CacheControl = cacheParam(td.CacheControl)
			}
			tools = append(tools, anthropicsdk.ToolUnionParam{OfTool: &tp})
		}
		if w.NativeWebSearch {
			// Server-side search on the SERVING request (mirrors the WebSearch side
			// channel's tool). The searched-in answer arrives as ordinary text blocks;
			// the structured server_tool_use / web_search_tool_result blocks are dropped
			// by streamOnce (unknown-typ accumulators produce no Block) — an accepted
			// degradation, identical in fidelity to the old side channel (text only).
			tools = append(tools, anthropicsdk.ToolUnionParam{OfWebSearchTool20250305: &anthropicsdk.WebSearchTool20250305Param{MaxUses: param.NewOpt(int64(5))}})
		}
		p.Tools = tools
		// Force a specific tool when asked (structured output): the model MUST call it, so
		// its JSON arrives in the tool_use input constrained to the tool's input_schema.
		if w.ToolChoice != "" {
			p.ToolChoice = anthropicsdk.ToolChoiceParamOfTool(w.ToolChoice)
		}
	}

	// Messages
	p.Messages = make([]anthropicsdk.MessageParam, len(w.Messages))
	for i, msg := range w.Messages {
		var role anthropicsdk.MessageParamRole
		switch msg.Role {
		case "assistant":
			role = anthropicsdk.MessageParamRoleAssistant
		default:
			role = anthropicsdk.MessageParamRoleUser
		}
		content := make([]anthropicsdk.ContentBlockParamUnion, 0, len(msg.Blocks))
		for _, blk := range msg.Blocks {
			// Drop foreign reasoning: an OpenAI reasoning item (id "rs_…") replayed here
			// would be sent as an Anthropic thinking signature → 400 invalid signature.
			// This is what bricked a session after a mid-conversation /model vendor switch.
			if (blk.Type == "thinking" || blk.Type == "redacted_thinking") && strings.HasPrefix(blk.ID, "rs_") {
				continue
			}
			// Anthropic requires every preserved thinking block from the latest assistant
			// message to be byte-for-byte intact. Cache annotations change the serialized
			// block, so never decorate a thinking/redacted_thinking block with the
			// conversation cache breakpoint.
			if blk.Type == "thinking" || blk.Type == "redacted_thinking" {
				blk.CacheControl = nil
			}
			content = append(content, blockToParam(blk))
		}
		p.Messages[i] = anthropicsdk.MessageParam{Role: role, Content: content}
	}

	// Thinking
	if w.Thinking != nil {
		adaptive := anthropicsdk.ThinkingConfigAdaptiveParam{}
		p.Thinking = anthropicsdk.ThinkingConfigParamUnion{OfAdaptive: &adaptive}
	}

	// OutputConfig (effort)
	if w.OutputConfig != nil && w.OutputConfig.Effort != "" {
		p.OutputConfig = anthropicsdk.OutputConfigParam{
			Effort: anthropicsdk.OutputConfigEffort(w.OutputConfig.Effort),
		}
	}

	return p
}

// blockToParam translates one of our Block types into the SDK's content block
// param union. CacheControl is attached when the block carries it.
func blockToParam(b wire.Block) anthropicsdk.ContentBlockParamUnion {
	var cc anthropicsdk.CacheControlEphemeralParam
	hasCacheControl := b.CacheControl != nil
	if hasCacheControl {
		cc = anthropicsdk.NewCacheControlEphemeralParam()
	}

	switch b.Type {
	case "text":
		tp := anthropicsdk.TextBlockParam{Text: b.Text}
		if hasCacheControl {
			tp.CacheControl = cc
		}
		return anthropicsdk.ContentBlockParamUnion{OfText: &tp}

	case "image":
		var mediaType anthropicsdk.Base64ImageSourceMediaType
		var data string
		if b.Source != nil {
			mediaType = anthropicsdk.Base64ImageSourceMediaType(b.Source.MediaType)
			data = b.Source.Data
		}
		ip := anthropicsdk.ImageBlockParam{
			Source: anthropicsdk.ImageBlockParamSourceUnion{
				OfBase64: &anthropicsdk.Base64ImageSourceParam{
					Data:      data,
					MediaType: mediaType,
				},
			},
		}
		if hasCacheControl {
			ip.CacheControl = cc
		}
		return anthropicsdk.ContentBlockParamUnion{OfImage: &ip}

	case "document":
		var data string
		if b.Source != nil {
			data = b.Source.Data
		}
		dp := anthropicsdk.DocumentBlockParam{
			Source: anthropicsdk.DocumentBlockParamSourceUnion{
				OfBase64: &anthropicsdk.Base64PDFSourceParam{
					Data: data,
				},
			},
		}
		if hasCacheControl {
			dp.CacheControl = cc
		}
		return anthropicsdk.ContentBlockParamUnion{OfDocument: &dp}

	case "tool_use":
		var input any
		if len(b.Input) > 0 {
			if err := json.Unmarshal(b.Input, &input); err != nil {
				provcore.LogToolInputMalformed("anthropic", err)
			}
		}
		tp := anthropicsdk.ToolUseBlockParam{ID: b.ID, Name: b.Name, Input: input}
		if hasCacheControl {
			tp.CacheControl = cc
		}
		return anthropicsdk.ContentBlockParamUnion{OfToolUse: &tp}

	case "tool_result":
		// A tool_result's id lives in ToolUseID (json:"tool_use_id"), NOT ID
		// (which is the tool_use block's own id and is empty here). Reading b.ID
		// sends an empty tool_use_id, which fails Anthropic's ^[a-zA-Z0-9_-]+$.
		rp := anthropicsdk.ToolResultBlockParam{ToolUseID: b.ToolUseID}
		if b.IsError {
			rp.IsError = param.NewOpt(true)
		}
		// Structured content (text + image blocks): the path for tool results that
		// include vision (e.g. a browser screenshot). Each content block becomes a
		// part in the tool_result's content union — OfText for text, OfImage (base64)
		// for image. This maps directly onto Anthropic's tool_result content union.
		if len(b.ContentBlocks) > 0 {
			parts := make([]anthropicsdk.ToolResultBlockParamContentUnion, 0, len(b.ContentBlocks))
			for _, cb := range b.ContentBlocks {
				switch cb.Type {
				case "text":
					parts = append(parts, anthropicsdk.ToolResultBlockParamContentUnion{
						OfText: &anthropicsdk.TextBlockParam{Text: cb.Text},
					})
				case "image":
					var mediaType anthropicsdk.Base64ImageSourceMediaType
					var data string
					if cb.Source != nil {
						mediaType = anthropicsdk.Base64ImageSourceMediaType(cb.Source.MediaType)
						data = cb.Source.Data
					}
					parts = append(parts, anthropicsdk.ToolResultBlockParamContentUnion{
						OfImage: &anthropicsdk.ImageBlockParam{
							Source: anthropicsdk.ImageBlockParamSourceUnion{
								OfBase64: &anthropicsdk.Base64ImageSourceParam{
									Data:      data,
									MediaType: mediaType,
								},
							},
						},
					})
				case "document":
					// A read PDF riding in the tool result — the content union takes
					// documents natively (base64 defaults to application/pdf).
					var data string
					if cb.Source != nil {
						data = cb.Source.Data
					}
					parts = append(parts, anthropicsdk.ToolResultBlockParamContentUnion{
						OfDocument: &anthropicsdk.DocumentBlockParam{
							Source: anthropicsdk.DocumentBlockParamSourceUnion{
								OfBase64: &anthropicsdk.Base64PDFSourceParam{Data: data},
							},
						},
					})
				}
			}
			rp.Content = parts
		} else if b.Content != "" {
			rp.Content = []anthropicsdk.ToolResultBlockParamContentUnion{
				{OfText: &anthropicsdk.TextBlockParam{Text: b.Content}},
			}
		}
		if hasCacheControl {
			rp.CacheControl = cc
		}
		return anthropicsdk.ContentBlockParamUnion{OfToolResult: &rp}

	case "thinking":
		// Always send Thinking even when empty — it is load-bearing for multi-turn
		// round-trips: the API requires the signature to validate the prior turn.
		tp := anthropicsdk.ThinkingBlockParam{
			Thinking:  b.Thinking,
			Signature: b.Signature,
		}
		return anthropicsdk.ContentBlockParamUnion{OfThinking: &tp}

	case "redacted_thinking":
		rp := anthropicsdk.RedactedThinkingBlockParam{Data: b.Data}
		return anthropicsdk.ContentBlockParamUnion{OfRedactedThinking: &rp}

	default:
		// Unknown block type — emit as empty text to avoid dropping it silently.
		return anthropicsdk.ContentBlockParamUnion{OfText: &anthropicsdk.TextBlockParam{Text: b.Text}}
	}
}

// Complete satisfies the non-streamed contract by streaming under the hood and
// assembling the full Response, discarding the live deltas. Anthropic's SDK REFUSES
// a true non-streaming request whose estimated time exceeds 10 minutes — for a large
// max_tokens (e.g. an Opus turn) client.go's CalculateNonStreamingTimeout returns
// "streaming is required for operations that may take longer than 10 minutes", which
// the gateway then surfaces as a 502. Streaming sidesteps that ceiling entirely.
//
// This is deliberately Anthropic-ONLY (it lives in this provider): Fireworks/vLLM keep
// their own non-streaming Complete, because streaming there breaks tool-call assembly.
func (a *Anthropic) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return a.Stream(ctx, r, wire.StreamHandler{})
}

// WebSearch answers a query (or "read this URL: …" request) using Anthropic's
// server-side web_search tool, returning the synthesized text. The API runs the
// searches within the single turn; we just return the assistant's text blocks.
func (a *Anthropic) WebSearch(ctx context.Context, query string) (string, wire.Response, error) {
	if a.apiKey == "" {
		return "", wire.Response{}, fmt.Errorf("no Anthropic API key (set %s)", EnvAnthropicKey)
	}
	params := anthropicsdk.MessageNewParams{
		Model:     anthropicsdk.Model(catalog.ModelSonnet),
		MaxTokens: 3000,
		System: []anthropicsdk.TextBlockParam{
			{Text: provcore.WebSearchSystemPrompt},
		},
		Messages: []anthropicsdk.MessageParam{
			{
				Role:    anthropicsdk.MessageParamRoleUser,
				Content: []anthropicsdk.ContentBlockParamUnion{{OfText: &anthropicsdk.TextBlockParam{Text: query}}},
			},
		},
		Tools: []anthropicsdk.ToolUnionParam{
			{OfWebSearchTool20250305: &anthropicsdk.WebSearchTool20250305Param{MaxUses: param.NewOpt(int64(5))}},
		},
	}
	cl := a.client()
	msg, err := provcore.WithRetry(ctx, func() (*anthropicsdk.Message, error) {
		return cl.Messages.New(ctx, params)
	})
	if err != nil {
		var apiErr *anthropicsdk.Error
		if errors.As(err, &apiErr) {
			raw := apiErr.RawJSON()
			return "", wire.Response{Model: catalog.ModelSonnet, Backend: "anthropic"}, fmt.Errorf("anthropic %s: %s", string(apiErr.Type()), raw)
		}
		return "", wire.Response{Model: catalog.ModelSonnet, Backend: "anthropic"}, err
	}
	// Anthropic splits a web_search reply into MULTIPLE text blocks whenever a span carries a
	// citation — often mid-sentence, e.g. ["Today's top story: ", "something happened."]. These
	// are fragments of ONE continuous string, not separate paragraphs; concatenate them directly
	// (no separator). Joining with "\n" (the old bug) injected a spurious newline at every
	// citation boundary — invisible in most prose, but it split a markdown bullet's "- " marker
	// from its own text onto two lines whenever a citation landed right after the marker.
	var b strings.Builder
	for _, cu := range msg.Content {
		if tb, ok := cu.AsAny().(anthropicsdk.TextBlock); ok && tb.Text != "" {
			b.WriteString(tb.Text)
		}
	}
	return strings.TrimSpace(b.String()), anthropicSideUsage(msg), nil
}

// anthropicSideUsage maps a Messages API result onto the neutral usage shape for
// side-channel metering (websearch/webfetch run on Sonnet). SearchCount carries
// usage.server_tool_use.web_search_requests — each search bills a per-request fee
// on top of tokens (SearchFeeUSD), so a /v1/websearch call meters it too. WebFetch
// naturally reports 0 here: web_fetch has no per-request fee and its requests are
// not counted in web_search_requests.
func anthropicSideUsage(msg *anthropicsdk.Message) wire.Response {
	return wire.Response{
		Model:        catalog.ModelSonnet,
		Backend:      "anthropic",
		InputTokens:  int(msg.Usage.InputTokens),
		OutputTokens: int(msg.Usage.OutputTokens),
		SearchCount:  int(msg.Usage.ServerToolUse.WebSearchRequests),
	}
}

// WebFetch retrieves a URL via Anthropic's server-side web_fetch tool and returns
// its readable content (the fetched document text, falling back to the model's
// text). Handles text + PDF; does NOT render JavaScript pages. No extra charge
// beyond tokens. The URL is in the request context, satisfying web_fetch's
// "URL must appear in context" rule.
func (a *Anthropic) WebFetch(ctx context.Context, url string) (string, wire.Response, error) {
	if a.apiKey == "" {
		return "", wire.Response{}, fmt.Errorf("no Anthropic API key (set %s)", EnvAnthropicKey)
	}
	params := anthropicsdk.MessageNewParams{
		Model:     anthropicsdk.Model(catalog.ModelSonnet),
		MaxTokens: 8192,
		Messages: []anthropicsdk.MessageParam{
			{
				Role: anthropicsdk.MessageParamRoleUser,
				Content: []anthropicsdk.ContentBlockParamUnion{
					{OfText: &anthropicsdk.TextBlockParam{Text: "Fetch this URL and return its full readable content verbatim as markdown, no commentary: " + url}},
				},
			},
		},
		Tools: []anthropicsdk.ToolUnionParam{
			{OfWebFetchTool20250910: &anthropicsdk.WebFetchTool20250910Param{
				MaxUses:          param.NewOpt(int64(3)),
				MaxContentTokens: param.NewOpt(int64(60000)),
			}},
		},
	}
	// web-fetch may require a beta header depending on API version; add it defensively.
	cl := a.client(option.WithHeader("anthropic-beta", "web-fetch-2025-09-10"))
	msg, err := provcore.WithRetry(ctx, func() (*anthropicsdk.Message, error) {
		return cl.Messages.New(ctx, params)
	})
	if err != nil {
		var apiErr *anthropicsdk.Error
		if errors.As(err, &apiErr) {
			raw := apiErr.RawJSON()
			return "", wire.Response{Model: catalog.ModelSonnet, Backend: "anthropic"}, fmt.Errorf("anthropic %s: %s", string(apiErr.Type()), raw)
		}
		return "", wire.Response{Model: catalog.ModelSonnet, Backend: "anthropic"}, err
	}
	// Prefer the fetched document text from typed result blocks; fall back to text blocks.
	// Same citation-boundary splitting as WebSearch above (see the comment there) — join
	// fragments directly, no inserted separator.
	var doc, text strings.Builder
	for _, cu := range msg.Content {
		switch v := cu.AsAny().(type) {
		case anthropicsdk.TextBlock:
			if v.Text != "" {
				text.WriteString(v.Text)
			}
		case anthropicsdk.WebFetchToolResultBlock:
			// The typed fetch result: a successful fetch carries the document
			// (a text source holds the readable content verbatim; a base64/PDF
			// source has no inline text to prefer). Error results carry no
			// document — the model's text fallback below explains the failure.
			if fetched := v.Content.AsResponseWebFetchResultBlock(); fetched.Content.Source.Type == "text" {
				doc.WriteString(fetched.Content.Source.Data)
			}
		}
	}
	if strings.TrimSpace(doc.String()) != "" {
		return strings.TrimSpace(doc.String()), anthropicSideUsage(msg), nil
	}
	return strings.TrimSpace(text.String()), anthropicSideUsage(msg), nil
}

type blockAccum struct {
	typ       string
	id        string
	name      string
	text      strings.Builder
	json      strings.Builder
	thinking  strings.Builder
	signature string
	data      string
}

// Stream sends a streaming Messages request, forwarding text/usage to h as they arrive, and
// returns the fully assembled Response. It runs a bounded, emitted-aware retry for transient
// failures (429/5xx before the first token) — the same pattern the OpenAI provider uses, and
// the reliability the escalation paths (self_heal, plan review) most need. A stream can't
// resume mid-flight, so it only re-attempts when nothing was forwarded to h yet.
func (a *Anthropic) Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	if a.apiKey == "" {
		return wire.Response{}, fmt.Errorf("no Anthropic API key (set %s)", EnvAnthropicKey)
	}
	// Claude Code compatibility mode: shape the request (identity + mcp__ tool
	// names) once, and restore memcode's tool names on the response.
	var oauthRev map[string]string
	if a.oauth {
		r, oauthRev = oauthEncodeRequest(r)
	}
	resp, err := provcore.StreamWithRetry(ctx, func() (wire.Response, bool, error) {
		return a.streamOnce(ctx, r, h)
	})
	if err == nil {
		oauthDecodeResponse(&resp, oauthRev)
	}
	return resp, err
}

// streamOnce runs a single streaming attempt. emitted reports whether any content bytes were
// forwarded to h (after which retrying would duplicate output for the caller).
func (a *Anthropic) streamOnce(ctx context.Context, r wire.Request, h wire.StreamHandler) (_ wire.Response, emitted bool, _ error) {
	maxTok := r.MaxTokens
	if maxTok == 0 {
		// 0 = uncapped. Anthropic REQUIRES max_tokens, so "uncapped" means the model's
		// own max output from the catalog (a value above the model max would 400).
		// The 4096 floor only covers a model the catalog doesn't declare — routing
		// serves cataloged models, so in practice the catalog value is used.
		if maxTok = catalog.MaxOutputTokens(r.Model); maxTok == 0 {
			maxTok = 4096
		}
	}

	ccIdentity := ""
	if a.oauth {
		ccIdentity = claudeCodeSystemPrefix
	}
	params := wireToParams(buildWire(r, maxTok, true, ccIdentity))
	cl := a.client()
	stream := cl.Messages.NewStreaming(ctx, params)

	builders := map[int]*blockAccum{}
	var order []int
	var inTok, outTok, cacheWrite, cacheRead int
	// searchCount: usage.server_tool_use.web_search_requests — the native
	// web_search_20250305 tool's per-request fee counter (a serving turn with
	// NativeWebSearch can run up to MaxUses searches, each billed upstream).
	var searchCount int
	var stopReason string
	notifyUsage := func() {
		if h.Usage != nil {
			h.Usage(inTok, outTok)
		}
	}

	for stream.Next() {
		switch evt := stream.Current().AsAny().(type) {
		case anthropicsdk.MessageStartEvent:
			inTok = int(evt.Message.Usage.InputTokens)
			outTok = int(evt.Message.Usage.OutputTokens)
			cacheWrite = int(evt.Message.Usage.CacheCreationInputTokens)
			cacheRead = int(evt.Message.Usage.CacheReadInputTokens)
			searchCount = int(evt.Message.Usage.ServerToolUse.WebSearchRequests)
			notifyUsage()

		case anthropicsdk.ContentBlockStartEvent:
			b := &blockAccum{}
			switch cb := evt.ContentBlock.AsAny().(type) {
			case anthropicsdk.TextBlock:
				// Seed from the start event: with adaptive thinking Anthropic can
				// deliver a short answer's ENTIRE text here (no text_delta ever
				// follows). Discarding it streamed zero deltas and assembled an
				// empty reply — "pong" after a thinking block simply vanished.
				b.typ = "text"
				if cb.Text != "" {
					b.text.WriteString(cb.Text)
					if h.Text != nil {
						h.Text(cb.Text)
						emitted = true
					}
				}
			case anthropicsdk.ToolUseBlock:
				b.typ, b.id, b.name = "tool_use", cb.ID, cb.Name
			case anthropicsdk.ThinkingBlock:
				// Seed from the start event: Anthropic can deliver initial thinking text
				// and/or the signature HERE (not only via deltas — adaptive thinking with
				// display:omitted returns a signature-only start block with no signature_delta).
				// Discarding them corrupted the round-trip: the next turn sent back a
				// thinking block with empty/truncated fields, and Anthropic rejected it as
				// "thinking blocks in the latest assistant message cannot be modified".
				b.typ = "thinking"
				b.thinking.WriteString(cb.Thinking)
				b.signature = cb.Signature
			case anthropicsdk.RedactedThinkingBlock:
				// Capture the opaque Data here — it arrives whole on the start event, not
				// via deltas. Without it the block round-tripped as data:"" and the API
				// rejected the next turn (hard 400 once any redacted thinking appeared).
				b.typ, b.data = "redacted_thinking", cb.Data
			default:
				_ = cb
			}
			builders[int(evt.Index)] = b
			order = append(order, int(evt.Index))

		case anthropicsdk.ContentBlockDeltaEvent:
			b := builders[int(evt.Index)]
			if b == nil {
				continue
			}
			switch d := evt.Delta.AsAny().(type) {
			case anthropicsdk.TextDelta:
				b.text.WriteString(d.Text)
				if h.Text != nil {
					h.Text(d.Text)
					emitted = true // content forwarded — no longer safe to retry this stream
				}
			case anthropicsdk.InputJSONDelta:
				b.json.WriteString(d.PartialJSON)
			case anthropicsdk.ThinkingDelta:
				// Captured for the mandatory round-trip, NOT streamed to the user —
				// reasoning is the model's scratch space, not part of the reply.
				b.thinking.WriteString(d.Thinking)
			case anthropicsdk.SignatureDelta:
				b.signature += d.Signature
			}

		case anthropicsdk.MessageDeltaEvent:
			if evt.Delta.StopReason != "" {
				stopReason = string(evt.Delta.StopReason)
			}
			outTok = int(evt.Usage.OutputTokens)
			// Cumulative count — searches run mid-turn, so the delta event's
			// value supersedes message_start's (usually 0 there).
			if n := int(evt.Usage.ServerToolUse.WebSearchRequests); n > searchCount {
				searchCount = n
			}
			notifyUsage()
		}
	}
	if err := stream.Err(); err != nil {
		// Return whatever the vendor already billed before the cut — a cancelled or failed
		// stream still costs those tokens, and the Runner meters a response with usage even
		// on error, so this keeps the expensive-failure case visible in /status and the bill.
		partial := wire.Response{
			InputTokens: inTok, OutputTokens: outTok,
			CacheWriteTokens: cacheWrite, CacheReadTokens: cacheRead,
			SearchCount: searchCount,
			Model:       r.Model, Backend: "anthropic",
		}
		if ctx.Err() != nil {
			return partial, emitted, ctx.Err()
		}
		var apiErr *anthropicsdk.Error
		if errors.As(err, &apiErr) {
			raw := apiErr.RawJSON()
			if provcore.IsOverflowMessage(raw) {
				return partial, emitted, &provcore.ContextOverflowError{Backend: "anthropic", Message: raw}
			}
			return partial, emitted, fmt.Errorf("anthropic %s: %s", string(apiErr.Type()), raw)
		}
		return partial, emitted, fmt.Errorf("anthropic stream: %w", err)
	}

	var blocks []wire.Block
	for _, idx := range order {
		b := builders[idx]
		switch b.typ {
		case "text":
			blocks = append(blocks, wire.Block{Type: "text", Text: b.text.String()})
		case "tool_use":
			input := strings.TrimSpace(b.json.String())
			if input == "" {
				input = "{}"
			}
			blocks = append(blocks, wire.Block{Type: "tool_use", ID: b.id, Name: b.name, Input: json.RawMessage(input)})
		case "thinking":
			// Preserve the thinking block verbatim — Signature is load-bearing for the
			// next turn's round-trip even when the thinking text is empty (omitted).
			blocks = append(blocks, wire.Block{Type: "thinking", Thinking: b.thinking.String(), Signature: b.signature})
		case "redacted_thinking":
			blocks = append(blocks, wire.Block{Type: "redacted_thinking", Data: b.data})
		}
	}
	return wire.Response{
		StopReason: stopReason, Blocks: blocks,
		InputTokens: inTok, OutputTokens: outTok,
		CacheWriteTokens: cacheWrite, CacheReadTokens: cacheRead,
		SearchCount: searchCount,
		Model:       r.Model, Backend: "anthropic",
	}, emitted, nil
}
