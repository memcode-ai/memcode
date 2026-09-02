package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/memcode-ai/memcode/internal/wire"
)

const sampleSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"read_file"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"x.go\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}
`

// TestStreamSeedsThinkingFromStartEvent is the regression for the 502 "thinking blocks
// in the latest assistant message cannot be modified": Anthropic can deliver initial
// thinking text AND/OR the signature in the content_block_start event (adaptive thinking
// with display:omitted returns a signature-only start block with no later signature_delta).
// The stream parser previously discarded both fields and rebuilt solely from deltas,
// corrupting the round-trip — the next tool-use turn replayed a thinking block with
// empty/truncated fields, which Anthropic rejected. This proves the start-event seed survives.
func TestStreamSeedsThinkingFromStartEvent(t *testing.T) {
	// A thinking block whose signature arrives ONLY in the start event (no signature_delta),
	// with initial thinking text also in the start event, plus a thinking_delta appending more.
	const sse = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"Let me think","signature":"sig_start_abc"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" about this"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"done"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer srv.Close()

	c := NewAnthropic("test-key")
	c.baseURL = srv.URL

	resp, err := c.Stream(context.Background(), wire.Request{Model: "m"}, wire.StreamHandler{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var thinking *wire.Block
	for i := range resp.Blocks {
		if resp.Blocks[i].Type == "thinking" {
			thinking = &resp.Blocks[i]
			break
		}
	}
	if thinking == nil {
		t.Fatalf("no thinking block in response: %+v", resp.Blocks)
	}
	// The start-event thinking text ("Let me think") must survive, with the delta appended.
	if want := "Let me think about this"; thinking.Thinking != want {
		t.Errorf("thinking text = %q, want %q (start-event seed + delta)", thinking.Thinking, want)
	}
	// The start-event signature must survive — there was no signature_delta to "fix" it.
	if thinking.Signature != "sig_start_abc" {
		t.Errorf("signature = %q, want %q (from start event, no delta)", thinking.Signature, "sig_start_abc")
	}
}

func TestWireToParamsDoesNotCacheDecorateThinking(t *testing.T) {
	params := wireToParams(buildWire(wire.Request{
		Model: "claude-sonnet-5",
		Messages: []wire.Message{
			{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "Use a tool."}}},
			{Role: "assistant", Blocks: []wire.Block{
				{Type: "thinking", Thinking: "private reasoning", Signature: "sig_123"},
				{Type: "tool_use", ID: "toolu_123", Name: "read_file", Input: []byte(`{"path":"x.go"}`)},
			}},
			{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: "toolu_123", Content: "contents"}}},
			{Role: "assistant", Blocks: []wire.Block{
				{Type: "thinking", Thinking: "more reasoning", Signature: "sig_456"},
				{Type: "text", Text: "done"},
			}},
		},
	}, 4096, true, ""))

	if got := len(params.Messages); got != 4 {
		t.Fatalf("messages = %d, want 4", got)
	}
	assistant := params.Messages[1]
	if got := len(assistant.Content); got != 2 {
		t.Fatalf("assistant content = %d, want 2", got)
	}
	thinking := assistant.Content[0].OfThinking
	if thinking == nil {
		t.Fatalf("first assistant block = %+v, want ThinkingBlockParam", assistant.Content[0])
	}
	if thinking.Thinking != "private reasoning" || thinking.Signature != "sig_123" {
		t.Errorf("thinking = %+v, want verbatim thinking and signature", thinking)
	}
	// The assistant tool_use is NOT the last block of the LAST message, so it does not
	// carry a cache breakpoint — but crucially it also has not been corrupted by stripping
	// its cache control or content. The fix is about not mutating previous assistant blocks
	// (especially thinking blocks), not about forcing breakpoints on every block.
	tool := assistant.Content[1].OfToolUse
	if tool == nil {
		t.Fatalf("second assistant block = %+v, want ToolUseBlockParam", assistant.Content[1])
	}
	if tool.ID != "toolu_123" || tool.Name != "read_file" {
		t.Errorf("tool_use = %+v, want intact tool_use", tool)
	}

	lastAssistant := params.Messages[3]
	if got := len(lastAssistant.Content); got != 2 {
		t.Fatalf("last assistant content = %d, want 2", got)
	}
	lastThinking := lastAssistant.Content[0].OfThinking
	if lastThinking == nil {
		t.Fatalf("last thinking block = %+v, want ThinkingBlockParam", lastAssistant.Content[0])
	}
	lastText := lastAssistant.Content[1].OfText
	if lastText == nil {
		t.Fatalf("last text block = %+v, want TextBlockParam", lastAssistant.Content[1])
	}
	if param.IsOmitted(lastText.CacheControl) {
		t.Errorf("last assistant text cache_control is omitted, want ephemeral breakpoint")
	}
}

func TestStreamParsesTextToolAndUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sampleSSE))
	}))
	defer srv.Close()

	c := NewAnthropic("test-key")
	c.baseURL = srv.URL

	var deltas []string
	var lastOut int
	resp, err := c.Stream(context.Background(), wire.Request{Model: "m"}, wire.StreamHandler{
		Text:  func(d string) { deltas = append(deltas, d) },
		Usage: func(in, out int) { lastOut = out },
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Text deltas streamed in order.
	if got := strings.Join(deltas, ""); got != "Hello world" {
		t.Errorf("deltas = %q", got)
	}
	// Assembled response: text + tool_use with reconstructed JSON input.
	if resp.Text() != "Hello world" {
		t.Errorf("Text() = %q", resp.Text())
	}
	uses := resp.ToolUses()
	if len(uses) != 1 || uses[0].Name != "read_file" || string(uses[0].Input) != `{"path":"x.go"}` {
		t.Fatalf("tool_use = %+v", uses)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop = %q", resp.StopReason)
	}
	// Usage: input from message_start, authoritative output from message_delta.
	if resp.InputTokens != 12 || resp.OutputTokens != 42 || lastOut != 42 {
		t.Errorf("usage in=%d out=%d lastOut=%d", resp.InputTokens, resp.OutputTokens, lastOut)
	}
}
