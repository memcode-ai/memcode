package compat

// translate.go is the PURE translation between the OpenAI-compat wire and the
// internal protocol (wire.Request / wire.Response). Nothing here touches
// routing, auth, or doctrine: model resolution, header→Intent mapping, and the
// delegate-doctrine append live in the server package (they need the router and
// its allow-lists). Everything behind the translation — steering, adapters,
// metering, sanitization — runs UNCHANGED on the translated request.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/wire"
)

// Turn is the translated body of one inbound chat-completions request — the
// pieces that slot straight into wire.Request. The caller stamps Model,
// BillingLane, Session, and the ops SystemPrefix.
type Turn struct {
	System         string // first system message → the cacheable stable prefix
	SystemVolatile string // second system message (3+ concatenate) → the volatile suffix
	Messages       []wire.Message
	Tools          []wire.ToolDef
	ToolChoice     string
	Effort         wire.Effort
	MaxTokens      int
}

// ToTurn translates one inbound ChatRequest body into a Turn. Errors are
// client errors (400): unsupported roles/parts, malformed data URLs, a forced
// tool_choice naming an undefined tool, and so on.
func ToTurn(req ChatRequest) (Turn, error) {
	var t Turn

	// Messages. The two-system convention: first system message = stable
	// prefix, second = volatile; a third or later concatenates into volatile
	// (any compat endpoint just sees N system messages and concatenates — the
	// gateway places the two halves per-vendor exactly as today). Tool results
	// ride the next user message as tool_result blocks; a contiguous run of
	// tool messages becomes ONE user message (the internal shape the adapters
	// already speak).
	var systems []string
	var pendingResults []wire.Block
	flush := func() {
		if len(pendingResults) > 0 {
			t.Messages = append(t.Messages, wire.Message{Role: "user", Blocks: pendingResults})
			pendingResults = nil
		}
	}
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			txt, err := textContent(m.Content)
			if err != nil {
				return t, fmt.Errorf("messages[%d] (%s): %w", i, m.Role, err)
			}
			systems = append(systems, txt)
		case "user":
			flush()
			blocks, err := userBlocks(m.Content)
			if err != nil {
				return t, fmt.Errorf("messages[%d] (user): %w", i, err)
			}
			t.Messages = append(t.Messages, wire.Message{Role: "user", Blocks: blocks})
		case "assistant":
			flush()
			blocks, err := assistantBlocks(m)
			if err != nil {
				return t, fmt.Errorf("messages[%d] (assistant): %w", i, err)
			}
			t.Messages = append(t.Messages, wire.Message{Role: "assistant", Blocks: blocks})
		case "tool":
			if m.ToolCallID == "" {
				return t, fmt.Errorf("messages[%d] (tool): tool_call_id is required", i)
			}
			txt, err := textContent(m.Content)
			if err != nil {
				return t, fmt.Errorf("messages[%d] (tool): %w", i, err)
			}
			pendingResults = append(pendingResults, wire.Block{
				Type: "tool_result", ToolUseID: m.ToolCallID, Content: txt,
			})
		default:
			return t, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	flush()
	if len(t.Messages) == 0 {
		return t, errors.New("messages must include at least one non-system message")
	}
	if len(systems) > 0 {
		t.System = systems[0]
	}
	if len(systems) > 1 {
		t.SystemVolatile = strings.Join(systems[1:], "\n\n")
	}

	// Tools + tool_choice.
	forced, dropTools, err := toolChoice(req.ToolChoice, req.Tools)
	if err != nil {
		return t, err
	}
	if !dropTools {
		t.Tools = toolDefs(req.Tools)
		t.ToolChoice = forced
	}

	t.Effort = EffortFrom(req.ReasoningEffort)
	t.MaxTokens = req.MaxCompletionTokens
	if t.MaxTokens == 0 {
		t.MaxTokens = req.MaxTokens
	}
	return t, nil
}

// textContent flattens a content union into plain text (system/tool/assistant
// text). Only text parts are legal in the array form here.
func textContent(c MessageContent) (string, error) {
	if !c.IsParts {
		return c.Text, nil
	}
	var parts []string
	for _, p := range c.Parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part %q is not allowed here (text only)", p.Type)
		}
		parts = append(parts, p.Text)
	}
	return strings.Join(parts, "\n"), nil
}

// userBlocks translates a user message's content into blocks: text parts,
// image_url data URLs (vision), and file parts (documents).
func userBlocks(c MessageContent) ([]wire.Block, error) {
	if !c.IsParts {
		if c.Text == "" {
			return nil, errors.New("content is empty")
		}
		return []wire.Block{wire.TextBlock(c.Text)}, nil
	}
	var blocks []wire.Block
	for i, p := range c.Parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, wire.TextBlock(p.Text))
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return nil, fmt.Errorf("part %d: image_url.url is required", i)
			}
			src, err := mediaFromDataURL(p.ImageURL.URL)
			if err != nil {
				return nil, fmt.Errorf("part %d: image_url must be a data: URL (the gateway does not fetch remote images): %w", i, err)
			}
			blocks = append(blocks, wire.Block{Type: "image", Source: src})
		case "file":
			if p.File == nil {
				return nil, fmt.Errorf("part %d: file payload is required", i)
			}
			if p.File.FileID != "" {
				return nil, fmt.Errorf("part %d: file_id is not supported — inline the document as a data: URL in file_data", i)
			}
			if p.File.FileData == "" {
				return nil, fmt.Errorf("part %d: file.file_data is required", i)
			}
			src, err := mediaFromDataURL(p.File.FileData)
			if err != nil {
				return nil, fmt.Errorf("part %d: file.file_data must be a data: URL: %w", i, err)
			}
			blocks = append(blocks, wire.Block{Type: "document", Source: src})
		default:
			return nil, fmt.Errorf("part %d: unsupported content part type %q", i, p.Type)
		}
	}
	if len(blocks) == 0 {
		return nil, errors.New("content is empty")
	}
	return blocks, nil
}

// mediaFromDataURL parses a `data:<mediatype>;base64,<payload>` URL into a
// MediaSource. The payload must be valid base64 (checked up front so a junk
// image fails as a clean 400 here, not an opaque vendor error downstream).
func mediaFromDataURL(url string) (*wire.MediaSource, error) {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return nil, fmt.Errorf("not a data: URL")
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return nil, fmt.Errorf("malformed data: URL (no comma)")
	}
	mediaType, isB64 := strings.CutSuffix(meta, ";base64")
	if !isB64 {
		return nil, fmt.Errorf("data: URL must be base64-encoded")
	}
	if mediaType == "" {
		return nil, fmt.Errorf("data: URL must declare a media type")
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return nil, fmt.Errorf("invalid base64 payload: %v", err)
	}
	return &wire.MediaSource{Type: "base64", MediaType: mediaType, Data: payload}, nil
}

// assistantBlocks rebuilds an assistant history message: memcode_opaque
// reasoning blocks re-expanded FIRST (their original response position), then
// text, then tool calls.
func assistantBlocks(m ChatMessage) ([]wire.Block, error) {
	var blocks []wire.Block
	for i, raw := range m.MemcodeOpaque {
		var b wire.Block
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("memcode_opaque[%d]: %w", i, err)
		}
		// Only reasoning blocks may ride the opaque channel — it exists for the
		// verbatim thinking round-trip, not as a side door for arbitrary blocks.
		if b.Type != "thinking" && b.Type != "redacted_thinking" {
			return nil, fmt.Errorf("memcode_opaque[%d]: unsupported block type %q", i, b.Type)
		}
		blocks = append(blocks, b)
	}
	txt, err := textContent(m.Content)
	if err != nil {
		return nil, err
	}
	if txt != "" {
		blocks = append(blocks, wire.TextBlock(txt))
	}
	for i, tc := range m.ToolCalls {
		if tc.Type != "" && tc.Type != "function" {
			return nil, fmt.Errorf("tool_calls[%d]: unsupported type %q", i, tc.Type)
		}
		if tc.Function.Name == "" {
			return nil, fmt.Errorf("tool_calls[%d]: function.name is required", i)
		}
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		blocks = append(blocks, wire.Block{
			Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args),
		})
	}
	if len(blocks) == 0 {
		return nil, errors.New("assistant message has no content")
	}
	return blocks, nil
}

// toolDefs translates tool definitions (parameters → input_schema verbatim —
// tools already ride the wire as JSON Schema).
func toolDefs(tools []Tool) []wire.ToolDef {
	if len(tools) == 0 {
		return nil
	}
	out := make([]wire.ToolDef, 0, len(tools))
	for _, t := range tools {
		out = append(out, wire.ToolDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	return out
}

// toolChoice interprets the standard union. Returns the forced tool name (""
// = the model chooses) and whether tool defs should be dropped entirely
// ("none" — the internal wire expresses "no tools this turn" by omission).
// "required" with exactly one tool forces that tool (the classifier pattern);
// with several it degrades to model-chooses (the internal wire cannot express
// "some tool, any tool"). A forced name must be among the defined tools.
func toolChoice(raw json.RawMessage, tools []Tool) (forced string, dropTools bool, err error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "", "auto":
			return "", false, nil
		case "none":
			return "", true, nil
		case "required":
			if len(tools) == 1 {
				return tools[0].Function.Name, false, nil
			}
			return "", false, nil
		default:
			return "", false, fmt.Errorf("unsupported tool_choice %q", s)
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", false, fmt.Errorf("malformed tool_choice: %w", err)
	}
	if obj.Function.Name == "" {
		return "", false, errors.New("tool_choice.function.name is required")
	}
	for _, t := range tools {
		if t.Function.Name == obj.Function.Name {
			return obj.Function.Name, false, nil
		}
	}
	return "", false, fmt.Errorf("tool_choice names undefined tool %q", obj.Function.Name)
}

// EffortFrom maps the standard reasoning_effort vocabulary onto the abstract
// Effort. Unknown values degrade to off rather than erroring (reasoning
// controls are an OPTIONAL capability, never a request-killer).
func EffortFrom(s string) wire.Effort {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "minimal", "low":
		return wire.EffortLow
	case "medium":
		return wire.EffortMedium
	case "high", "xhigh":
		return wire.EffortHigh
	default:
		return wire.EffortOff
	}
}

// ── response direction ──────────────────────────────────────────────────────

// ResponseFrom renders a (sanitized) internal response as a chat completion.
func ResponseFrom(resp wire.Response, id string, created int64) ChatResponse {
	msg := ResponseMessage{
		Role:          "assistant",
		ToolCalls:     ToolCallsFrom(resp.ToolUses()),
		MemcodeOpaque: OpaqueFrom(resp.Blocks),
	}
	// Standard shape: content is null (not "") only when the message is
	// tool-calls-only.
	if txt := resp.Text(); txt != "" || len(msg.ToolCalls) == 0 {
		msg.Content = &txt
	}
	u := UsageFrom(resp)
	return ChatResponse{
		ID: id, Object: "chat.completion", Created: created, Model: resp.Model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: FinishReasonFrom(resp.StopReason)}},
		Usage:   &u,
		Memcode: ExtFrom(resp),
	}
}

// ToolCallsFrom renders tool_use blocks as standard tool calls. Empty inputs
// become "{}" so clients can always json-parse arguments.
func ToolCallsFrom(uses []wire.Block) []ToolCall {
	if len(uses) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(uses))
	for _, b := range uses {
		args := strings.TrimSpace(string(b.Input))
		if args == "" {
			args = "{}"
		}
		out = append(out, ToolCall{
			ID: b.ID, Type: "function",
			Function: FunctionCall{Name: b.Name, Arguments: args},
		})
	}
	return out
}

// OpaqueFrom extracts the response's reasoning blocks as the memcode_opaque
// array (each element the block's wire JSON, re-expandable verbatim).
func OpaqueFrom(blocks []wire.Block) []json.RawMessage {
	var out []json.RawMessage
	for _, b := range blocks {
		if b.Type != "thinking" && b.Type != "redacted_thinking" {
			continue
		}
		raw, err := json.Marshal(b)
		if err != nil {
			continue // marshal of these fields cannot realistically fail; never kill a response over telemetry
		}
		out = append(out, raw)
	}
	return out
}

// FinishReasonFrom maps internal stop reasons onto the standard vocabulary.
func FinishReasonFrom(stop string) string {
	switch stop {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

// UsageFrom converts internal (Anthropic-style, cache-exclusive) counts to the
// standard shape (prompt_tokens cache-INCLUSIVE, cached subset in details).
func UsageFrom(resp wire.Response) Usage {
	prompt := resp.InputTokens + resp.CacheReadTokens + resp.CacheWriteTokens
	u := Usage{
		PromptTokens:     prompt,
		CompletionTokens: resp.OutputTokens,
		TotalTokens:      prompt + resp.OutputTokens,
	}
	if resp.CacheReadTokens > 0 {
		u.PromptTokensDetails = &PromptTokensDetails{CachedTokens: resp.CacheReadTokens}
	}
	return u
}

// ChunkFrom wraps one streamed delta in the standard chunk shape (the wire
// constants live here, not at the send sites).
func ChunkFrom(id string, created int64, model string, choices []ChunkChoice) ChatChunk {
	return ChatChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: choices}
}

// ErrorFrom wraps a message in the standard {"error":{message,type,code}}
// envelope — the one shape both the HTTP error writer and the mid-stream
// error event emit.
func ErrorFrom(msg, typ, code string) ErrorResponse {
	return ErrorResponse{Error: ErrorBody{Message: msg, Type: typ, Code: code}}
}

// ModelEntryFrom wraps one catalog label + its selection facts in the standard
// model-list entry shape.
func ModelEntryFrom(label string, meta ModelMeta) ModelEntry {
	return ModelEntry{ID: label, Object: "model", OwnedBy: "memcode", Memcode: &meta}
}

// ModelListFrom wraps entries + the list-level extension in the standard list
// shape. Data is never null on the wire (clients range over it).
func ModelListFrom(entries []ModelEntry, ext *ModelsExt) ModelList {
	if entries == nil {
		entries = []ModelEntry{}
	}
	return ModelList{Object: "list", Data: entries, Memcode: ext}
}

// ExtFrom builds the memcode extension object from a (sanitized) response.
func ExtFrom(resp wire.Response) *MemcodeExt {
	return &MemcodeExt{
		Byok:           resp.BYOK,
		FallbackReason: resp.FallbackReason,
		SearchCount:    resp.SearchCount,
		ContextWindow:  resp.ContextWindow,
		InputBudget:    resp.InputBudget,
		Pool:           resp.Pool,
	}
}
