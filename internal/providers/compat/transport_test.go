package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

func sp(s string) *string { return &s }

// capture records what the scripted server saw.
type capture struct {
	mu      sync.Mutex
	bodies  []ChatRequest
	headers []http.Header
	calls   int
}

func (c *capture) record(r *http.Request) ChatRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	var body ChatRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	c.bodies = append(c.bodies, body)
	c.headers = append(c.headers, r.Header.Clone())
	return body
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newTestTransport(t *testing.T, memcode bool, h http.HandlerFunc) *Transport {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	// Model is the endpoint-mode session default; ignored (resolveModel
	// short-circuits) on the memcode backend, required for pinless requests off
	// it — endpoint-specific behavior has its own tests below.
	return New(Config{BaseURL: srv.URL, Token: "memcode_test", Memcode: memcode, Model: "local-test", HTTPClient: srv.Client()})
}

func sse(w http.ResponseWriter, payloads ...any) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, p := range payloads {
		if s, ok := p.(string); ok {
			fmt.Fprintf(w, "data: %s\n\n", s)
			continue
		}
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
}

func okCompletion(text string) ChatResponse {
	return ChatResponse{
		ID: "chatcmpl-x", Object: "chat.completion", Model: "glm-5p2",
		Choices: []Choice{{Message: ResponseMessage{Role: "assistant", Content: sp(text)}, FinishReason: "stop"}},
		Usage:   &Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
	}
}

// TestStreamAssembly is the full happy path: role/content/tool-call deltas
// (one full, one split across chunks), a reasoning (opaque) delta, the finish
// chunk, the usage chunk carrying the memcode extension, then [DONE].
func TestStreamAssembly(t *testing.T) {
	cap := &capture{}
	tr := newTestTransport(t, true, func(w http.ResponseWriter, r *http.Request) {
		body := cap.record(r)
		if !body.Stream || body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
			t.Errorf("stream request must set stream + stream_options.include_usage, got %+v", body)
		}
		fin := "tool_calls"
		final := ChatChunk{Object: "chat.completion.chunk", Model: "glm-5p2", Choices: []ChunkChoice{},
			Usage:   &Usage{PromptTokens: 1100, CompletionTokens: 50, TotalTokens: 1150, PromptTokensDetails: &PromptTokensDetails{CachedTokens: 400}},
			Memcode: &MemcodeExt{Byok: true, FallbackReason: "cheap_lane_error: x", SearchCount: 2, ContextWindow: 200000, InputBudget: 150000, Pool: "h200_256k"},
		}
		sse(w,
			ChatChunk{Object: "chat.completion.chunk", Model: "auto", Choices: []ChunkChoice{{Delta: Delta{Role: "assistant", Content: sp("")}}}},
			ChatChunk{Object: "chat.completion.chunk", Choices: []ChunkChoice{{Delta: Delta{Content: sp("Hel")}}}},
			ChatChunk{Object: "chat.completion.chunk", Choices: []ChunkChoice{{Delta: Delta{Content: sp("lo")}}}},
			ChatChunk{Object: "chat.completion.chunk", Model: "glm-5p2", Choices: []ChunkChoice{{Delta: Delta{ToolCalls: []ToolCallDelta{
				{Index: 0, ID: "call_1", Type: "function", Function: &FunctionCall{Name: "grade", Arguments: `{"ok":true}`}},
			}}}}},
			ChatChunk{Object: "chat.completion.chunk", Choices: []ChunkChoice{{Delta: Delta{ToolCalls: []ToolCallDelta{
				{Index: 1, ID: "call_2", Type: "function", Function: &FunctionCall{Name: "edit_", Arguments: `{"a":`}},
			}}}}},
			ChatChunk{Object: "chat.completion.chunk", Choices: []ChunkChoice{{Delta: Delta{ToolCalls: []ToolCallDelta{
				{Index: 1, Function: &FunctionCall{Name: "file", Arguments: `1}`}},
			}}}}},
			ChatChunk{Object: "chat.completion.chunk", Choices: []ChunkChoice{{Delta: Delta{MemcodeOpaque: []json.RawMessage{
				json.RawMessage(`{"type":"thinking","thinking":"hmm","signature":"sig1"}`),
			}}}}},
			ChatChunk{Object: "chat.completion.chunk", Choices: []ChunkChoice{{Delta: Delta{}, FinishReason: &fin}}},
			final,
			"[DONE]",
		)
	})

	var deltas []string
	var usageIn, usageOut int
	resp, err := tr.Stream(context.Background(), wire.Request{
		Pin:      "sonnet",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("go")}}},
	}, wire.StreamHandler{
		Text:  func(d string) { deltas = append(deltas, d) },
		Usage: func(in, out int) { usageIn, usageOut = in, out },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Text(); got != "Hello" {
		t.Errorf("text = %q, want Hello", got)
	}
	if !reflect.DeepEqual(deltas, []string{"Hel", "lo"}) {
		t.Errorf("streamed deltas = %v", deltas)
	}
	if len(resp.Blocks) == 0 || resp.Blocks[0].Type != "thinking" || resp.Blocks[0].Thinking != "hmm" || resp.Blocks[0].Signature != "sig1" {
		t.Errorf("reasoning block must be re-extracted FIRST, got %+v", resp.Blocks)
	}
	uses := resp.ToolUses()
	if len(uses) != 2 {
		t.Fatalf("tool uses = %d, want 2", len(uses))
	}
	if uses[0].ID != "call_1" || uses[0].Name != "grade" || string(uses[0].Input) != `{"ok":true}` {
		t.Errorf("tool call 0 wrong: %+v", uses[0])
	}
	if uses[1].ID != "call_2" || uses[1].Name != "edit_file" || string(uses[1].Input) != `{"a":1}` {
		t.Errorf("split tool-call deltas must accumulate by index: %+v", uses[1])
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop reason = %q, want tool_use", resp.StopReason)
	}
	// Usage semantics conversion: prompt_tokens is cache-INCLUSIVE on the wire.
	if resp.InputTokens != 700 || resp.CacheReadTokens != 400 || resp.OutputTokens != 50 {
		t.Errorf("usage conversion wrong: in=%d cr=%d out=%d", resp.InputTokens, resp.CacheReadTokens, resp.OutputTokens)
	}
	if usageIn != 700 || usageOut != 50 {
		t.Errorf("Usage callback got (%d,%d), want (700,50)", usageIn, usageOut)
	}
	// The memcode extension feeds the footer/compaction fields unchanged.
	if !resp.BYOK || resp.FallbackReason != "cheap_lane_error: x" || resp.SearchCount != 2 ||
		resp.ContextWindow != 200000 || resp.InputBudget != 150000 || resp.Pool != "h200_256k" {
		t.Errorf("memcode extension not decoded: %+v", resp)
	}
	if resp.Model != "glm-5p2" {
		t.Errorf("model = %q, want the last chunk's label", resp.Model)
	}
}

// TestCompleteDecodesBodyAndExtensions covers the non-streamed path, including
// the tool-calls-only null content shape and the memcode-gating of both the
// opaque re-extract and the extension object.
func TestCompleteDecodesBodyAndExtensions(t *testing.T) {
	body := ChatResponse{
		ID: "chatcmpl-y", Object: "chat.completion", Model: "sonnet",
		Choices: []Choice{{Message: ResponseMessage{
			Role: "assistant", Content: nil,
			ToolCalls:     []ToolCall{{ID: "c9", Type: "function", Function: FunctionCall{Name: "ripgrep", Arguments: ""}}},
			MemcodeOpaque: []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"t","signature":"s"}`)},
		}, FinishReason: "tool_calls"}},
		Usage:   &Usage{PromptTokens: 300, CompletionTokens: 20, TotalTokens: 320, PromptTokensDetails: &PromptTokensDetails{CachedTokens: 100}},
		Memcode: &MemcodeExt{ContextWindow: 128000, Pool: "glm-5p2"},
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	req := wire.Request{Pin: "sonnet", Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}}}

	resp, err := newTestTransport(t, true, handler).Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "" {
		t.Errorf("null content must decode to no text, got %q", resp.Text())
	}
	uses := resp.ToolUses()
	if len(uses) != 1 || uses[0].ID != "c9" || string(uses[0].Input) != "{}" {
		t.Errorf("tool call decode wrong (empty args must become {}): %+v", uses)
	}
	if len(resp.Blocks) == 0 || resp.Blocks[0].Type != "thinking" {
		t.Errorf("opaque re-extract missing: %+v", resp.Blocks)
	}
	if resp.StopReason != "tool_use" || resp.Model != "sonnet" {
		t.Errorf("stop/model wrong: %q %q", resp.StopReason, resp.Model)
	}
	if resp.InputTokens != 200 || resp.CacheReadTokens != 100 || resp.OutputTokens != 20 {
		t.Errorf("usage conversion wrong: %+v", resp)
	}
	if resp.ContextWindow != 128000 || resp.Pool != "glm-5p2" {
		t.Errorf("extension decode wrong: %+v", resp)
	}

	// Off the memcode backend both extensions are ignored/gated.
	resp, err = newTestTransport(t, false, handler).Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].Type != "tool_use" {
		t.Errorf("opaque must NOT be re-extracted off-memcode: %+v", resp.Blocks)
	}
	if resp.ContextWindow != 0 || resp.Pool != "" {
		t.Errorf("memcode extension must be ignored off-memcode: %+v", resp)
	}
}

// TestErrorMapping pins the status+code → sentinel contract (the same signals
// the legacy wire maps), including that no 4xx is ever retried.
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error  // errors.Is target; nil = substring check only
		sub    string // message substring for non-sentinel errors
	}{
		{"insufficient credits", http.StatusPaymentRequired, `{"error":{"message":"out of credits","type":"insufficient_quota","code":"insufficient_credits"}}`, wire.ErrInsufficientCredit, ""},
		{"subscription required", http.StatusPaymentRequired, `{"error":{"message":"no plan","code":"subscription_required"}}`, wire.ErrSubscriptionRequired, ""},
		{"account locked", http.StatusPaymentRequired, `{"error":{"message":"negative","code":"account_locked"}}`, wire.ErrAccountLocked, ""},
		{"byok key failed", http.StatusUnprocessableEntity, `{"error":{"message":"key rejected","code":"byok_key_failed"}}`, wire.ErrByokKeyFailed, ""},
		{"context overflow", http.StatusRequestEntityTooLarge, `{"error":{"message":"too large","code":"context_overflow"}}`, wire.ErrContextOverflow, ""},
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"bad token"}}`, wire.ErrUnauthorized, ""},
		{"unknown model", http.StatusBadRequest, `{"error":{"message":"unknown model \"gpt-nope\" — send \"auto\" or a model id from GET /openai/v1/models","type":"invalid_request_error","code":"unknown_model"}}`, nil, "unknown model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &capture{}
			tr := newTestTransport(t, true, func(w http.ResponseWriter, r *http.Request) {
				cap.record(r)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			_, err := tr.Complete(context.Background(), wire.Request{
				Pin:      "sonnet",
				Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
			})
			if err == nil {
				t.Fatal("want an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is(%v)", err, tc.want)
			}
			if tc.want == nil && !strings.Contains(err.Error(), tc.sub) {
				t.Errorf("err = %v, want substring %q", err, tc.sub)
			}
			if got := cap.count(); got != 1 {
				t.Errorf("a 4xx must never be retried: %d calls", got)
			}
		})
	}

	// The same mapping guards the stream-connect path.
	tr := newTestTransport(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		fmt.Fprint(w, `{"error":{"message":"too large","code":"context_overflow"}}`)
	})
	if _, err := tr.Stream(context.Background(), wire.Request{
		Pin:      "sonnet",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
	}, wire.StreamHandler{}); !errors.Is(err, wire.ErrContextOverflow) {
		t.Errorf("stream connect must map 413+code to ErrContextOverflow, got %v", err)
	}
}

// TestRetryPolicy: 429/5xx retry with Retry-After honored, bounded attempts,
// notify callback wired — ported semantics from the legacy SDK client.
func TestRetryPolicy(t *testing.T) {
	cap := &capture{}
	tr := newTestTransport(t, true, func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		if cap.count() <= 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okCompletion("ok"))
	})
	var notifies int
	var lastErr error
	tr.SetRetryNotify(func(attempt int, err error, delay time.Duration) { notifies++; lastErr = err })

	resp, err := tr.Complete(context.Background(), wire.Request{
		Pin:      "sonnet",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "ok" {
		t.Errorf("text = %q", resp.Text())
	}
	if cap.count() != 4 || notifies != 3 {
		t.Errorf("calls=%d notifies=%d, want 4/3", cap.count(), notifies)
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "429") {
		t.Errorf("notify cause = %v, want the 429", lastErr)
	}

	// Exhausted retries fail with the mapped error after maxRetries+1 attempts.
	cap2 := &capture{}
	tr2 := newTestTransport(t, true, func(w http.ResponseWriter, r *http.Request) {
		cap2.record(r)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"cold start","type":"server_error"}}`)
	})
	_, err = tr2.Complete(context.Background(), wire.Request{
		Pin:      "sonnet",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
	})
	if err == nil || !strings.Contains(err.Error(), "http 503") {
		t.Errorf("exhausted retries must surface the mapped error, got %v", err)
	}
	if cap2.count() != maxRetries+1 {
		t.Errorf("calls = %d, want %d", cap2.count(), maxRetries+1)
	}
	if !retryableStatus(500) || !retryableStatus(502) || !retryableStatus(504) || retryableStatus(413) || retryableStatus(400) {
		t.Error("retryable status set drifted from the legacy client's")
	}
}

// TestMidStreamError: an error envelope on a data: line maps to the same
// sentinels as HTTP statuses.
func TestMidStreamError(t *testing.T) {
	tr := newTestTransport(t, true, func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			ChatChunk{Object: "chat.completion.chunk", Choices: []ChunkChoice{{Delta: Delta{Role: "assistant", Content: sp("")}}}},
			`{"error":{"message":"key rejected mid-turn","type":"invalid_request_error","code":"byok_key_failed"}}`,
			"[DONE]",
		)
	})
	_, err := tr.Stream(context.Background(), wire.Request{
		Pin:      "sonnet",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
	}, wire.StreamHandler{})
	if !errors.Is(err, wire.ErrByokKeyFailed) {
		t.Errorf("mid-stream error must map its code, got %v", err)
	}
}

// TestStreamCutIsIncomplete: a stream ending without [DONE] is transient
// transport — the runtime retries it from the same history.
func TestStreamCutIsIncomplete(t *testing.T) {
	tr := newTestTransport(t, true, func(w http.ResponseWriter, r *http.Request) {
		sse(w, ChatChunk{Object: "chat.completion.chunk", Choices: []ChunkChoice{{Delta: Delta{Content: sp("par")}}}})
		// handler returns without [DONE] — the connection closes mid-stream
	})
	_, err := tr.Stream(context.Background(), wire.Request{
		Pin:      "sonnet",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
	}, wire.StreamHandler{})
	if !errors.Is(err, wire.ErrStreamIncomplete) {
		t.Errorf("cut stream must map to ErrStreamIncomplete, got %v", err)
	}
}

// TestRequestEncoding pins the outbound wire shape: the two-system convention,
// content parts (image/file), tool messages + image hoisting, tools + forced
// tool_choice, effort/max/user, the X-Memcode-* headers, and the
// memcode-gating of opaque attach + headers.
func TestRequestEncoding(t *testing.T) {
	req := wire.Request{
		System: "STABLE", SystemVolatile: "VOLATILE",
		Purpose: "main_loop", Mode: "chat", Difficulty: "hard", Session: "sess-1",
		RoutingHint: &wire.RoutingHint{Reason: "self_heal"},
		Pin:         "sonnet", Effort: wire.EffortHigh, MaxTokens: 4096, ToolChoice: "grade",
		Tools: []wire.ToolDef{{Name: "grade", Description: "grade it", InputSchema: map[string]any{"type": "object"}}},
		Messages: []wire.Message{
			{Role: "user", Blocks: []wire.Block{
				wire.TextBlock("hi"),
				wire.ImageBlock("image/png", []byte{1, 2}),
				wire.DocumentBlock("application/pdf", []byte{3, 4}),
			}},
			{Role: "assistant", Blocks: []wire.Block{
				{Type: "thinking", Thinking: "t", Signature: "s"},
				wire.TextBlock("doing"),
				{Type: "tool_use", ID: "c1", Name: "grade", Input: json.RawMessage(`{"x":1}`)},
			}},
			{Role: "user", Blocks: []wire.Block{
				{Type: "tool_result", ToolUseID: "c1", Content: "shot taken", IsError: false,
					ContentBlocks: []wire.Block{wire.TextBlock("shot taken"), wire.ImageBlock("image/png", []byte{9})}},
			}},
		},
	}

	run := func(memcode bool) (ChatRequest, http.Header) {
		cap := &capture{}
		tr := newTestTransport(t, memcode, func(w http.ResponseWriter, r *http.Request) {
			cap.record(r)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(okCompletion("done"))
		})
		if _, err := tr.Complete(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return cap.bodies[0], cap.headers[0]
	}

	body, hdr := run(true)
	if body.Model != "sonnet" {
		t.Errorf("model = %q, want the pin label", body.Model)
	}
	if body.ReasoningEffort != "high" || body.MaxCompletionTokens != 4096 || body.User != "sess-1" {
		t.Errorf("effort/max/user wrong: %+v", body)
	}
	if body.Stream {
		t.Error("Complete must not set stream")
	}
	m := body.Messages
	if len(m) != 6 {
		t.Fatalf("message count = %d, want 6 (2 system, user, assistant, tool, hoisted user)\n%+v", len(m), m)
	}
	if m[0].Role != "system" || m[0].Content.Text != "STABLE" || m[1].Role != "system" || m[1].Content.Text != "VOLATILE" {
		t.Errorf("two-system convention wrong: %+v %+v", m[0], m[1])
	}
	// user with parts: text + image data URL + file data URL
	if !m[2].Content.IsParts || len(m[2].Content.Parts) != 3 {
		t.Fatalf("user parts wrong: %+v", m[2])
	}
	if p := m[2].Content.Parts[1]; p.Type != "image_url" || !strings.HasPrefix(p.ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("image part wrong: %+v", p)
	}
	if p := m[2].Content.Parts[2]; p.Type != "file" || !strings.HasPrefix(p.File.FileData, "data:application/pdf;base64,") {
		t.Errorf("file part wrong: %+v", p)
	}
	// assistant: opaque attached (memcode on), text content, tool_calls
	if len(m[3].MemcodeOpaque) != 1 || !strings.Contains(string(m[3].MemcodeOpaque[0]), `"thinking":"t"`) {
		t.Errorf("opaque attach wrong: %s", m[3].MemcodeOpaque)
	}
	if m[3].Content.Text != "doing" || len(m[3].ToolCalls) != 1 || m[3].ToolCalls[0].ID != "c1" ||
		m[3].ToolCalls[0].Function.Name != "grade" || m[3].ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Errorf("assistant encode wrong: %+v", m[3])
	}
	// tool result → tool message (text), images hoisted into a user message
	if m[4].Role != "tool" || m[4].ToolCallID != "c1" || m[4].Content.Text != "shot taken" {
		t.Errorf("tool message wrong: %+v", m[4])
	}
	if m[5].Role != "user" || !m[5].Content.IsParts || len(m[5].Content.Parts) != 2 ||
		m[5].Content.Parts[0].Type != "text" || !strings.Contains(m[5].Content.Parts[0].Text, "c1") ||
		m[5].Content.Parts[1].Type != "image_url" {
		t.Errorf("hoisted tool-result image wrong: %+v", m[5])
	}
	// tools + forced tool_choice
	if len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Function.Name != "grade" ||
		body.Tools[0].Function.Parameters["type"] != "object" {
		t.Errorf("tool defs wrong: %+v", body.Tools)
	}
	var forced struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(body.ToolChoice, &forced); err != nil || forced.Type != "function" || forced.Function.Name != "grade" {
		t.Errorf("forced tool_choice wrong: %s (%v)", body.ToolChoice, err)
	}
	// Uniformity: NO memcode headers on any backend — the request the CLI
	// sends the gateway is byte-shape-identical to the one it sends Ollama
	// (base URL, key, and the model id aside). Auth rides the standard header.
	if got := hdr.Get("Authorization"); got != "Bearer memcode_test" {
		t.Errorf("Authorization = %q", got)
	}
	for k := range hdr {
		if strings.HasPrefix(strings.ToLower(k), "x-memcode") {
			t.Errorf("memcode header %s must not exist (uniformity)", k)
		}
	}

	// Off-memcode: no opaque either — pure standard wire.
	body, hdr = run(false)
	for _, msg := range body.Messages {
		if len(msg.MemcodeOpaque) != 0 {
			t.Errorf("opaque must not attach off-memcode: %+v", msg)
		}
	}
	for k := range hdr {
		if strings.HasPrefix(strings.ToLower(k), "x-memcode") {
			t.Errorf("memcode header %s must not exist off-memcode", k)
		}
	}
}

// TestEndpointModeWire pins the arbitrary-endpoint contract (one-wire Phase
// C): pure standard wire — no Authorization header without a key (and a plain
// Bearer with one), no X-Memcode-* headers ever, the configured session model
// substituted for pinless requests (a pin wins), and no model at all is a
// LOCAL, actionable error — the gateway sentinel "auto" never reaches an
// endpoint.
func TestEndpointModeWire(t *testing.T) {
	newEndpoint := func(t *testing.T, key, model string, cap *capture) *Transport {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cap.record(r)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(okCompletion("pong"))
		}))
		t.Cleanup(srv.Close)
		return New(Config{BaseURL: srv.URL, Token: key, Memcode: false, Model: model, HTTPClient: srv.Client()})
	}
	req := wire.Request{Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}}}

	t.Run("keyless sends no auth and the default model", func(t *testing.T) {
		cap := &capture{}
		resp, err := newEndpoint(t, "", "qwen3:4b", cap).Complete(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got := cap.headers[0].Get("Authorization"); got != "" {
			t.Errorf("keyless endpoint must send NO Authorization header, got %q", got)
		}
		for _, k := range []string{"X-Memcode-Purpose", "X-Memcode-Mode", "X-Memcode-Risk", "X-Memcode-Difficulty"} {
			if cap.headers[0].Get(k) != "" {
				t.Errorf("header %s must never reach an endpoint", k)
			}
		}
		if cap.bodies[0].Model != "qwen3:4b" {
			t.Errorf("pinless request must carry the endpoint session model, got %q", cap.bodies[0].Model)
		}
		if resp.Backend != "endpoint" {
			t.Errorf("endpoint responses must be tagged backend=endpoint (honest /cost attribution), got %q", resp.Backend)
		}
		if resp.BYOK {
			t.Error("BYOK must never be set off the memcode backend (footer byok segment)")
		}
	})

	t.Run("key rides as a plain bearer", func(t *testing.T) {
		cap := &capture{}
		r := req
		r.Pin = "mistral:latest" // a /model pick overrides the boot default
		if _, err := newEndpoint(t, "sk-local", "qwen3:4b", cap).Complete(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		if got := cap.headers[0].Get("Authorization"); got != "Bearer sk-local" {
			t.Errorf("Authorization = %q, want the configured key", got)
		}
		if cap.bodies[0].Model != "mistral:latest" {
			t.Errorf("the pin must win over the endpoint default, got %q", cap.bodies[0].Model)
		}
	})

	t.Run("no model anywhere fails locally", func(t *testing.T) {
		cap := &capture{}
		_, err := newEndpoint(t, "", "", cap).Complete(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "/model") {
			t.Fatalf("want the actionable no-model error, got %v", err)
		}
		if cap.count() != 0 {
			t.Error("the no-model error must fail before any network request")
		}
		if _, err := newEndpoint(t, "", "", cap).Stream(context.Background(), req, wire.StreamHandler{}); err == nil ||
			!strings.Contains(err.Error(), "/model") {
			t.Fatalf("stream must fail the same way, got %v", err)
		}
	})

	t.Run("stream tags the endpoint backend", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sse(w,
				ChatChunk{Object: "chat.completion.chunk", Model: "qwen3:4b", Choices: []ChunkChoice{{Delta: Delta{Content: sp("ok")}}}},
				"[DONE]",
			)
		}))
		t.Cleanup(srv.Close)
		tr := New(Config{BaseURL: srv.URL, Memcode: false, Model: "qwen3:4b", HTTPClient: srv.Client()})
		resp, err := tr.Stream(context.Background(), req, wire.StreamHandler{})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Backend != "endpoint" || resp.Model != "qwen3:4b" {
			t.Errorf("streamed endpoint response tagged wrong: backend=%q model=%q", resp.Backend, resp.Model)
		}
	})
}

// The Compose hook composes legacy-shaped side calls (Mode stamped, no
// composed system) before encoding — the CALLER owns doctrine; the engine
// only guarantees the hook runs exactly when needed and its output rides the
// two-system convention. (The real-doctrine integration test lives with the
// CLI, which owns the doctrine renderer.)
func TestComposeHookRuns(t *testing.T) {
	cap := &capture{}
	tr := newTestTransport(t, true, func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(okCompletion("ok"))
	})
	tr.compose = func(r wire.Request) (wire.Request, error) {
		r.System, r.SystemVolatile, r.Facts = "STABLE-"+r.Mode, "[volatile]", nil
		return r, nil
	}
	if _, err := tr.Complete(context.Background(), wire.Request{
		Mode: "compact", Pin: "glm-5p2",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
	}); err != nil {
		t.Fatal(err)
	}
	m := cap.bodies[0].Messages
	if len(m) != 3 || m[0].Content.Text != "STABLE-compact" || m[1].Content.Text != "[volatile]" {
		t.Fatalf("hook output must ride the two-system convention: %+v", m)
	}
	// Already-composed requests bypass the hook.
	tr.compose = func(r wire.Request) (wire.Request, error) {
		t.Fatal("hook must not run for composed requests")
		return r, nil
	}
	if _, err := tr.Complete(context.Background(), wire.Request{
		Pin: "glm-5p2", System: "S", SystemVolatile: "V",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
	}); err != nil {
		t.Fatal(err)
	}
}
