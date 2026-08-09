package provider

// Lane test fixtures: a capture server speaking the lane's chat-completions
// wire (a TEST decoder, not a protocol implementation — the engine lives in
// providers/compat).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type oaFunc struct {
	Name        string `json:"name"`
	Arguments   string `json:"arguments,omitempty"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type oaToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function oaFunc `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function oaFunc `json:"function"`
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaRequest struct {
	Model           string      `json:"model"`
	Messages        []oaMessage `json:"messages"`
	Tools           []oaTool    `json:"tools,omitempty"`
	MaxTokens       int         `json:"max_tokens,omitempty"`
	Stream          bool        `json:"stream,omitempty"`
	ReasoningEffort string      `json:"reasoning_effort,omitempty"`
	User            string      `json:"user,omitempty"`
}

// captureServer is a fake OpenAI-compatible endpoint: it records the decoded
// request and returns a canned body (or an SSE stream).
func captureServer(t *testing.T, status int, respond func(w http.ResponseWriter, req oaRequest)) (*httptest.Server, *oaRequest) {
	t.Helper()
	captured := &oaRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(captured); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		w.WriteHeader(status)
		respond(w, *captured)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}
