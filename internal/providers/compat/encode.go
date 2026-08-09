package compat

// encode.go — the pure translation from the internal protocol (wire.Request)
// onto the compat wire, mirroring the gateway's inbound translation
// (api/internal/compat/translate.go) in reverse so a round trip rebuilds the
// same wire.Request the legacy path would have carried.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/wire"
)

// composeLocal fills the two system halves of a request that still carries the
// LEGACY shape (Mode stamped, doctrine not yet composed) via the transport's
// Compose hook — the CALLER owns doctrine (the CLI passes its doctrine
// renderer; the gateway lane passes nothing). A composed request
// (SystemVolatile set) or a raw one (no Mode) passes through untouched.
func (t *Transport) composeLocal(r wire.Request) (wire.Request, error) {
	if t.compose == nil || r.Mode == "" || r.SystemVolatile != "" {
		return r, nil
	}
	return t.compose(r)
}

// encodeBody renders a (composed) wire.Request as the wire body. memcode
// gates extension (3): reasoning blocks ride memcode_opaque only when the
// backend is the memcode gateway — strict third-party validators reject
// unknown message fields, so off-gateway turns simply skip the reasoning
// round-trip (the same limitation every compat client has).
func (t *Transport) encodeBody(r wire.Request, stream bool) (ChatRequest, error) {
	memcode := t.memcode
	body := ChatRequest{Model: r.Pin, Stream: stream} // the selected catalog label / endpoint model id
	if body.Model == "" && t.lane {
		body.Model = r.Model // the lane path carries the resolved raw id on Model
	}
	if stream {
		body.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	if r.MaxTokens > 0 {
		if t.lane {
			body.MaxTokens = r.MaxTokens // the lane's proven spelling (Fireworks)
		} else {
			body.MaxCompletionTokens = r.MaxTokens
		}
	}
	if t.lane {
		// The lane's model-conditional reasoning vocabulary (none/high/max —
		// ported from the gateway's Fireworks client verbatim).
		body.ReasoningEffort = laneReasoningEffort(body.Model, r.Effort, len(r.Tools) > 0)
	} else if r.Effort != wire.EffortOff {
		body.ReasoningEffort = string(r.Effort) // the vocabularies coincide (low|medium|high)
	}
	if r.Session != "" {
		body.User = r.Session // wire-native session affinity; the gateway reflects it into X-Memcode-Session
	}
	if memcode && r.BillingLane != "" {
		body.MemcodeBilling = r.BillingLane // the enforced billing-lane extension
	}

	// The two-system convention: first = stable cacheable prefix, second =
	// volatile suffix. An empty stable half still emits (as "") when a volatile
	// half exists, so the halves keep their positions server-side.
	if r.System != "" || r.SystemVolatile != "" {
		body.Messages = append(body.Messages, ChatMessage{Role: "system", Content: StringContent(r.System)})
	}
	if r.SystemVolatile != "" {
		body.Messages = append(body.Messages, ChatMessage{Role: "system", Content: StringContent(r.SystemVolatile)})
	}

	for i, m := range r.Messages {
		switch m.Role {
		case "user":
			msgs, err := encodeUser(m)
			if err != nil {
				return body, fmt.Errorf("messages[%d] (user): %w", i, err)
			}
			body.Messages = append(body.Messages, msgs...)
		case "assistant":
			msg, err := encodeAssistant(m, memcode)
			if err != nil {
				return body, fmt.Errorf("messages[%d] (assistant): %w", i, err)
			}
			body.Messages = append(body.Messages, msg)
		default:
			return body, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}

	for _, td := range r.Tools {
		body.Tools = append(body.Tools, Tool{Type: "function", Function: FunctionDef{
			Name: td.Name, Description: td.Description, Parameters: td.InputSchema,
		}})
	}
	if r.ToolChoice != "" {
		// The forced-tool form — the reliable cross-provider path to structured
		// output (the classifier pattern).
		tc, err := json.Marshal(struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}{Type: "function", Function: struct {
			Name string `json:"name"`
		}{Name: r.ToolChoice}})
		if err != nil {
			return body, err
		}
		body.ToolChoice = tc
	}
	return body, nil
}

// encodeUser renders one internal user message. tool_result blocks become
// `tool` role messages (the wire shape; the gateway re-bundles a contiguous
// run into one internal user message); everything else becomes user content
// parts. Images inside a tool_result's structured ContentBlocks cannot ride a
// tool message on this wire (tool content is text-only, mirroring the
// gateway's translate), so they are HOISTED into a follow-up user message —
// the ecosystem-standard workaround that keeps vision tool results (browser
// screenshots) visible to the model. The is_error flag has no wire carrier and
// is dropped; the result text itself still rides.
func encodeUser(m wire.Message) ([]ChatMessage, error) {
	var out []ChatMessage
	var parts, hoisted []ContentPart
	for _, b := range m.Blocks {
		switch b.Type {
		case "text":
			parts = append(parts, TextPart(b.Text))
		case "image":
			u, err := dataURL(b.Source)
			if err != nil {
				return nil, err
			}
			parts = append(parts, ContentPart{Type: "image_url", ImageURL: &ImageURLPart{URL: u}})
		case "document":
			u, err := dataURL(b.Source)
			if err != nil {
				return nil, err
			}
			parts = append(parts, ContentPart{Type: "file", File: &FilePart{FileData: u}})
		case "tool_result":
			out = append(out, ChatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: StringContent(toolResultText(b))})
			for _, cb := range b.ContentBlocks {
				if cb.Type != "image" {
					continue
				}
				u, err := dataURL(cb.Source)
				if err != nil {
					return nil, err
				}
				hoisted = append(hoisted,
					TextPart("[image output of tool call "+b.ToolUseID+"]"),
					ContentPart{Type: "image_url", ImageURL: &ImageURLPart{URL: u}})
			}
		default:
			return nil, fmt.Errorf("unsupported block type %q", b.Type)
		}
	}
	if len(hoisted) > 0 {
		parts = append(hoisted, parts...)
	}
	if len(parts) > 0 {
		out = append(out, ChatMessage{Role: "user", Content: userContent(parts)})
	}
	return out, nil
}

// userContent collapses a single text part to the plain-string content form
// (the ecosystem-common shape); anything richer stays the parts array.
func userContent(parts []ContentPart) MessageContent {
	if len(parts) == 1 && parts[0].Type == "text" {
		return StringContent(parts[0].Text)
	}
	return PartsContent(parts...)
}

// toolResultText flattens a tool_result to the text a tool message can carry:
// the structured ContentBlocks' text when present (the flat Content mirrors it
// on that path), else the flat Content.
func toolResultText(b wire.Block) string {
	if len(b.ContentBlocks) == 0 {
		return b.Content
	}
	var texts []string
	for _, cb := range b.ContentBlocks {
		if cb.Type == "text" && cb.Text != "" {
			texts = append(texts, cb.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// encodeAssistant renders one internal assistant message: reasoning blocks →
// memcode_opaque (memcode backend only — dropped otherwise), text → content,
// tool_use → tool_calls.
func encodeAssistant(m wire.Message, memcode bool) (ChatMessage, error) {
	out := ChatMessage{Role: "assistant"}
	var texts []string
	for _, b := range m.Blocks {
		switch b.Type {
		case "thinking", "redacted_thinking":
			if !memcode {
				continue // no reasoning round-trip off-gateway
			}
			raw, err := json.Marshal(b) // Block.MarshalJSON emits the exact wire form the gateway re-expands
			if err != nil {
				return out, err
			}
			out.MemcodeOpaque = append(out.MemcodeOpaque, raw)
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "tool_use":
			args := strings.TrimSpace(string(b.Input))
			if args == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Type: "function",
				Function: FunctionCall{Name: b.Name, Arguments: args}})
		default:
			return out, fmt.Errorf("unsupported block type %q", b.Type)
		}
	}
	if len(texts) > 0 {
		out.Content = StringContent(strings.Join(texts, "\n"))
	}
	if out.Content.IsZero() && len(out.ToolCalls) == 0 && len(out.MemcodeOpaque) == 0 {
		return out, errors.New("assistant message has no content")
	}
	return out, nil
}

// dataURL renders a base64 MediaSource as the data: URL the wire carries.
func dataURL(src *wire.MediaSource) (string, error) {
	if src == nil || src.Data == "" || src.MediaType == "" {
		return "", errors.New("media block missing its base64 source")
	}
	if src.Type != "base64" {
		return "", fmt.Errorf("unsupported media source type %q", src.Type)
	}
	return "data:" + src.MediaType + ";base64," + src.Data, nil
}
