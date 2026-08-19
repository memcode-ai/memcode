// Package openaiwire is the OpenAI Responses API adapter — the ONE
// implementation of the Responses dialect (request encoding, streaming
// decode, reasoning-item round-trip, tool calls, usage parsing), shared by
// the hosted gateway (which injects its own or the user's BYOK key) and the
// CLI's direct endpoint mode (api.openai.com / api.x.ai). Extracted verbatim
// from the gateway's internal provider package; transport encoding only — no
// routing policy, no metering.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/providers/provcore"
	"github.com/memcode-ai/memcode/internal/wire"
)

// EnvOpenAIKey is the environment variable holding the OpenAI API key.
const EnvOpenAIKey = "OPENAI_API_KEY"

func init() {
	// Teach the shared retry kernel this vendor's API error shape.
	provcore.RegisterErrorInfo(func(err error) (int, http.Header, bool) {
		var oaErr *oai.Error
		if errors.As(err, &oaErr) {
			var hdr http.Header
			if oaErr.Response != nil {
				hdr = oaErr.Response.Header
			}
			return oaErr.StatusCode, hdr, true
		}
		return 0, nil, false
	})
}

// OpenAI is the strong-tier ModelProvider backed by the OpenAI Responses API (GPT-5.6
// family). It implements ModelProvider + Streamer + WebSearcher + WebFetcher — the same
// capability surface as the Anthropic provider. It maps wire.Request onto the
// Responses API (instructions + input items + flat function tools + reasoning.effort +
// prompt-cache breakpoints) and Responses output back to wire.Response.
//
// The Responses API is item-structured (not message-structured): the conversation is a
// flat list of input items (messages, function calls, function outputs, reasoning), and
// the response is a flat list of output items. Reasoning items carry an encrypted_content
// blob that must be round-tripped on the next turn for stateless multi-turn tool use
// (store=false; we manage conversation state ourselves).
type OpenAI struct {
	apiKey  string
	http    *http.Client
	baseURL string // override for tests / OpenAI-compatible vendors; "" → the SDK default

	// Vendor identity — the Responses dialect is spoken by more than one vendor
	// (xAI's api.x.ai is OpenAI-Responses-compatible and its documented Go path
	// is this SDK + a base URL; xAI ships no Go SDK). Grok embeds this adapter
	// with these fields overridden instead of duplicating the dialect.
	backend      string // wire backend tag stamped on responses ("openai" | "grok")
	defaultModel string // side-channel + display model (Terra | grok-4.6)
	keyEnv       string // env var named in missing-key errors
	clampEffort  bool   // clamp reasoning.effort to the low|high vocabulary (xAI)
	includeEnc   bool   // request encrypted-reasoning round-trip (OpenAI-only includable)

	// extraHeaders are sent on every request — the identity a subscription
	// backend requires (a ChatGPT/Codex endpoint's originator + account id).
	// Empty for a normal OpenAI/xAI key.
	extraHeaders map[string]string
}

// NewOpenAI returns a client using the given API key. baseURL is left empty so the SDK
// defaults; tests override it via o.baseURL = srv.URL before use (same pattern as Anthropic).
func NewOpenAI(apiKey string) *OpenAI {
	return &OpenAI{
		apiKey:       apiKey,
		http:         provcore.NewTurnHTTPClient(),
		backend:      "openai",
		defaultModel: catalog.ModelTerra,
		keyEnv:       EnvOpenAIKey,
		includeEnc:   true,
	}
}

// SetBaseURL points the adapter at a different Responses-API host (tests,
// proxies, enterprise gateways). "" restores the SDK default.
func (o *OpenAI) SetBaseURL(u string) { o.baseURL = u }

// SetExtraHeaders sets identity headers sent on every request — the shape a
// subscription backend (ChatGPT/Codex) requires to accept the call.
func (o *OpenAI) SetExtraHeaders(h map[string]string) { o.extraHeaders = h }

// BaseURL reports the configured override ("" = the SDK default).
func (o *OpenAI) BaseURL() string { return o.baseURL }

// client builds a per-call SDK client from the struct fields (same pattern as Anthropic:
// building per-call is cheap and makes the baseURL test override work).
func (o *OpenAI) client(extraOpts ...option.RequestOption) oai.Client {
	opts := []option.RequestOption{
		option.WithAPIKey(o.apiKey),
		option.WithMaxRetries(0), // we run our own bounded retry loop (withRetry, shared with Anthropic)
		option.WithHTTPClient(o.http),
	}
	if o.baseURL != "" {
		opts = append(opts, option.WithBaseURL(o.baseURL))
	}
	for k, v := range o.extraHeaders {
		opts = append(opts, option.WithHeader(k, v))
	}
	opts = append(opts, extraOpts...)
	return oai.NewClient(opts...)
}

// Complete satisfies the non-streamed contract by streaming under the hood and assembling
// the full Response — same rationale as Anthropic.Complete (the Responses API has the same
// long-operation streaming requirement for large max_tokens).
func (o *OpenAI) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return o.Stream(ctx, r, wire.StreamHandler{})
}

// mapEffort translates memcode's abstract Effort onto the Responses API's reasoning.effort
// vocabulary (none|minimal|low|medium|high|xhigh). EffortHigh (the hardest planning/review
// turns) maps to xhigh; a normal tool-carrying turn maps to high; EffortOff maps to none so
// a cheap classifier turn stays fast. The GPT-5.6 family supports the full range.
func mapEffort(e wire.Effort) shared.ReasoningEffort {
	switch e {
	case wire.EffortHigh:
		return shared.ReasoningEffortXhigh
	case wire.EffortMedium:
		return shared.ReasoningEffortHigh
	case wire.EffortLow:
		return shared.ReasoningEffortLow
	default: // EffortOff
		return shared.ReasoningEffortNone
	}
}

// effortFor applies the adapter's effort vocabulary: mapEffort's full range for
// OpenAI; clamped to low|high for vendors that reject the rest (xAI grok).
func (o *OpenAI) effortFor(e wire.Effort) shared.ReasoningEffort {
	eff := mapEffort(e)
	if o.clampEffort {
		switch eff {
		case shared.ReasoningEffortXhigh:
			eff = shared.ReasoningEffortHigh
		case shared.ReasoningEffortNone, shared.ReasoningEffortMinimal:
			eff = shared.ReasoningEffortLow
		}
	}
	return eff
}

// buildParams assembles the Responses API request from a wire.Request.
//
// System prompt → Instructions (a single string; the Responses API caches the prefix
// automatically ≥1024 tokens, so explicit breakpoints are an optimization we skip — the
// stable doctrine prefix is the longest common prefix and auto-caches). Volatile facts
// append to the last user message's text (outside the cached prefix), matching the split
// the gateway's compose() already produces.
//
// Messages → Input items: user text/image → input_message; assistant text → output_message;
// assistant tool_use → function_call; user tool_result → function_call_output; thinking
// blocks (with their encrypted reasoning) → reasoning items round-tripped via
// reasoning.encrypted_content in Include.
//
// Tools → flat FunctionToolParam (Name + Parameters top-level, NOT nested under
// "function" — the Responses API shape). ToolChoice forces a named tool when set.
func (o *OpenAI) buildParams(r wire.Request, maxTok int) responses.ResponseNewParams {
	p := responses.ResponseNewParams{
		Model:             shared.ResponsesModel(r.Model),
		Store:             oai.Bool(false), // stateless — we manage conversation state
		ParallelToolCalls: oai.Bool(false), // sequential tool calls — the agent loop expects one round at a time
	}
	if maxTok > 0 {
		// 0 = uncapped: omit the field so the provider allows up to the model's own max
		// output. An arbitrary default here silently truncated forced-tool verdicts when
		// a reasoning model spent the whole budget thinking (the gpt-oss-120b incident).
		p.MaxOutputTokens = oai.Int(int64(maxTok))
	}

	// System prompt → Instructions: the STABLE half ONLY. Instructions precede the input
	// items in the serialized prompt, so anything volatile placed here would sit before the
	// whole conversation and bust OpenAI's automatic prefix cache every turn (a room flip or
	// the daily date rollover would re-bill the entire history). The volatile half rides on
	// the LAST user message instead (see buildInputItems), outside the cached prefix.
	if r.System != "" {
		p.Instructions = oai.String(r.System)
	}

	// Reasoning effort (always set — GPT-5.6 defaults to a non-trivial effort when omitted,
	// so an explicit none keeps classifier/utility turns cheap).
	p.Reasoning = shared.ReasoningParam{Effort: o.effortFor(r.Effort)}

	// Include encrypted reasoning content so thinking blocks survive the stateless
	// round-trip. OpenAI-only: xAI (grok) doesn't support the includable.
	if o.includeEnc {
		p.Include = []responses.ResponseIncludable{
			responses.ResponseIncludable("reasoning.encrypted_content"),
		}
	}

	// Tools → flat function tool defs. The CLI's web_search FUNCTION tool is swapped for
	// the Responses API's NATIVE web_search built-in: the serving model searches within
	// the request (no side-channel model hop, no cold second context). The synthesized
	// answer arrives as ordinary output_text; the web_search_call output items are
	// ignored by streamOnce (positive-only item handling) — nothing round-trips.
	if len(r.Tools) > 0 {
		fnTools, nativeSearch := provcore.SplitWebSearchTool(r.Tools)
		tools := make([]responses.ToolUnionParam, 0, len(fnTools)+1)
		for _, t := range fnTools {
			ft := &responses.FunctionToolParam{
				Name:       t.Name,
				Parameters: t.InputSchema,
				Strict:     oai.Bool(false), // our tool schemas aren't always strict-JSON-Schema-compatible
			}
			if t.Description != "" {
				ft.Description = oai.String(t.Description)
			}
			tools = append(tools, responses.ToolUnionParam{OfFunction: ft})
		}
		if nativeSearch {
			tools = append(tools, responses.ToolUnionParam{OfWebSearch: &responses.WebSearchToolParam{Type: responses.WebSearchToolTypeWebSearch}})
		}
		p.Tools = tools
		// Force a specific tool when asked (structured output): the model MUST call it.
		if r.ToolChoice != "" {
			p.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
				OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: r.ToolChoice},
			}
		}
	}

	// Messages → input items. The volatile system half rides on the last user message
	// (outside the cached instruction prefix — see the Instructions note above).
	p.Input = responses.ResponseNewParamsInputUnion{
		OfInputItemList: o.buildInputItems(r.Messages, r.SystemVolatile),
	}

	return p
}

// buildInputItems maps the block-structured conversation onto Responses API input items.
// volatile, when set, is appended as a trailing input_message so it sits AFTER the cached
// conversation prefix (see buildParams' Instructions note) rather than in front of it.
func (o *OpenAI) buildInputItems(msgs []wire.Message, volatile string) responses.ResponseInputParam {
	var items responses.ResponseInputParam
	for _, msg := range msgs {
		switch msg.Role {
		case "assistant":
			items = append(items, o.assistantItems(msg.Blocks)...)
		default: // user
			items = append(items, o.userItems(msg.Blocks)...)
		}
	}
	if strings.TrimSpace(volatile) != "" {
		items = append(items, responses.ResponseInputItemParamOfInputMessage(
			[]responses.ResponseInputContentUnionParam{
				responses.ResponseInputContentParamOfInputText(volatile),
			},
			"user",
		))
	}
	return items
}

// assistantItems emits the assistant turn's blocks as output items: text → an
// output_message with output_text content; tool_use → a function_call item; thinking →
// a reasoning item carrying its encrypted content for the stateless round-trip.
func (o *OpenAI) assistantItems(blocks []wire.Block) []responses.ResponseInputItemUnionParam {
	var out []responses.ResponseInputItemUnionParam
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, responses.ResponseInputItemParamOfOutputMessage(
					[]responses.ResponseOutputMessageContentUnionParam{
						responses.ResponseOutputMessageContentUnionParam{
							OfOutputText: &responses.ResponseOutputTextParam{Text: b.Text},
						},
					},
					"", // no item id (we don't carry OpenAI item ids across turns)
					responses.ResponseOutputMessageStatusCompleted,
				))
			}
		case "tool_use":
			// A prior assistant tool call → a function_call input item. Arguments is the
			// JSON string the model produced; CallID is the tool_use block's ID.
			out = append(out, responses.ResponseInputItemParamOfFunctionCall(
				string(b.Input), b.ID, b.Name,
			))
		case "thinking":
			// Round-trip the encrypted reasoning so multi-turn tool-use turns keep their
			// reasoning context in stateless mode. The encrypted content is opaque to us;
			// we carry it verbatim. (When empty, skip — no reasoning to preserve.)
			if b.Signature == "" {
				continue
			}
			// Drop foreign reasoning: an Anthropic thinking block (no "rs_" id) replayed
			// here would be sent as OpenAI encrypted_content → 400 could-not-decrypt.
			// This is the other half of the vendor-switch fix (see anthropic buildWire).
			if !strings.HasPrefix(b.ID, "rs_") {
				continue
			}
			// The Responses API requires each reasoning item to round-trip with its
			// ORIGINAL, UNIQUE id (captured at stream time as b.ID, e.g. "rs_abc…").
			id := b.ID
			rp := &responses.ResponseReasoningItemParam{
				ID:               id,
				EncryptedContent: oai.String(b.Signature),
				Summary:          []responses.ResponseReasoningItemSummaryParam{},
			}
			if b.Thinking != "" {
				rp.Summary = append(rp.Summary, responses.ResponseReasoningItemSummaryParam{
					Text: b.Thinking,
				})
			}
			out = append(out, responses.ResponseInputItemUnionParam{OfReasoning: rp})
		}
	}
	return out
}

// inputFilePart builds a Responses input_file content part from a document block
// (base64 data URL — the Responses API's native PDF input).
func inputFilePart(b wire.Block) responses.ResponseInputContentUnionParam {
	return responses.ResponseInputContentUnionParam{OfInputFile: &responses.ResponseInputFileParam{
		FileData: oai.String("data:" + b.Source.MediaType + ";base64," + b.Source.Data),
		Filename: oai.String("document.pdf"),
	}}
}

// userItems emits the user turn's blocks as input items: tool_result → a
// function_call_output item (answering the preceding assistant function_call); text → an
// input_message; image/document → an input_message with image / input_file content parts.
func (o *OpenAI) userItems(blocks []wire.Block) []responses.ResponseInputItemUnionParam {
	var out []responses.ResponseInputItemUnionParam
	// Media parts destined for the trailing user input_message. Documents inside a
	// tool_result land here too: the Responses function output is a string, so the
	// PDF itself rides as user content right after the tool output — the model sees
	// the file natively instead of a dropped block (images in tool results stay
	// text-folded as before).
	var media []responses.ResponseInputContentUnionParam
	// Tool results first (they answer the assistant's preceding function calls), then text/media.
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		content := b.Content
		if len(b.ContentBlocks) > 0 {
			var texts []string
			for _, cb := range b.ContentBlocks {
				switch cb.Type {
				case "text":
					texts = append(texts, cb.Text)
				case "document":
					if cb.Source != nil {
						media = append(media, inputFilePart(cb))
					}
				}
			}
			content = strings.Join(texts, "\n")
		}
		if b.IsError {
			content = "ERROR: " + content
		}
		if content == "" {
			content = "(no output)"
		}
		out = append(out, responses.ResponseInputItemParamOfFunctionCallOutput(
			b.ToolUseID, content,
		))
	}
	// Then text + images + documents as a single input_message (grouped).
	var text strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "image":
			if b.Source != nil {
				ip := responses.ResponseInputContentParamOfInputImage(responses.ResponseInputImageDetailAuto)
				// Set the image URL (data URI) via the param's field — the constructor leaves
				// the source empty, so we fill it from the block's base64 data.
				ip.OfInputImage.ImageURL = oai.String("data:" + b.Source.MediaType + ";base64," + b.Source.Data)
				media = append(media, ip)
			}
		case "document":
			if b.Source != nil {
				media = append(media, inputFilePart(b))
			}
		}
	}
	if text.Len() > 0 || len(media) > 0 {
		content := media
		if text.Len() > 0 {
			// Text first, then media.
			content = append([]responses.ResponseInputContentUnionParam{
				responses.ResponseInputContentParamOfInputText(text.String()),
			}, media...)
		}
		out = append(out, responses.ResponseInputItemParamOfInputMessage(
			content, "user",
		))
	}
	return out
}

// callAccum tracks a function call as its arguments stream in delta-by-delta.
type callAccum struct {
	id   string
	name string
	args strings.Builder
}

// Stream sends a streaming Responses request, forwarding text/usage to h as they arrive,
// and returns the fully assembled Response (so the agent loop is identical to the
// non-streaming path).
func (o *OpenAI) Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	if o.apiKey == "" {
		return wire.Response{}, fmt.Errorf("no API key (set %s)", o.keyEnv)
	}
	params := o.buildParams(r, r.MaxTokens) // 0 = uncapped (field omitted; provider's model max applies)

	// Shared emitted-aware retry: a stream can't resume mid-flight, so it only
	// re-attempts when NOTHING was forwarded to h yet — see provcore.StreamWithRetry.
	return provcore.StreamWithRetry(ctx, func() (wire.Response, bool, error) {
		return o.streamOnce(ctx, params, r, h)
	})
}

// streamOnce runs a single streaming attempt. emitted reports whether any output bytes
// were forwarded to h (a stream that has started can't be safely retried).
func (o *OpenAI) streamOnce(ctx context.Context, params responses.ResponseNewParams, r wire.Request, h wire.StreamHandler) (_ wire.Response, emitted bool, _ error) {
	cl := o.client()
	stream := cl.Responses.NewStreaming(ctx, params)

	// Function calls accumulate by output index; text accumulates by item id; reasoning
	// by item id. The stream union is a FLATTENED struct — we switch on evt.Type and read
	// fields directly (there is no AsAny() on ResponseStreamEventUnion).
	calls := map[int64]*callAccum{}
	var callOrder []int64
	textItems := map[string]*strings.Builder{} // itemID → text accumulator
	var textOrder []string
	reasoningItems := map[string]*strings.Builder{}
	var reasoningOrder []string
	reasoningEncrypted := map[string]string{}
	var inTok, outTok, cacheRead int
	// searchCount: completed web_search_call output items — the native web_search
	// built-in's per-request fee counter. Counts for grok too (the embedded
	// adapter serves xAI's Responses-compatible Agent Tools, which bill the same
	// per-invocation way; Live Search's per-source num_sources_used card is gone —
	// search_parameters 410s since 2026-07-18).
	var searchCount int
	var stopReason string
	notifyUsage := func() {
		if h.Usage != nil {
			h.Usage(inTok, outTok)
		}
	}

	for stream.Next() {
		evt := stream.Current()
		switch evt.Type {
		case "response.output_text.delta":
			// Accumulate text per item id (a response may have multiple output messages).
			acc := textItems[evt.ItemID]
			if acc == nil {
				acc = &strings.Builder{}
				textItems[evt.ItemID] = acc
				textOrder = append(textOrder, evt.ItemID)
			}
			acc.WriteString(evt.Delta)
			if h.Text != nil {
				h.Text(evt.Delta)
				emitted = true // output forwarded — no longer safe to retry this stream
			}

		case "response.reasoning_text.delta":
			// Reasoning text — captured for round-trip, NOT streamed to the user
			// (reasoning is the model's scratch space, same as Anthropic thinking).
			acc := reasoningItems[evt.ItemID]
			if acc == nil {
				acc = &strings.Builder{}
				reasoningItems[evt.ItemID] = acc
				reasoningOrder = append(reasoningOrder, evt.ItemID)
			}
			acc.WriteString(evt.Delta)

		case "response.function_call_arguments.delta":
			// Accumulate a function call's arguments as they stream in.
			c := calls[evt.OutputIndex]
			if c == nil {
				c = &callAccum{}
				calls[evt.OutputIndex] = c
				callOrder = append(callOrder, evt.OutputIndex)
			}
			c.args.WriteString(evt.Delta)

		case "response.output_item.added", "response.output_item.done":
			// An output item was added/done. A function_call item carries its id+name
			// here (the arguments stream separately above); a reasoning item carries its
			// encrypted_content here when done.
			item := evt.Item
			if item.Type == "web_search_call" && evt.Type == "response.output_item.done" {
				// A completed native search — one per-request fee upstream. Counted on
				// done only (added would double-count; an errored search isn't billed).
				searchCount++
			}
			if item.Type == "function_call" {
				c := calls[evt.OutputIndex]
				if c == nil {
					c = &callAccum{}
					calls[evt.OutputIndex] = c
					callOrder = append(callOrder, evt.OutputIndex)
				}
				if item.CallID != "" {
					c.id = item.CallID
				}
				if item.Name != "" {
					c.name = item.Name
				}
			}
			if item.Type == "reasoning" && evt.Type == "response.output_item.done" {
				if item.EncryptedContent != "" {
					reasoningEncrypted[item.ID] = item.EncryptedContent
					// Register the item in reasoningOrder even when it emitted NO
					// reasoning_text.delta (the normal GPT-5.x case: encrypted CoT, no
					// summary). Assembly iterates reasoningOrder, so without this the
					// encrypted content is dropped and the next tool-use turn round-trips
					// function calls WITHOUT their required reasoning items → 400.
					if _, seen := reasoningItems[item.ID]; !seen {
						reasoningItems[item.ID] = &strings.Builder{}
						reasoningOrder = append(reasoningOrder, item.ID)
					}
				}
			}

		case "response.completed":
			// Final usage from the completed response.
			u := evt.Response.Usage
			inTok = int(u.InputTokens)
			outTok = int(u.OutputTokens)
			cacheRead = int(u.InputTokensDetails.CachedTokens)
			// Stop reason: infer from the output. The Responses API doesn't put a single
			// stop_reason on the completed event; we derive it from the output items below.
			notifyUsage()

		case "response.incomplete":
			// A truncated turn (hit max_output_tokens or a content limit). The API emits
			// this INSTEAD of response.completed, so without handling it usage stayed 0
			// (metered free) and the stop reason inferred as end_turn — the CLI's
			// truncation/continue handling never fired. Read usage here and mark it.
			u := evt.Response.Usage
			inTok = int(u.InputTokens)
			outTok = int(u.OutputTokens)
			cacheRead = int(u.InputTokensDetails.CachedTokens)
			stopReason = "max_tokens"
			notifyUsage()

		case "error", "response.failed":
			// An error event. The failure reason for response.failed lives at
			// evt.Response.Error (evt.Message/evt.Code are populated only on the "error"
			// event variant), so read both — otherwise the message was empty and
			// overflow self-heal never triggered.
			msg := evt.Message
			if msg == "" {
				msg = evt.Code
			}
			if msg == "" && evt.Response.Error.Message != "" {
				msg = evt.Response.Error.Message
			}
			// Preserve partial usage so a failed stream is still billed for the
			// tokens consumed before the error (mirrors the Anthropic path).
			partial := wire.Response{
				InputTokens:     inTok - cacheRead,
				CacheReadTokens: cacheRead,
				OutputTokens:    outTok,
				SearchCount:     searchCount,
				Model:           r.Model,
				Backend:         o.backend,
			}
			if provcore.IsOverflowMessage(msg) {
				return partial, emitted, &provcore.ContextOverflowError{Backend: o.backend, Message: msg}
			}
			return partial, emitted, fmt.Errorf("openai %s: %s", evt.Type, msg)
		}
	}
	if err := stream.Err(); err != nil {
		// Preserve partial usage on stream-level errors too (same rationale as above).
		partial := wire.Response{
			InputTokens:     inTok - cacheRead,
			CacheReadTokens: cacheRead,
			OutputTokens:    outTok,
			SearchCount:     searchCount,
			Model:           r.Model,
			Backend:         o.backend,
		}
		if ctx.Err() != nil {
			return partial, emitted, ctx.Err()
		}
		var apiErr *oai.Error
		if errors.As(err, &apiErr) {
			msg := apiErr.Message
			if provcore.IsOverflowMessage(msg) {
				return partial, emitted, &provcore.ContextOverflowError{Backend: o.backend, Message: msg}
			}
			return partial, emitted, fmt.Errorf("openai %d: %s", apiErr.StatusCode, msg)
		}
		return partial, emitted, fmt.Errorf("openai stream: %w", err)
	}

	// Assemble blocks in the order the Responses API expects them back: reasoning items
	// FIRST (each reasoning item must precede the function_call it produced, and a trailing
	// reasoning item with no following item is rejected), then text, then function calls.
	// The agent loop reads text/tool_use/thinking regardless of order.
	var blocks []wire.Block
	// Reasoning blocks — carry the REAL item id (for the unique-id round-trip) and the
	// encrypted content as the signature. Emitted even when the summary text is empty,
	// because the encrypted content is what the next turn needs.
	for _, id := range reasoningOrder {
		acc := reasoningItems[id]
		enc, hasEnc := reasoningEncrypted[id]
		if (acc == nil || acc.Len() == 0) && !hasEnc {
			continue // nothing to round-trip
		}
		b := wire.Block{Type: "thinking", ID: id}
		if acc != nil {
			b.Thinking = acc.String()
		}
		if hasEnc {
			b.Signature = enc
		}
		blocks = append(blocks, b)
	}
	for _, id := range textOrder {
		if acc := textItems[id]; acc != nil && acc.Len() > 0 {
			blocks = append(blocks, wire.Block{Type: "text", Text: acc.String()})
		}
	}
	// Function calls.
	hasCalls := false
	for _, idx := range callOrder {
		c := calls[idx]
		if c == nil || c.name == "" {
			continue
		}
		input := strings.TrimSpace(c.args.String())
		if input == "" {
			input = "{}"
		}
		blocks = append(blocks, wire.Block{Type: "tool_use", ID: c.id, Name: c.name, Input: json.RawMessage(input)})
		hasCalls = true
	}

	// A tool call always means tool_use; otherwise keep an already-set reason
	// (max_tokens from response.incomplete) or infer end_turn from produced content.
	if hasCalls {
		stopReason = "tool_use"
	} else if stopReason == "" && len(blocks) > 0 {
		stopReason = "end_turn"
	}

	out := wire.Response{
		StopReason:      stopReason,
		Blocks:          blocks,
		InputTokens:     inTok - cacheRead, // disjoint: uncached prompt only (matches the oaClient convention)
		CacheReadTokens: cacheRead,
		OutputTokens:    outTok,
		SearchCount:     searchCount,
		Model:           r.Model,
		Backend:         o.backend,
	}
	if len(r.Tools) > 0 && hasCalls {
		out.ToolOrigin = "structured_openai"
	}
	return out, emitted, nil
}

// WebSearch answers a query using the Responses API's built-in web_search tool, returning
// the synthesized text (replaces Anthropic's server-side web_search).
func (o *OpenAI) WebSearch(ctx context.Context, query string) (string, wire.Response, error) {
	if o.apiKey == "" {
		return "", wire.Response{}, fmt.Errorf("no API key (set %s)", o.keyEnv)
	}
	params := responses.ResponseNewParams{
		Model:           shared.ResponsesModel(o.defaultModel),
		Instructions:    oai.String(provcore.WebSearchSystemPrompt),
		MaxOutputTokens: oai.Int(3000),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: responses.ResponseInputParam{
				responses.ResponseInputItemParamOfInputMessage(
					[]responses.ResponseInputContentUnionParam{
						responses.ResponseInputContentParamOfInputText(query),
					},
					"user",
				),
			},
		},
		Tools: []responses.ToolUnionParam{
			{OfWebSearch: &responses.WebSearchToolParam{Type: responses.WebSearchToolTypeWebSearch}},
		},
	}
	cl := o.client()
	resp, err := provcore.WithRetry(ctx, func() (*responses.Response, error) {
		return cl.Responses.New(ctx, params)
	})
	if err != nil {
		return "", wire.Response{Model: o.defaultModel, Backend: o.backend}, err
	}
	return strings.TrimSpace(resp.OutputText()), o.sideUsage(resp), nil
}

// openaiSideUsage maps a Responses API result onto the neutral usage shape for
// side-channel metering (websearch/webfetch run on Terra). SearchCount counts the
// web_search_call output items — both /v1/websearch AND /v1/webfetch run the
// native web_search tool here (the Responses API has no dedicated fetch tool), so
// each bills the per-request search fee on top of tokens (SearchFeeUSD).
func (o *OpenAI) sideUsage(resp *responses.Response) wire.Response {
	searches := 0
	for _, item := range resp.Output {
		if item.Type == "web_search_call" {
			searches++
		}
	}
	return wire.Response{
		Model:        o.defaultModel,
		Backend:      o.backend,
		InputTokens:  int(resp.Usage.InputTokens),
		OutputTokens: int(resp.Usage.OutputTokens),
		SearchCount:  searches,
	}
}

// WebFetch retrieves a URL using the web_search tool with the URL in the prompt (the
// Responses API has no dedicated url_fetch tool; the model retrieves and summarizes the
// URL content). Returns the readable content as text.
func (o *OpenAI) WebFetch(ctx context.Context, url string) (string, wire.Response, error) {
	if o.apiKey == "" {
		return "", wire.Response{}, fmt.Errorf("no API key (set %s)", o.keyEnv)
	}
	params := responses.ResponseNewParams{
		Model:           shared.ResponsesModel(o.defaultModel),
		MaxOutputTokens: oai.Int(8192),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: responses.ResponseInputParam{
				responses.ResponseInputItemParamOfInputMessage(
					[]responses.ResponseInputContentUnionParam{
						responses.ResponseInputContentParamOfInputText(
							"Fetch this URL and return its full readable content verbatim as markdown, no commentary: " + url,
						),
					},
					"user",
				),
			},
		},
		Tools: []responses.ToolUnionParam{
			{OfWebSearch: &responses.WebSearchToolParam{Type: responses.WebSearchToolTypeWebSearch}},
		},
	}
	cl := o.client()
	resp, err := provcore.WithRetry(ctx, func() (*responses.Response, error) {
		return cl.Responses.New(ctx, params)
	})
	if err != nil {
		return "", wire.Response{Model: o.defaultModel, Backend: o.backend}, err
	}
	return strings.TrimSpace(resp.OutputText()), o.sideUsage(resp), nil
}

// Model returns the default model id (for display). The OpenAI provider serves whatever
// model resolve.go chose; this is the strong-tier default.
func (o *OpenAI) Model() string { return o.defaultModel }

// _keep param import referenced (used in buildParams via oai.String/Bool/Int aliases).
var _ = param.NewOpt[string]
