package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/providers/provcore"
	"github.com/memcode-ai/memcode/internal/wire"
)

// sampleOpenAISSE is a Responses API SSE stream: a text delta, a function-call
// arguments delta, an output_item.added (carrying the function call's id+name),
// and a response.completed (carrying usage). The stream union is a FLATTENED
// struct — events carry a `type` discriminator and the fields inline.
const sampleOpenAISSE = `event: response.created
data: {"type":"response.created"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[],"status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" world"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","status":"completed"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"","status":"in_progress"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"path\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"\"x.go\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"x.go\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.6-terra","output":[],"usage":{"input_tokens":12,"output_tokens":42,"total_tokens":54,"input_tokens_details":{"cached_tokens":5}}}}

`

func TestOpenAIStreamParsesTextToolAndUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The OpenAI SDK sends the key as a Bearer header.
		if r.Header.Get("authorization") != "Bearer sk-test" {
			t.Errorf("missing/wrong auth header: %q", r.Header.Get("authorization"))
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sampleOpenAISSE))
	}))
	defer srv.Close()

	c := NewOpenAI("sk-test")
	c.baseURL = srv.URL

	var deltas []string
	var lastOut int
	resp, err := c.Stream(context.Background(), wire.Request{
		Model: catalog.ModelTerra,
		Tools: []wire.ToolDef{{Name: "read_file", Description: "read", InputSchema: map[string]any{"type": "object"}}},
	}, wire.StreamHandler{
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
	if uses[0].ID != "call_1" {
		t.Errorf("tool_use ID = %q, want call_1", uses[0].ID)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop = %q", resp.StopReason)
	}
	// Usage: input from response.completed (12, with 5 cached → 7 uncached), output 42.
	if resp.InputTokens != 7 || resp.OutputTokens != 42 || resp.CacheReadTokens != 5 {
		t.Errorf("usage in=%d out=%d cacheRead=%d, want 7/42/5", resp.InputTokens, resp.OutputTokens, resp.CacheReadTokens)
	}
	if lastOut != 42 {
		t.Errorf("lastOut = %d, want 42", lastOut)
	}
	// Backend stamped as openai.
	if resp.Backend != "openai" {
		t.Errorf("Backend = %q, want openai", resp.Backend)
	}
	// Tool origin tagged as structured (the intended path).
	if resp.ToolOrigin != "structured_openai" {
		t.Errorf("ToolOrigin = %q, want structured_openai", resp.ToolOrigin)
	}
}

// A reasoning item that emits encrypted_content but NO reasoning_text.delta (the
// normal GPT-5.x case) must still round-trip: the assembled thinking block carries
// the encrypted content as its signature AND the REAL item id (rs_…), not a dropped
// item or a shared "rs_" placeholder. This is the fatal Responses-API round-trip bug.
const reasoningNoSummarySSE = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_real123","summary":[]}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_real123","encrypted_content":"ENCRYPTED_BLOB","summary":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_9","name":"read_file","arguments":"","status":"in_progress"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_9","output_index":1,"delta":"{}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_9","name":"read_file","arguments":"{}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_2","model":"gpt-5.6-terra","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":0}}}}

`

func TestOpenAIReasoningRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(reasoningNoSummarySSE))
	}))
	defer srv.Close()
	c := NewOpenAI("sk-test")
	c.baseURL = srv.URL

	resp, err := c.Stream(context.Background(), wire.Request{
		Model: catalog.ModelTerra,
		Tools: []wire.ToolDef{{Name: "read_file", InputSchema: map[string]any{"type": "object"}}},
	}, wire.StreamHandler{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// The reasoning item survived assembly despite having no summary text.
	var think *wire.Block
	for i := range resp.Blocks {
		if resp.Blocks[i].Type == "thinking" {
			think = &resp.Blocks[i]
		}
	}
	if think == nil {
		t.Fatal("reasoning item dropped — no thinking block (encrypted content lost)")
	}
	if think.Signature != "ENCRYPTED_BLOB" {
		t.Errorf("thinking.Signature = %q, want the encrypted content", think.Signature)
	}
	if think.ID != "rs_real123" {
		t.Errorf("thinking.ID = %q, want the real item id rs_real123", think.ID)
	}
	// Reasoning must come BEFORE the tool_use in block order (API round-trip contract).
	if len(resp.Blocks) < 2 || resp.Blocks[0].Type != "thinking" {
		t.Errorf("blocks = %+v, want reasoning first", resp.Blocks)
	}

	// Round-trip: feeding the assembled thinking block back builds a reasoning input
	// item carrying the REAL id (not a shared "rs_" placeholder that would collide).
	items := c.assistantItems([]wire.Block{*think})
	if len(items) != 1 || items[0].OfReasoning == nil {
		t.Fatalf("assistantItems = %+v, want one reasoning item", items)
	}
	if items[0].OfReasoning.ID != "rs_real123" {
		t.Errorf("round-trip reasoning id = %q, want rs_real123", items[0].OfReasoning.ID)
	}
}

func TestOpenAIStreamParsesTextOnly(t *testing.T) {
	sse := `event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"just text"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.6-terra","output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8,"input_tokens_details":{"cached_tokens":0}}}}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer srv.Close()

	c := NewOpenAI("sk-test")
	c.baseURL = srv.URL
	resp, err := c.Stream(context.Background(), wire.Request{Model: catalog.ModelTerra}, wire.StreamHandler{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Text() != "just text" {
		t.Errorf("Text() = %q", resp.Text())
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop = %q, want end_turn", resp.StopReason)
	}
	if len(resp.ToolUses()) != 0 {
		t.Errorf("expected no tool uses, got %+v", resp.ToolUses())
	}
}

func TestOpenAIReasoningEffortMapping(t *testing.T) {
	cases := []struct {
		name string
		eff  wire.Effort
		want string
	}{
		{"off → none", wire.EffortOff, "none"},
		{"low → low", wire.EffortLow, "low"},
		{"medium → high", wire.EffortMedium, "high"},
		{"high → xhigh", wire.EffortHigh, "xhigh"},
	}
	for _, c := range cases {
		got := string(mapEffort(c.eff))
		if got != c.want {
			t.Errorf("%s: mapEffort(%v) = %q, want %q", c.name, c.eff, got, c.want)
		}
	}
}

func TestOpenAIWireShape(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the request body to inspect the Responses API params.
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("content-type", "text/event-stream")
		// Minimal completed event so the stream closes cleanly.
		w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"gpt-5.6-terra\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n"))
	}))
	defer srv.Close()

	c := NewOpenAI("sk-test")
	c.baseURL = srv.URL
	_, _ = c.Stream(context.Background(), wire.Request{
		Model:          catalog.ModelTerra,
		System:         "be terse",
		SystemVolatile: "VOLATILE",
		Messages: []wire.Message{
			{Role: "user", Blocks: []wire.Block{wire.TextBlock("hello")}},
		},
		Tools:     []wire.ToolDef{{Name: "read_file", Description: "read", InputSchema: map[string]any{"type": "object"}}},
		MaxTokens: 512,
		Effort:    wire.EffortHigh,
	}, wire.StreamHandler{})

	// The instructions field should carry the system prompt (stable + volatile).
	if !strings.Contains(gotBody, "be terse") || !strings.Contains(gotBody, "VOLATILE") {
		t.Errorf("instructions missing system/volatile: %s", gotBody)
	}
	// store=false (stateless).
	if !strings.Contains(gotBody, `"store":false`) {
		t.Errorf("store should be false: %s", gotBody)
	}
	// max_output_tokens.
	if !strings.Contains(gotBody, `"max_output_tokens":512`) {
		t.Errorf("max_output_tokens missing: %s", gotBody)
	}
	// reasoning effort xhigh (EffortHigh → xhigh).
	if !strings.Contains(gotBody, `"effort":"xhigh"`) {
		t.Errorf("reasoning effort xhigh missing: %s", gotBody)
	}
	// The tool definition (flat — name top-level, not nested under "function").
	if !strings.Contains(gotBody, `"name":"read_file"`) {
		t.Errorf("tool name missing: %s", gotBody)
	}
	// Include encrypted reasoning content for round-trip.
	if !strings.Contains(gotBody, "reasoning.encrypted_content") {
		t.Errorf("include reasoning.encrypted_content missing: %s", gotBody)
	}
}

func TestOpenAIContextOverflow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"message\":\"This request exceeds the maximum context length.\"}\n\n"))
	}))
	defer srv.Close()

	c := NewOpenAI("sk-test")
	c.baseURL = srv.URL
	_, err := c.Stream(context.Background(), wire.Request{Model: catalog.ModelTerra}, wire.StreamHandler{})
	if err == nil {
		t.Fatal("expected an error")
	}
	var co *provcore.ContextOverflowError
	if !errors.As(err, &co) {
		t.Fatalf("expected ContextOverflowError, got %v", err)
	}
}

// sampleWebSearchSSE is a Responses stream for a native-search turn: two
// web_search_call items (each added AND done — the fee counter must count each
// search ONCE, on done) interleaved with the searched-in text answer.
const sampleWebSearchSSE = `event: response.created
data: {"type":"response.created"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"web_search_call","id":"ws_2","status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"type":"web_search_call","id":"ws_2","status":"completed"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":2,"content_index":0,"delta":"answer"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_ws","model":"gpt-5.6-terra","output":[],"usage":{"input_tokens":20,"output_tokens":9,"total_tokens":29,"input_tokens_details":{"cached_tokens":0}}}}

`

// A native-search serving turn must count its web_search_call items — each one
// bills a per-request fee upstream ($10/1k for OpenAI) that tokens alone
// under-metered (SearchFeeUSD prices SearchCount at emitUsage).
func TestOpenAIStreamCountsWebSearchCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sampleWebSearchSSE))
	}))
	defer srv.Close()

	c := NewOpenAI("sk-test")
	c.baseURL = srv.URL
	resp, err := c.Stream(context.Background(), wire.Request{
		Model: catalog.ModelTerra,
		Tools: []wire.ToolDef{{Name: "web_search", InputSchema: map[string]any{"type": "object"}}},
	}, wire.StreamHandler{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.SearchCount != 2 {
		t.Errorf("SearchCount = %d, want 2 (one per DONE web_search_call, not per added+done)", resp.SearchCount)
	}
	if resp.Text() != "answer" {
		t.Errorf("Text() = %q", resp.Text())
	}
}

// The /v1/websearch side channel (non-streamed Responses call) must count its
// web_search_call output items the same way — the side channel's searches carry
// the same per-request fee as an in-turn search.
func TestOpenAIWebSearchSideChannelCountsSearches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"resp_1","model":"gpt-5.6-terra","status":"completed","output":[
			{"type":"web_search_call","id":"ws_1","status":"completed"},
			{"type":"web_search_call","id":"ws_2","status":"completed"},
			{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"searched answer","annotations":[]}]}
		],"usage":{"input_tokens":30,"output_tokens":12,"total_tokens":42,"input_tokens_details":{"cached_tokens":0}}}`))
	}))
	defer srv.Close()

	c := NewOpenAI("sk-test")
	c.baseURL = srv.URL
	text, usage, err := c.WebSearch(context.Background(), "latest news")
	if err != nil {
		t.Fatalf("WebSearch: %v", err)
	}
	if text != "searched answer" {
		t.Errorf("text = %q", text)
	}
	if usage.InputTokens != 30 || usage.OutputTokens != 12 {
		t.Errorf("usage tokens = %d/%d, want 30/12", usage.InputTokens, usage.OutputTokens)
	}
	if usage.SearchCount != 2 {
		t.Errorf("usage.SearchCount = %d, want 2 (web_search_call output items)", usage.SearchCount)
	}
	if usage.Backend != "openai" {
		t.Errorf("Backend = %q, want openai", usage.Backend)
	}
}
