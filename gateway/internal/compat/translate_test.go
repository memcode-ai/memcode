package compat

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

func mustTurn(t *testing.T, req ChatRequest) Turn {
	t.Helper()
	turn, err := ToTurn(req)
	if err != nil {
		t.Fatalf("ToTurn: %v", err)
	}
	return turn
}

func sys(s string) ChatMessage  { return ChatMessage{Role: "system", Content: StringContent(s)} }
func user(s string) ChatMessage { return ChatMessage{Role: "user", Content: StringContent(s)} }

// The two-system convention: first system message = the stable prefix, second =
// the volatile suffix, third and later concatenate into volatile. This is
// extension (1) — the cache-critical placement the gateway maps onto
// System/SystemVolatile (the CLI's doctrine layer fills both halves).
func TestSystemSplitPlacement(t *testing.T) {
	// one system message → stable only
	one := mustTurn(t, ChatRequest{Model: "auto", Messages: []ChatMessage{sys("STABLE"), user("hi")}})
	if one.System != "STABLE" || one.SystemVolatile != "" {
		t.Fatalf("one system: got system=%q volatile=%q", one.System, one.SystemVolatile)
	}
	// two → stable + volatile
	two := mustTurn(t, ChatRequest{Model: "auto", Messages: []ChatMessage{sys("STABLE"), sys("VOLATILE"), user("hi")}})
	if two.System != "STABLE" || two.SystemVolatile != "VOLATILE" {
		t.Fatalf("two systems: got system=%q volatile=%q", two.System, two.SystemVolatile)
	}
	// three+ → the tail concatenates into volatile (never into the cached prefix)
	three := mustTurn(t, ChatRequest{Model: "auto", Messages: []ChatMessage{sys("STABLE"), sys("V1"), sys("V2"), user("hi")}})
	if three.System != "STABLE" || three.SystemVolatile != "V1\n\nV2" {
		t.Fatalf("three systems: got system=%q volatile=%q", three.System, three.SystemVolatile)
	}
	// `developer` is the modern spelling of system
	dev := mustTurn(t, ChatRequest{Model: "auto", Messages: []ChatMessage{
		{Role: "developer", Content: StringContent("STABLE")}, sys("VOLATILE"), user("hi")}})
	if dev.System != "STABLE" || dev.SystemVolatile != "VOLATILE" {
		t.Fatalf("developer role: got system=%q volatile=%q", dev.System, dev.SystemVolatile)
	}
	// no non-system message → client error
	if _, err := ToTurn(ChatRequest{Model: "auto", Messages: []ChatMessage{sys("only")}}); err == nil {
		t.Fatal("system-only message list must error")
	}
	// array-form system content flattens (text parts only)
	parts := mustTurn(t, ChatRequest{Model: "auto", Messages: []ChatMessage{
		{Role: "system", Content: PartsContent(TextPart("A"), TextPart("B"))}, user("hi")}})
	if parts.System != "A\nB" {
		t.Fatalf("parts system: got %q", parts.System)
	}
	if _, err := ToTurn(ChatRequest{Model: "auto", Messages: []ChatMessage{
		{Role: "system", Content: PartsContent(ContentPart{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,AA=="}})}, user("hi")}}); err == nil {
		t.Fatal("non-text system part must error")
	}
}

// Standard messages → common blocks: text, image data URLs, file parts.
func TestUserContentTranslation(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("not-really-a-png-but-valid-b64"))
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 tiny"))
	turn := mustTurn(t, ChatRequest{Model: "auto", Messages: []ChatMessage{
		{Role: "user", Content: PartsContent(
			TextPart("look at this"),
			ContentPart{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64," + png}},
			ContentPart{Type: "file", File: &FilePart{Filename: "doc.pdf", FileData: "data:application/pdf;base64," + pdf}},
		)},
	}})
	if len(turn.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(turn.Messages))
	}
	b := turn.Messages[0].Blocks
	if len(b) != 3 {
		t.Fatalf("blocks = %d, want 3", len(b))
	}
	if b[0].Type != "text" || b[0].Text != "look at this" {
		t.Errorf("block 0 = %+v", b[0])
	}
	if b[1].Type != "image" || b[1].Source == nil || b[1].Source.MediaType != "image/png" || b[1].Source.Data != png || b[1].Source.Type != "base64" {
		t.Errorf("image block = %+v", b[1])
	}
	if b[2].Type != "document" || b[2].Source == nil || b[2].Source.MediaType != "application/pdf" || b[2].Source.Data != pdf {
		t.Errorf("document block = %+v", b[2])
	}

	// remote image URLs are refused (the gateway never fetches on the user's behalf)
	if _, err := ToTurn(ChatRequest{Model: "auto", Messages: []ChatMessage{
		{Role: "user", Content: PartsContent(ContentPart{Type: "image_url", ImageURL: &ImageURLPart{URL: "https://example.com/x.png"}})}}}); err == nil {
		t.Error("remote image_url must error")
	}
	// junk base64 is a clean 400-class error here, not a vendor error downstream
	if _, err := ToTurn(ChatRequest{Model: "auto", Messages: []ChatMessage{
		{Role: "user", Content: PartsContent(ContentPart{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,@@@"}})}}}); err == nil {
		t.Error("invalid base64 must error")
	}
	// file_id has no file store behind it
	if _, err := ToTurn(ChatRequest{Model: "auto", Messages: []ChatMessage{
		{Role: "user", Content: PartsContent(ContentPart{Type: "file", File: &FilePart{FileID: "file-123"}})}}}); err == nil {
		t.Error("file_id must error")
	}
	// empty user content
	if _, err := ToTurn(ChatRequest{Model: "auto", Messages: []ChatMessage{user("")}}); err == nil {
		t.Error("empty user message must error")
	}
}

// Tool messages fold into the internal shape: a contiguous run of role:"tool"
// results becomes ONE user message of tool_result blocks (what the adapters
// already speak), and later user text stays its own message.
func TestToolResultFolding(t *testing.T) {
	turn := mustTurn(t, ChatRequest{Model: "auto", Messages: []ChatMessage{
		user("run the tools"),
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: FunctionCall{Name: "read_file", Arguments: `{"path":"a"}`}},
			{ID: "call_2", Type: "function", Function: FunctionCall{Name: "read_file", Arguments: `{"path":"b"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: StringContent("contents A")},
		{Role: "tool", ToolCallID: "call_2", Content: StringContent("contents B")},
		user("now summarize"),
	}})
	if len(turn.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (user, assistant, folded results, user)", len(turn.Messages))
	}
	asst := turn.Messages[1]
	if asst.Role != "assistant" || len(asst.Blocks) != 2 || asst.Blocks[0].Type != "tool_use" ||
		asst.Blocks[0].ID != "call_1" || asst.Blocks[0].Name != "read_file" ||
		string(asst.Blocks[0].Input) != `{"path":"a"}` {
		t.Fatalf("assistant message = %+v", asst)
	}
	results := turn.Messages[2]
	if results.Role != "user" || len(results.Blocks) != 2 {
		t.Fatalf("folded results = %+v", results)
	}
	for i, want := range []string{"call_1", "call_2"} {
		if results.Blocks[i].Type != "tool_result" || results.Blocks[i].ToolUseID != want {
			t.Errorf("result %d = %+v", i, results.Blocks[i])
		}
	}
	if results.Blocks[0].Content != "contents A" || results.Blocks[1].Content != "contents B" {
		t.Errorf("result contents = %+v", results.Blocks)
	}
	if turn.Messages[3].Role != "user" || turn.Messages[3].Blocks[0].Text != "now summarize" {
		t.Errorf("trailing user message = %+v", turn.Messages[3])
	}
	// a tool message without tool_call_id is malformed
	if _, err := ToTurn(ChatRequest{Model: "auto", Messages: []ChatMessage{
		{Role: "tool", Content: StringContent("x")}}}); err == nil {
		t.Error("tool message without tool_call_id must error")
	}
	// empty tool-call arguments normalize to "{}" (always-parseable)
	empty := mustTurn(t, ChatRequest{Model: "auto", Messages: []ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c", Function: FunctionCall{Name: "noop"}}}}}})
	if string(empty.Messages[0].Blocks[0].Input) != "{}" {
		t.Errorf("empty arguments = %q, want {}", empty.Messages[0].Blocks[0].Input)
	}
}

// Tools + forced tool_choice — the exact shape the classifiers depend on.
func TestToolsAndToolChoice(t *testing.T) {
	tools := []Tool{{Type: "function", Function: FunctionDef{
		Name: "record_shell_risk", Description: "Record the risk verdict.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{"risk": map[string]any{"type": "string"}}},
	}}}
	base := ChatRequest{Model: "auto", Messages: []ChatMessage{user("classify: ls -la")}, Tools: tools}

	// plain tools, no choice → defs ride, model chooses
	plain := mustTurn(t, base)
	if len(plain.Tools) != 1 || plain.Tools[0].Name != "record_shell_risk" ||
		plain.Tools[0].Description != "Record the risk verdict." || plain.ToolChoice != "" {
		t.Fatalf("plain tools = %+v choice=%q", plain.Tools, plain.ToolChoice)
	}
	if ty := plain.Tools[0].InputSchema["type"]; ty != "object" {
		t.Errorf("input_schema lost: %+v", plain.Tools[0].InputSchema)
	}

	// forced named tool → ToolChoice carries the name
	forced := base
	forced.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"record_shell_risk"}}`)
	if got := mustTurn(t, forced); got.ToolChoice != "record_shell_risk" {
		t.Fatalf("forced choice = %q", got.ToolChoice)
	}

	// forcing an undefined tool is a client error, same as upstream OpenAI
	bad := base
	bad.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"nope"}}`)
	if _, err := ToTurn(bad); err == nil {
		t.Fatal("forcing an undefined tool must error")
	}

	// "none" drops the defs (the internal wire's spelling of no-tools)
	none := base
	none.ToolChoice = json.RawMessage(`"none"`)
	if got := mustTurn(t, none); got.Tools != nil || got.ToolChoice != "" {
		t.Fatalf("none: tools=%v choice=%q", got.Tools, got.ToolChoice)
	}

	// "required" with exactly one tool forces it; with several, model chooses
	req1 := base
	req1.ToolChoice = json.RawMessage(`"required"`)
	if got := mustTurn(t, req1); got.ToolChoice != "record_shell_risk" {
		t.Fatalf("required(1 tool) = %q", got.ToolChoice)
	}
	req2 := base
	req2.Tools = append([]Tool{}, tools...)
	req2.Tools = append(req2.Tools, Tool{Type: "function", Function: FunctionDef{Name: "other"}})
	req2.ToolChoice = json.RawMessage(`"required"`)
	if got := mustTurn(t, req2); got.ToolChoice != "" {
		t.Fatalf("required(2 tools) = %q, want model-chooses", got.ToolChoice)
	}

	// "auto" is the default
	auto := base
	auto.ToolChoice = json.RawMessage(`"auto"`)
	if got := mustTurn(t, auto); got.ToolChoice != "" || len(got.Tools) != 1 {
		t.Fatalf("auto: %+v", got)
	}
}

// memcode_opaque round-trip (extension 3): Anthropic thinking blocks and OpenAI
// rs_ reasoning items marshal into the opaque array off a response and
// re-expand VERBATIM into the request's assistant blocks — the whole point is
// the vendor round-trip requirement (signatures verified, rs_ ids unique).
func TestOpaqueRoundTrip(t *testing.T) {
	original := []wire.Block{
		// Anthropic thinking (signature-verified; empty thinking text must survive)
		{Type: "thinking", Thinking: "let me reason", Signature: "sig-abc"},
		{Type: "thinking", Thinking: "", Signature: "sig-only"},
		// OpenAI reasoning item (rs_ id + encrypted content in Signature)
		{Type: "thinking", ID: "rs_12345", Thinking: "openai reasoning", Signature: "enc-xyz"},
		// redacted thinking (opaque payload)
		{Type: "redacted_thinking", Data: "opaque-payload"},
		// non-reasoning blocks must NOT enter the opaque channel
		wire.TextBlock("the answer"),
		{Type: "tool_use", ID: "call_9", Name: "t", Input: json.RawMessage(`{}`)},
	}
	opq := OpaqueFrom(original)
	if len(opq) != 4 {
		t.Fatalf("opaque items = %d, want 4", len(opq))
	}
	// the wire form of a thinking block must always carry the "thinking" field
	// (Anthropic rejects a signature-only block without it)
	if !strings.Contains(string(opq[1]), `"thinking"`) {
		t.Errorf("signature-only thinking lost its thinking field: %s", opq[1])
	}
	// re-expand through an assistant message: opaque first, then text, then calls
	turn := mustTurn(t, ChatRequest{Model: "auto", Messages: []ChatMessage{
		user("go"),
		{Role: "assistant", Content: StringContent("the answer"), MemcodeOpaque: opq,
			ToolCalls: []ToolCall{{ID: "call_9", Type: "function", Function: FunctionCall{Name: "t", Arguments: "{}"}}}},
	}})
	got := turn.Messages[1].Blocks
	want := []wire.Block{
		{Type: "thinking", Thinking: "let me reason", Signature: "sig-abc"},
		{Type: "thinking", Thinking: "", Signature: "sig-only"},
		{Type: "thinking", ID: "rs_12345", Thinking: "openai reasoning", Signature: "enc-xyz"},
		{Type: "redacted_thinking", Data: "opaque-payload"},
		wire.TextBlock("the answer"),
		{Type: "tool_use", ID: "call_9", Name: "t", Input: json.RawMessage(`{}`)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	// the opaque channel is reasoning-only: smuggling other block types is refused
	fake, _ := json.Marshal(wire.Block{Type: "tool_use", ID: "x", Name: "evil", Input: json.RawMessage(`{}`)})
	if _, err := ToTurn(ChatRequest{Model: "auto", Messages: []ChatMessage{
		user("go"),
		{Role: "assistant", Content: StringContent("a"), MemcodeOpaque: []json.RawMessage{fake}},
	}}); err == nil {
		t.Fatal("non-reasoning opaque block must be refused")
	}
}

func TestEffortMapping(t *testing.T) {
	cases := map[string]wire.Effort{
		"": wire.EffortOff, "none": wire.EffortOff, "bogus": wire.EffortOff,
		"minimal": wire.EffortLow, "low": wire.EffortLow, "LOW": wire.EffortLow,
		"medium": wire.EffortMedium,
		"high":   wire.EffortHigh, "xhigh": wire.EffortHigh,
	}
	for in, want := range cases {
		if got := EffortFrom(in); got != want {
			t.Errorf("EffortFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaxTokensPrecedence(t *testing.T) {
	both := mustTurn(t, ChatRequest{Model: "auto", MaxTokens: 100, MaxCompletionTokens: 200,
		Messages: []ChatMessage{user("hi")}})
	if both.MaxTokens != 200 {
		t.Errorf("max_completion_tokens must win: got %d", both.MaxTokens)
	}
	old := mustTurn(t, ChatRequest{Model: "auto", MaxTokens: 100, Messages: []ChatMessage{user("hi")}})
	if old.MaxTokens != 100 {
		t.Errorf("legacy max_tokens must apply: got %d", old.MaxTokens)
	}
}

// Response-direction mapping: finish reasons, usage semantics conversion
// (Anthropic-style cache-exclusive → OpenAI cache-inclusive prompt_tokens),
// null-vs-text content, tool calls, and the memcode extension object.
func TestResponseFrom(t *testing.T) {
	resp := wire.Response{
		StopReason: "tool_use",
		Blocks: []wire.Block{
			{Type: "thinking", Thinking: "t", Signature: "s"},
			{Type: "tool_use", ID: "call_1", Name: "record_shell_risk", Input: json.RawMessage(`{"risk":"safe_read"}`)},
		},
		Model: "glm-5p2", Backend: "cheap",
		InputTokens: 1000, OutputTokens: 50, CacheReadTokens: 400, CacheWriteTokens: 100,
		ContextWindow: 202000, InputBudget: 150000, Pool: "fw",
		BYOK: true, FallbackReason: "cheap_lane_error: x", SearchCount: 2,
	}
	out := ResponseFrom(resp, "chatcmpl-test", 1234)
	if out.Object != "chat.completion" || out.ID != "chatcmpl-test" || out.Model != "glm-5p2" {
		t.Fatalf("envelope = %+v", out)
	}
	ch := out.Choices[0]
	if ch.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", ch.FinishReason)
	}
	if ch.Message.Content != nil {
		t.Errorf("tool-calls-only content must be null, got %q", *ch.Message.Content)
	}
	if len(ch.Message.ToolCalls) != 1 || ch.Message.ToolCalls[0].Function.Name != "record_shell_risk" ||
		ch.Message.ToolCalls[0].Function.Arguments != `{"risk":"safe_read"}` || ch.Message.ToolCalls[0].Type != "function" {
		t.Errorf("tool_calls = %+v", ch.Message.ToolCalls)
	}
	if len(ch.Message.MemcodeOpaque) != 1 {
		t.Errorf("memcode_opaque = %+v", ch.Message.MemcodeOpaque)
	}
	// usage: prompt = in + cache_read + cache_write (OpenAI semantics), cached subset in details
	if out.Usage.PromptTokens != 1500 || out.Usage.CompletionTokens != 50 || out.Usage.TotalTokens != 1550 {
		t.Errorf("usage = %+v", out.Usage)
	}
	if out.Usage.PromptTokensDetails == nil || out.Usage.PromptTokensDetails.CachedTokens != 400 {
		t.Errorf("cached tokens = %+v", out.Usage.PromptTokensDetails)
	}
	// the memcode extension object carries the footer/compaction facts
	ext := out.Memcode
	if ext == nil || !ext.Byok || ext.FallbackReason != "cheap_lane_error: x" || ext.SearchCount != 2 ||
		ext.ContextWindow != 202000 || ext.InputBudget != 150000 || ext.Pool != "fw" {
		t.Errorf("memcode ext = %+v", ext)
	}

	// text answer: content set, finish stop, no opaque
	text := ResponseFrom(wire.Response{StopReason: "end_turn",
		Blocks: []wire.Block{wire.TextBlock("hi")}, Model: "terra", OutputTokens: 1, InputTokens: 2}, "id", 1)
	if text.Choices[0].Message.Content == nil || *text.Choices[0].Message.Content != "hi" ||
		text.Choices[0].FinishReason != "stop" || text.Choices[0].Message.MemcodeOpaque != nil {
		t.Errorf("text response = %+v", text.Choices[0])
	}
	if FinishReasonFrom("max_tokens") != "length" {
		t.Error("max_tokens must map to length")
	}
}

// The content union survives both wire forms.
func TestMessageContentJSON(t *testing.T) {
	var m ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":"plain"}`), &m); err != nil || m.Content.Text != "plain" || m.Content.IsParts {
		t.Fatalf("string content: %+v err=%v", m, err)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"a"}]}`), &m); err != nil || !m.Content.IsParts || m.Content.Parts[0].Text != "a" {
		t.Fatalf("parts content: %+v err=%v", m, err)
	}
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}`), &m); err != nil || !m.Content.IsZero() {
		t.Fatalf("null content: %+v err=%v", m, err)
	}
	// marshal side: string stays a string, parts stay an array, zero content is omitted
	b, _ := json.Marshal(ChatMessage{Role: "user", Content: StringContent("x")})
	if string(b) != `{"role":"user","content":"x"}` {
		t.Errorf("string marshal = %s", b)
	}
	b, _ = json.Marshal(ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: "c", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}}}})
	if strings.Contains(string(b), `"content"`) {
		t.Errorf("zero content must be omitted: %s", b)
	}
}
