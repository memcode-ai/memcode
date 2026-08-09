package server

// compat_test.go — the turn surface (POST /v1/chat/completions) as a METERED
// SERVING ENDPOINT (all-policy-client-side): strict label-only model gate
// (unknown_model for "auto"/vendor ids/typos), exact-label serving, the
// two-system split + SystemPrefix placement, billing-lane validation, opaque
// round-trip through the endpoint, SSE chunk shape, and the MONEY CANARY:
// absolute assertions that a turn reports the served model with
// the right usage shape (and that a keyed-serve turn carries the byok
// flags). Routing/steering tests died with server-side routing — the CLI owns
// selection now (cli/internal/llm, proven against parity goldens).

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/gateway/internal/compat"
	"github.com/memcode-ai/memcode/gateway/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// compatProvider records the request it served and returns a configurable
// canned response (Model defaults to the requested id). Stream emits each text
// block as its own delta.
type compatProvider struct {
	mu   sync.Mutex
	last wire.Request
	resp wire.Response
}

func (p *compatProvider) lastReq() wire.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func (p *compatProvider) canned(r wire.Request) wire.Response {
	p.mu.Lock()
	p.last = r
	out := p.resp
	p.mu.Unlock()
	if out.Model == "" {
		out.Model = r.Model
	}
	if out.StopReason == "" {
		out.StopReason = "end_turn"
		out.Blocks = []wire.Block{wire.TextBlock("ok")}
		out.Backend = "anthropic"
		out.InputTokens, out.OutputTokens = 7, 3
	}
	return out
}

func (p *compatProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	return p.canned(r), nil
}

func (p *compatProvider) Stream(_ context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	out := p.canned(r)
	if h.Text != nil {
		for _, b := range out.Blocks {
			if b.Type == "text" && b.Text != "" {
				h.Text(b.Text)
			}
		}
	}
	if h.Usage != nil {
		h.Usage(out.InputTokens, out.OutputTokens)
	}
	return out, nil
}

// servingEnv pins the credential env the label gate (LookupServable →
// backendConfigured) reads, so tests are deterministic regardless of the
// developer's ambient shell.
func servingEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MEMCODE_PROVIDER", "hybrid")
	t.Setenv("OPENAI_API_KEY", "sk-oa")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	t.Setenv("XAI_API_KEY", "sk-grok")
	t.Setenv("MEMCODE_FIREWORKS_URL", "https://fw.example/v1")
	t.Setenv("GEMINI_API_KEY", "sk-gem")
}

func newCompatServer(t *testing.T, prov provider.ModelProvider, prefix string) *httptest.Server {
	t.Helper()
	servingEnv(t)
	srv := httptest.NewServer(newCoreHandler(Config{
		SystemPrefix: prefix,
		Provider:     prov,
		BackendName:  "test",
	}))
	t.Cleanup(srv.Close)
	return srv
}

// postCompat sends one chat-completions request and returns the raw response.
func postCompat(t *testing.T, srv *httptest.Server, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sekrit")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func compatBody(model, text string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, model, text)
}

// The strict model gate: unknown ids, catalog-but-unpinnable labels, and the
// empty model all 400 with code unknown_model. There is no server-side
// Automatic and no vendor logical ids: the agent names a concrete label or the
// gateway declines.
func TestCompatUnknownModel400(t *testing.T) {
	fp := &compatProvider{}
	srv := newCompatServer(t, fp, "")
	for _, model := range []string{"gpt-4o", "claude-sonnet-5" /* raw id, not a label */, "auto", "openai", "anthropic", "gemini-embedding-001", ""} {
		resp, raw := postCompat(t, srv, compatBody(model, "hi"), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("model %q: status = %d, want 400 (%s)", model, resp.StatusCode, raw)
		}
		var e compat.ErrorResponse
		if err := json.Unmarshal(raw, &e); err != nil || e.Error.Code != "unknown_model" {
			t.Fatalf("model %q: body = %s (err %v), want error.code unknown_model", model, raw, err)
		}
	}
	// and the gate must not have let anything reach the provider
	if fp.lastReq().Model != "" {
		t.Fatalf("provider was called for an unknown model: %+v", fp.lastReq())
	}
}

// A label serves EXACTLY that model: raw id internally, label on the wire.
// Non-pinnable chat labels (the classify lane) serve too — pinnable is a
// picker fact, not a serving gate.
func TestCompatServesExactLabel(t *testing.T) {
	fp := &compatProvider{}
	srv := newCompatServer(t, fp, "")

	resp, raw := postCompat(t, srv, compatBody("sonnet", "hi"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sonnet: %d %s", resp.StatusCode, raw)
	}
	if got := fp.lastReq(); got.Model != catalog.ModelSonnet {
		t.Fatalf("label must serve the exact catalog model: %+v", got.Model)
	}
	var out compat.ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "sonnet" || out.Object != "chat.completion" {
		t.Fatalf("wire model must be the sanitized label: %+v", out)
	}
	if out.Choices[0].Message.Content == nil || *out.Choices[0].Message.Content != "ok" {
		t.Fatalf("content = %+v", out.Choices[0].Message)
	}
	if out.Usage == nil || out.Usage.PromptTokens != 7 || out.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	if out.Memcode == nil {
		t.Fatal("the memcode extension object must ride the final body")
	}

	// A non-pinnable servable label (the CLI's classify lane asks for it directly).
	resp, raw = postCompat(t, srv, compatBody("gpt-oss-120b", "hi"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gpt-oss-120b: %d %s", resp.StatusCode, raw)
	}
	if got := fp.lastReq(); got.Model != "accounts/fireworks/models/gpt-oss-120b" {
		t.Fatalf("non-pinnable servable label must serve: %+v", got.Model)
	}
}

// The billing-lane extension is validated and threaded through; an unknown
// value 400s; the retired X-Memcode routing header is INERT (ignored like any
// unknown header — never an error, never an effect).
func TestCompatBillingLaneAndInertHeader(t *testing.T) {
	fp := &compatProvider{}
	srv := newCompatServer(t, fp, "")

	// Valid lanes pass through to the request (enforced in byokroute.go).
	body := `{"model":"sonnet","memcode_billing":"credits","messages":[{"role":"user","content":"hi"}]}`
	resp, raw := postCompat(t, srv, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("credits lane: %d %s", resp.StatusCode, raw)
	}
	if got := fp.lastReq(); got.BillingLane != "credits" {
		t.Fatalf("BillingLane = %q, want credits", got.BillingLane)
	}

	// Unknown lane → 400.
	body = `{"model":"sonnet","memcode_billing":"whatever","messages":[{"role":"user","content":"hi"}]}`
	resp, _ = postCompat(t, srv, body, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bogus lane: %d, want 400", resp.StatusCode)
	}

	// The retired routing header changes nothing.
	resp, _ = postCompat(t, srv, compatBody("sonnet", "hi"), map[string]string{"X-Memcode": "purpose=review; risk=self_heal"})
	if resp.StatusCode != http.StatusOK {
		t.Fatal("the retired header must be ignored, not an error")
	}
	if got := fp.lastReq(); got.Model != catalog.ModelSonnet {
		t.Fatalf("the retired header must not affect serving: %+v", got.Model)
	}
}

// The two-system convention lands on System/SystemVolatile, with the global
// SystemPrefix prepended to the STABLE half; the standard `user` field carries
// session/cache affinity.
func TestCompatSystemSplitAndSession(t *testing.T) {
	fp := &compatProvider{}
	srv := newCompatServer(t, fp, "DOCTRINE")
	body := `{"model":"sonnet","user":"sess-42","messages":[
		{"role":"system","content":"STABLE"},
		{"role":"system","content":"VOLATILE-A"},
		{"role":"system","content":"VOLATILE-B"},
		{"role":"user","content":"hi"}]}`
	resp, raw := postCompat(t, srv, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	got := fp.lastReq()
	if got.System != "DOCTRINE\n\nSTABLE" {
		t.Errorf("stable half = %q — SystemPrefix must prepend the first system message", got.System)
	}
	if got.SystemVolatile != "VOLATILE-A\n\nVOLATILE-B" {
		t.Errorf("volatile half = %q", got.SystemVolatile)
	}
	if got.Session != "sess-42" {
		t.Errorf("session = %q — the standard user field must carry cache affinity", got.Session)
	}
}

// Images and file parts reach the provider as vision/document blocks; remote
// image URLs are refused as a clean 400.
func TestCompatImageAndFileParts(t *testing.T) {
	fp := &compatProvider{}
	srv := newCompatServer(t, fp, "")
	png := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4"))
	body := fmt.Sprintf(`{"model":"sonnet","messages":[{"role":"user","content":[
		{"type":"text","text":"see"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}},
		{"type":"file","file":{"filename":"d.pdf","file_data":"data:application/pdf;base64,%s"}}]}]}`, png, pdf)
	resp, raw := postCompat(t, srv, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	blocks := fp.lastReq().Messages[0].Blocks
	if len(blocks) != 3 || blocks[1].Type != "image" || blocks[1].Source.MediaType != "image/png" ||
		blocks[2].Type != "document" || blocks[2].Source.MediaType != "application/pdf" {
		t.Fatalf("blocks = %+v", blocks)
	}
	resp, raw = postCompat(t, srv, `{"model":"sonnet","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}]}`, nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "data:") {
		t.Fatalf("remote image URL: %d %s, want 400 naming data: URLs", resp.StatusCode, raw)
	}
}

// Tools + forced tool_choice flow through to the internal wire (the classifier
// contract), and a tool-call answer comes back in the standard shape.
func TestCompatToolsEndToEnd(t *testing.T) {
	fp := &compatProvider{resp: wire.Response{
		StopReason: "tool_use",
		Blocks: []wire.Block{
			{Type: "tool_use", ID: "call_1", Name: "record_shell_risk", Input: json.RawMessage(`{"risk":"safe_read"}`)},
		},
		Backend: "anthropic", InputTokens: 5, OutputTokens: 2,
	}}
	srv := newCompatServer(t, fp, "")
	body := `{"model":"sonnet","messages":[{"role":"user","content":"classify: ls"}],
		"tools":[{"type":"function","function":{"name":"record_shell_risk","description":"d","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"record_shell_risk"}}}`
	resp, raw := postCompat(t, srv, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	got := fp.lastReq()
	if len(got.Tools) != 1 || got.Tools[0].Name != "record_shell_risk" || got.ToolChoice != "record_shell_risk" {
		t.Fatalf("tools/choice = %+v %q", got.Tools, got.ToolChoice)
	}
	var out compat.ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	ch := out.Choices[0]
	if ch.FinishReason != "tool_calls" || len(ch.Message.ToolCalls) != 1 ||
		ch.Message.ToolCalls[0].Function.Name != "record_shell_risk" ||
		ch.Message.ToolCalls[0].Function.Arguments != `{"risk":"safe_read"}` {
		t.Fatalf("choice = %+v", ch)
	}
	if ch.Message.Content != nil {
		t.Errorf("tool-calls-only content must be null, got %q", *ch.Message.Content)
	}
}

// Opaque round-trip THROUGH the endpoint: a response's thinking blocks come
// back as memcode_opaque, and replaying that assistant message re-expands them
// verbatim (Anthropic signature block + OpenAI rs_ item).
func TestCompatOpaqueRoundTripEndToEnd(t *testing.T) {
	thinking := []wire.Block{
		{Type: "thinking", Thinking: "reasoned", Signature: "sig-1"},
		{Type: "thinking", ID: "rs_777", Thinking: "", Signature: "enc-2"},
	}
	fp := &compatProvider{resp: wire.Response{
		StopReason: "end_turn",
		Blocks:     append(append([]wire.Block{}, thinking...), wire.TextBlock("answer")),
		Backend:    "anthropic", InputTokens: 5, OutputTokens: 2,
	}}
	srv := newCompatServer(t, fp, "")
	resp, raw := postCompat(t, srv, compatBody("sonnet", "think about it"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var out compat.ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	opq := out.Choices[0].Message.MemcodeOpaque
	if len(opq) != 2 {
		t.Fatalf("memcode_opaque = %d items (%s)", len(opq), raw)
	}

	// replay the assistant message with its opaque array — the request the CLI
	// would send on the next tool-use turn
	follow := map[string]any{
		"model": "sonnet",
		"messages": []any{
			map[string]any{"role": "user", "content": "think about it"},
			map[string]any{"role": "assistant", "content": "answer", "memcode_opaque": opq},
			map[string]any{"role": "user", "content": "continue"},
		},
	}
	fb, _ := json.Marshal(follow)
	resp, raw = postCompat(t, srv, string(fb), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow-up: %d %s", resp.StatusCode, raw)
	}
	asst := fp.lastReq().Messages[1]
	if asst.Role != "assistant" || len(asst.Blocks) != 3 {
		t.Fatalf("assistant history = %+v", asst)
	}
	if asst.Blocks[0].Type != "thinking" || asst.Blocks[0].Thinking != "reasoned" || asst.Blocks[0].Signature != "sig-1" {
		t.Errorf("anthropic thinking block did not round-trip: %+v", asst.Blocks[0])
	}
	if asst.Blocks[1].Type != "thinking" || asst.Blocks[1].ID != "rs_777" || asst.Blocks[1].Signature != "enc-2" {
		t.Errorf("openai rs_ item did not round-trip: %+v", asst.Blocks[1])
	}
	if asst.Blocks[2].Type != "text" || asst.Blocks[2].Text != "answer" {
		t.Errorf("assistant text lost: %+v", asst.Blocks[2])
	}
}

// The SSE stream: role chunk first, content deltas, tool-call delta, finish
// chunk, final usage chunk with the memcode extension, [DONE] — and never a
// provider-path leak.
func TestCompatStreamShape(t *testing.T) {
	fp := &compatProvider{resp: wire.Response{
		StopReason: "tool_use",
		Blocks: []wire.Block{
			wire.TextBlock("hello "),
			wire.TextBlock("world"),
			{Type: "tool_use", ID: "call_9", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)},
		},
		Model: "accounts/fireworks/models/glm-5p2", Backend: "fireworks",
		InputTokens: 100, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 10,
		ContextWindow: 202000, InputBudget: 150000,
	}}
	srv := newCompatServer(t, fp, "")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"sonnet","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sekrit")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	var chunks []compat.ChatChunk
	sawDone := false
	var rawAll strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		rawAll.WriteString(line + "\n")
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var c compat.ChatChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		chunks = append(chunks, c)
	}
	if !sawDone {
		t.Fatal("stream must terminate with [DONE]")
	}
	if len(chunks) < 5 {
		t.Fatalf("chunks = %d (%s)", len(chunks), rawAll.String())
	}
	// role chunk first
	if chunks[0].Choices[0].Delta.Role != "assistant" || chunks[0].Object != "chat.completion.chunk" {
		t.Errorf("first chunk = %+v", chunks[0])
	}
	// content accumulates
	var text strings.Builder
	var toolName, toolArgs, toolID string
	var finish string
	var final *compat.ChatChunk
	for i := range chunks {
		c := chunks[i]
		if len(c.Choices) == 0 {
			final = &chunks[i]
			continue
		}
		ch := c.Choices[0]
		if ch.Delta.Content != nil {
			text.WriteString(*ch.Delta.Content)
		}
		for _, td := range ch.Delta.ToolCalls {
			toolID += td.ID
			if td.Function != nil {
				toolName += td.Function.Name
				toolArgs += td.Function.Arguments
			}
		}
		if ch.FinishReason != nil {
			finish = *ch.FinishReason
		}
	}
	if text.String() != "hello world" {
		t.Errorf("accumulated text = %q", text.String())
	}
	if toolName != "read_file" || toolArgs != `{"path":"x"}` || toolID != "call_9" {
		t.Errorf("accumulated tool call = %q %q %q", toolID, toolName, toolArgs)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q", finish)
	}
	// final usage chunk: empty choices + usage + the memcode extension
	if final == nil {
		t.Fatal("missing the final usage chunk (empty choices + usage)")
	}
	if final.Usage == nil || final.Usage.PromptTokens != 140 || final.Usage.CompletionTokens != 20 ||
		final.Usage.PromptTokensDetails == nil || final.Usage.PromptTokensDetails.CachedTokens != 30 {
		t.Errorf("final usage = %+v", final.Usage)
	}
	if final.Memcode == nil || final.Memcode.ContextWindow != 202000 || final.Memcode.InputBudget != 150000 {
		t.Errorf("final memcode ext = %+v", final.Memcode)
	}
	// sanitization holds on the streamed wire too
	low := strings.ToLower(rawAll.String())
	for _, leak := range []string{"accounts/", "fireworks"} {
		if strings.Contains(low, leak) {
			t.Errorf("stream leaked %q: %s", leak, rawAll.String())
		}
	}
	if final.Model != "glm-5p2" {
		t.Errorf("final model = %q, want the sanitized label", final.Model)
	}
}
func TestCompatAuthRequired(t *testing.T) {
	srv := newCompatServer(t, &compatProvider{}, "")
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(compatBody("sonnet", "hi")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	}
}
