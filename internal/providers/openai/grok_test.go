package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/wire"
)

// grokResponsesServer fakes xAI's /v1/responses SSE stream (grok speaks the
// OpenAI Responses dialect — same fake shape as the OpenAI adapter tests).
func grokResponsesServer(t *testing.T, onReq func(r *http.Request)) *httptest.Server {
	t.Helper()
	sse := `event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8,"input_tokens_details":{"cached_tokens":0}}}}

`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onReq != nil {
			onReq(r)
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sse))
	}))
}

// TestGrokStampsGrokBackend: grok serves over the Responses dialect and stamps its
// OWN backend label ("grok") — distinct from "openai" and the cheap lane — so the
// cost ledger and served-by UI can tell the vendors apart.
func TestGrokStampsGrokBackend(t *testing.T) {
	var gotPath, gotAuth string
	srv := grokResponsesServer(t, func(r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
	})
	defer srv.Close()
	g := NewGrok("xai-secret-key")
	g.baseURL = srv.URL
	resp, err := g.Complete(context.Background(), wire.Request{
		Model:    catalog.ModelGrok45,
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Backend != "grok" {
		t.Fatalf("Backend = %q, want \"grok\"", resp.Backend)
	}
	if resp.Text() != "ok" {
		t.Fatalf("Text = %q", resp.Text())
	}
	if gotPath != "/responses" {
		t.Fatalf("grok must serve on the responses endpoint, got %q", gotPath)
	}
	if gotAuth != "Bearer xai-secret-key" {
		t.Fatalf("authorization = %q", gotAuth)
	}
}

// TestGrokReasoningEffortClamped: xAI's reasoning.effort vocabulary is low|high —
// the full OpenAI range must clamp (xhigh→high, none/minimal→low) so a hard turn
// reasons at high instead of erroring on an unsupported value.
func TestGrokReasoningEffortClamped(t *testing.T) {
	g := NewGrok("k")
	cases := []struct {
		eff  wire.Effort
		want string
	}{
		{wire.EffortHigh, "high"},
		{wire.EffortMedium, "high"},
		{wire.EffortLow, "low"},
		{wire.EffortOff, "low"},
	}
	for _, c := range cases {
		if got := string(g.effortFor(c.eff)); got != c.want {
			t.Errorf("effortFor(%v) = %q, want %q", c.eff, got, c.want)
		}
	}
	// OpenAI stays unclamped.
	if got := string(NewOpenAI("k").effortFor(wire.EffortHigh)); got != "xhigh" {
		t.Errorf("openai effortFor(high) = %q, want xhigh", got)
	}
}

// TestGrokNoEncryptedInclude: the encrypted-reasoning includable is OpenAI-only —
// grok requests must not carry it (xAI rejects unknown includables).
func TestGrokNoEncryptedInclude(t *testing.T) {
	g := NewGrok("k")
	p := g.buildParams(wire.Request{Model: catalog.ModelGrok45}, 4096)
	if len(p.Include) != 0 {
		t.Fatalf("grok must not request includables: %v", p.Include)
	}
	if p2 := NewOpenAI("k").buildParams(wire.Request{Model: catalog.ModelTerra}, 4096); len(p2.Include) == 0 {
		t.Fatal("openai must keep the encrypted-reasoning includable")
	}
}

// TestGrokModelReturnsGrok45 locks that Model() returns the grok-4.5 id.
func TestGrokModelReturnsGrok45(t *testing.T) {
	g := NewGrok("key")
	if g.Model() != catalog.ModelGrok45 {
		t.Errorf("Model() = %q, want %q", g.Model(), catalog.ModelGrok45)
	}
}
