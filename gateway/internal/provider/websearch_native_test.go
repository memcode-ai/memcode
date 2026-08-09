package provider

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// Native web search: a strong provider swaps the CLI's web_search FUNCTION def for its
// built-in server-side search at request build (OpenAI: Responses web_search tool;
// Anthropic: web_search_20250305). The cheap lane (Fireworks) keeps the function def —
// it round-trips to /v1/websearch. The OpenAI/Grok halves of this suite moved with the
// shared adapter (providers/openai).

func searchToolReq() wire.Request {
	return wire.Request{
		Model: "m",
		Tools: []wire.ToolDef{
			{Name: "read_file", InputSchema: map[string]any{"type": "object"}},
			{Name: "web_search", InputSchema: map[string]any{"type": "object"}},
			{Name: "bash", InputSchema: map[string]any{"type": "object"}},
		},
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}},
	}
}

// The cheap lane (Fireworks) keeps the web_search FUNCTION def — no native
// search there; the CLI executes it via the gateway's /v1/websearch side channel.
func TestFireworksKeepsWebSearchDef(t *testing.T) {
	srv, got := captureServer(t, 200, func(w http.ResponseWriter, _ oaRequest) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	})
	fw := NewFireworks(srv.URL+"/v1", "k", "m")
	if _, err := fw.Complete(context.Background(), searchToolReq()); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 3 {
		t.Fatalf("fireworks must keep the web_search function def: %#v", got.Tools)
	}
}
