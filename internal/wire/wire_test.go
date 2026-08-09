package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// The gateway wire is a language-agnostic REST contract, NOT Go structs reflected over HTTP.
// Every request/response field MUST serialize as lower_snake_case. This golden test marshals
// fully-populated Request and Response values and asserts the exact snake_case keys are present
// and that NO Go-style PascalCase key leaks — so a future field added without a json tag (which
// would marshal as PascalCase) fails CI instead of hardening into the contract.
func TestWireContractIsSnakeCase(t *testing.T) {
	req := Request{
		Model: "m", System: "s", Messages: []Message{{Role: "user", Blocks: []Block{TextBlock("hi")}}},
		Tools: []ToolDef{{Name: "read_file"}}, MaxTokens: 10, Mode: "chat",
		Facts: map[string]string{"root": "/r"}, Effort: EffortHigh,
		RoutingHint: &RoutingHint{Reason: "self_heal"},
		Purpose:     "main_loop", Session: "sess_x", // json:"-" — must NOT appear
	}
	reqJSON := mustMarshal(t, req)
	assertHasKeys(t, "Request", reqJSON, "model", "system", "messages", "tools", "max_tokens", "mode", "facts", "effort", "routing_hint")
	// Purpose/Session are internal transport hints lifted onto the envelope/header — never in the request body.
	assertNoKeys(t, "Request", reqJSON, "Purpose", "Session", "purpose", "session")

	resp := Response{
		StopReason: "end_turn", Blocks: []Block{TextBlock("ok")}, InputTokens: 1, OutputTokens: 2,
		CacheWriteTokens: 3, CacheReadTokens: 4, ToolOrigin: "structured", Model: "m", Backend: "vllm",
		FallbackReason: "", RequestedModel: "claude", ContextWindow: 200000, InputBudget: 180000,
		Pool: "h200_256k", EstimatedPromptTokens: 123,
	}
	respJSON := mustMarshal(t, resp)
	assertHasKeys(t, "Response", respJSON, "stop_reason", "blocks", "input_tokens", "output_tokens",
		"cache_write_tokens", "cache_read_tokens", "tool_origin", "model", "backend", "requested_model",
		"context_window", "input_budget", "pool", "estimated_prompt_tokens")

	// The catch-all: NO capital letter may begin any JSON key in either payload. This is what
	// makes "added a field without a json tag" impossible to miss.
	for _, c := range []struct{ name, js string }{{"Request", reqJSON}, {"Response", respJSON}} {
		for _, key := range jsonKeys(c.js) {
			if key != "" && key[0] >= 'A' && key[0] <= 'Z' {
				t.Errorf("%s has a PascalCase key %q — every wire field needs a lower_snake_case json tag", c.name, key)
			}
		}
	}
}

// TestToolResultContentBlocks asserts that a tool_result Block carrying structured
// content (text + image) serializes with snake_case keys and a base64 image source —
// the wire shape for tool results that include vision (e.g. browser screenshots).
func TestToolResultContentBlocks(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4E, 0x47} // PNG magic bytes
	tr := ToolResultBlocks("toolu_01", []Block{
		TextBlock("screenshot captured"),
		ImageBlock("image/png", png),
	}, true) // isError=true so is_error appears on the wire (it's omitempty)

	js := mustMarshal(t, tr)
	assertHasKeys(t, "tool_result", js, "type", "tool_use_id", "content_blocks", "is_error")
	assertNoKeys(t, "tool_result", js, "ContentBlocks", "ToolUseID", "IsError")

	// The content_blocks array must contain a text block and an image block with a
	// base64 source — the structured content union providers emit.
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(js), &m) != nil {
		t.Fatalf("unmarshal: %v", js)
	}
	var blocks []map[string]any
	if json.Unmarshal(m["content_blocks"], &blocks) != nil {
		t.Fatalf("unmarshal content_blocks: %v", m["content_blocks"])
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(blocks))
	}
	if blocks[0]["type"] != "text" {
		t.Errorf("first block should be text, got %v", blocks[0]["type"])
	}
	if blocks[1]["type"] != "image" {
		t.Errorf("second block should be image, got %v", blocks[1]["type"])
	}
	src, _ := blocks[1]["source"].(map[string]any)
	if src == nil || src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Errorf("image source should be base64 image/png, got %v", src)
	}
	if src["data"] != "iVBORw==" { // base64 of \x89PNG
		t.Errorf("image data should be base64-encoded PNG magic, got %v", src["data"])
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func assertHasKeys(t *testing.T, what, js string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if !strings.Contains(js, `"`+k+`":`) {
			t.Errorf("%s JSON missing key %q in: %s", what, k, js)
		}
	}
}

func assertNoKeys(t *testing.T, what, js string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if strings.Contains(js, `"`+k+`":`) {
			t.Errorf("%s JSON must NOT contain key %q in: %s", what, k, js)
		}
	}
}

// jsonKeys returns the top-level object keys of a JSON object (shallow — enough to catch a
// PascalCase top-level field; nested wire types have their own tags already covered above).
func jsonKeys(js string) []string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(js), &m) != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
