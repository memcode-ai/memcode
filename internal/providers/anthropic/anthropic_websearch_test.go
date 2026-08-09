package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A real web_search response carries server_tool_use + web_search_tool_result
// blocks; the latter's `content` is an ARRAY. Decoding this into wireResponse
// (Block.Content is a string) fails — the bug that surfaced as "Web Search · failed"
// with no visible reason. WebSearch must parse permissively and return the text.
const sampleWebSearchResponse = `{
  "type": "message",
  "stop_reason": "end_turn",
  "content": [
    {"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": {"query": "latest news today"}},
    {"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1", "content": [
      {"type": "web_search_result", "title": "Headline A", "url": "https://example.com/a"},
      {"type": "web_search_result", "title": "Headline B", "url": "https://example.com/b"}
    ]},
    {"type": "text", "text": "Today's top story: "},
    {"type": "text", "text": "something happened."}
  ],
  "usage": {"input_tokens": 120, "output_tokens": 45, "server_tool_use": {"web_search_requests": 2}}
}`

func TestWebSearchParsesServerToolResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(sampleWebSearchResponse))
	}))
	defer srv.Close()

	c := NewAnthropic("test-key")
	c.baseURL = srv.URL

	out, usage, err := c.WebSearch(context.Background(), "latest news today")
	if err != nil {
		t.Fatalf("WebSearch errored on a valid server-tool response: %v", err)
	}
	if !strings.Contains(out, "Today's top story") || !strings.Contains(out, "something happened.") {
		t.Errorf("expected the synthesized text blocks, got: %q", out)
	}
	// The usage must ride back for side-channel metering — these calls moved
	// real money invisibly when WebSearch returned text alone.
	if usage.InputTokens != 120 || usage.OutputTokens != 45 || usage.Model == "" {
		t.Errorf("usage must carry the billed tokens + model, got %+v", usage)
	}
	// The per-request search fee counter must ride too: each search bills $10/1k
	// upstream (usage.server_tool_use.web_search_requests), which tokens alone
	// under-metered until SearchFeeUSD picked this count up.
	if usage.SearchCount != 2 {
		t.Errorf("usage.SearchCount = %d, want 2 (server_tool_use.web_search_requests)", usage.SearchCount)
	}
}

// Regression: Anthropic splits a web_search reply into multiple text blocks at citation
// boundaries — often mid-sentence/mid-word, exactly like sampleWebSearchResponse's two
// fragments above. They're contiguous pieces of ONE string, not separate paragraphs, so
// joining them must NOT insert a separator; a stray "\n" here reproduced as a real
// newline in the final answer, splitting a markdown bullet's "- " marker from its own
// text onto two lines whenever a citation landed right after the marker.
func TestWebSearchJoinsCitationSplitBlocksWithNoSeparator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(sampleWebSearchResponse))
	}))
	defer srv.Close()

	c := NewAnthropic("test-key")
	c.baseURL = srv.URL

	out, _, err := c.WebSearch(context.Background(), "latest news today")
	if err != nil {
		t.Fatalf("WebSearch errored: %v", err)
	}
	if want := "Today's top story: something happened."; out != want {
		t.Errorf("citation-split blocks joined wrong: got %q, want %q", out, want)
	}
	if strings.Contains(out, "\n") {
		t.Errorf("joined text must not contain a spurious newline at a citation boundary: %q", out)
	}
}

// A genuine API error must still surface its message (not a decode error).
func TestWebSearchSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad tool"}}`))
	}))
	defer srv.Close()

	c := NewAnthropic("test-key")
	c.baseURL = srv.URL

	_, _, err := c.WebSearch(context.Background(), "q")
	if err == nil || !strings.Contains(err.Error(), "invalid_request_error") || !strings.Contains(err.Error(), "bad tool") {
		t.Errorf("expected the API error surfaced, got: %v", err)
	}
}
