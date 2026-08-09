package advisor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
)

func advisorSSE(text string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

func TestAdviseSendsReasoningEffortAndReturnsAdvice(t *testing.T) {
	var gotModel, gotEffort, gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The Anthropic SDK sends the key as x-api-key (NOT Bearer).
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("missing/wrong auth header: %q", r.Header.Get("x-api-key"))
		}
		// The Anthropic SDK sends the anthropic-version header.
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic-version header")
		}
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Model    string                  `json:"model"`
			System   []struct{ Text string } `json:"system"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
			Thinking     struct{ Type string }   `json:"thinking"`
			OutputConfig struct{ Effort string } `json:"output_config"`
			MaxTokens    int                     `json:"max_tokens"`
		}
		_ = json.Unmarshal(raw, &in)
		gotModel, gotEffort = in.Model, in.OutputConfig.Effort
		if len(in.System) > 0 {
			gotSystem = in.System[0].Text
		}
		// Respond in the streamed Messages shape (the shared adapter streams
		// under the hood).
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(advisorSSE("Recommendation: do X. Risk: Y.")))
	}))
	defer srv.Close()

	a := New("sk-test", "")
	a.setBaseURL(srv.URL)

	advice, err := a.Advise(context.Background(), "should we use serverless or a pod?", "low")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(advice, "Recommendation: do X") {
		t.Fatalf("advice not returned, got %q", advice)
	}
	// The default model is Claude Sonnet (catalog.ModelSonnet) — a cheaper cross-vendor
	// second opinion than Opus, overridable via ANTHROPIC_ADVISOR_MODEL.
	if gotModel != catalog.ModelSonnet {
		t.Fatalf("model = %q, want %q (Claude Sonnet)", gotModel, catalog.ModelSonnet)
	}
	// effort "low" maps to Anthropic's "low" effort.
	if gotEffort != "low" {
		t.Fatalf("effort = %q, want %q", gotEffort, "low")
	}
	// The system prompt should ride in the `system` field (not messages), and carry
	// the advisory role.
	if !strings.Contains(gotSystem, "second-opinion advisor") {
		t.Fatalf("advisory system prompt missing, got %q", gotSystem)
	}
}

func TestAdviseDefaultsToHighEffort(t *testing.T) {
	var gotEffort string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			OutputConfig struct{ Effort string } `json:"output_config"`
		}
		_ = json.Unmarshal(raw, &in)
		gotEffort = in.OutputConfig.Effort
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(advisorSSE("advice")))
	}))
	defer srv.Close()

	a := New("sk-test", "")
	a.setBaseURL(srv.URL)
	if _, err := a.Advise(context.Background(), "q", ""); err != nil {
		t.Fatal(err)
	}
	// Empty effort defaults to "high".
	if gotEffort != "high" {
		t.Fatalf("default effort = %q, want %q", gotEffort, "high")
	}
}

func TestAdviseUnconfigured(t *testing.T) {
	a := New("", "")
	if a.Available() {
		t.Fatal("an unkeyed advisor must report Available()=false")
	}
	if _, err := a.Advise(context.Background(), "q", ""); err == nil {
		t.Fatal("unkeyed advisor must error, not panic")
	}
}
