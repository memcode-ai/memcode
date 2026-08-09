package compat

// degrade_test.go — the reasoning-vs-tools capability degradation: OpenAI's
// chat/completions rejects function tools whenever reasoning is active for
// its reasoning models (which default to reasoning when the field is
// omitted), demanding an explicit reasoning_effort:"none". The transport
// retries ONCE with "none" when an endpoint says so — keyed on the error
// text, never the hostname.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

func toolReq(effort wire.Effort) wire.Request {
	return wire.Request{
		Pin:        "gpt-5.6-terra",
		Effort:     effort,
		ToolChoice: "record_verdict",
		Tools: []wire.ToolDef{{Name: "record_verdict", Description: "record it",
			InputSchema: map[string]any{"type": "object"}}},
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("verdict?")}}},
	}
}

// An endpoint that 400s tools-with-reasoning until reasoning_effort:"none"
// arrives — the OpenAI 5.6 chat/completions behavior, scripted.
func reasoningRejector(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var efforts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReasoningEffort string            `json:"reasoning_effort"`
			Tools           []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		efforts = append(efforts, body.ReasoningEffort)
		if len(body.Tools) > 0 && body.ReasoningEffort != "none" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Function tools with reasoning_effort are not supported for this model in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"record_verdict","arguments":"{\"verdict\":\"yes\"}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &efforts
}

func TestReasoningToolsDegradeRetry(t *testing.T) {
	srv, efforts := reasoningRejector(t)
	tr := New(Config{BaseURL: srv.URL, Memcode: false})

	// Omitted effort (the default-reasoning trap): first attempt 400s, the
	// retry carries the explicit "none" and the tool call lands.
	resp, err := tr.Complete(context.Background(), toolReq(wire.EffortOff))
	if err != nil {
		t.Fatalf("degradation must rescue the call: %v", err)
	}
	if len(resp.ToolUses()) != 1 {
		t.Fatalf("want the tool call, got %+v", resp.Blocks)
	}
	if len(*efforts) != 2 || (*efforts)[0] != "" || (*efforts)[1] != "none" {
		t.Fatalf("efforts sent = %v, want [\"\" none]", *efforts)
	}

	// A judged high-effort tool turn degrades the same way (thinking depth is
	// worth less than a dead turn).
	*efforts = nil
	if _, err := tr.Complete(context.Background(), toolReq(wire.EffortHigh)); err != nil {
		t.Fatalf("high-effort degradation: %v", err)
	}
	if len(*efforts) != 2 || (*efforts)[0] != "high" || (*efforts)[1] != "none" {
		t.Fatalf("efforts sent = %v, want [high none]", *efforts)
	}
}

func TestReasoningDegradeNeverLoops(t *testing.T) {
	// An endpoint that 400s mentioning reasoning_effort EVEN with "none" must
	// get exactly one retry, then the error surfaces.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"reasoning_effort is cursed here"}}`))
	}))
	t.Cleanup(srv.Close)
	tr := New(Config{BaseURL: srv.URL, Memcode: false})
	if _, err := tr.Complete(context.Background(), toolReq(wire.EffortOff)); err == nil {
		t.Fatal("want the 400 to surface after one degrade retry")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want exactly 2 (original + one degrade retry)", calls)
	}
}

func TestReasoningDegradeStreamPath(t *testing.T) {
	srv, efforts := reasoningRejector(t)
	// The rejector answers non-SSE JSON even for stream requests; the decoder
	// tolerates it poorly, so only assert the RETRY fired with "none" — the
	// wire mechanics of streaming are covered elsewhere.
	tr := New(Config{BaseURL: srv.URL, Memcode: false})
	_, _ = tr.Stream(context.Background(), toolReq(wire.EffortOff), wire.StreamHandler{})
	if len(*efforts) != 2 || (*efforts)[1] != "none" {
		t.Fatalf("stream efforts sent = %v, want the degrade retry", *efforts)
	}
}
