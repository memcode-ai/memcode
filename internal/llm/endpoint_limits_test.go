package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// endpointRunner points a Runner at a fake strict endpoint with the given
// limits, recording each request's output budget.
func endpointRunner(t *testing.T, limits provider.Limits, model string) (*Runner, *[]int) {
	t.Helper()
	for _, k := range []string{provider.EnvAPIToken, provider.EnvEndpointURL, provider.EnvCredentials, provider.EnvCredentialSource} {
		t.Setenv(k, "")
	}
	var got []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxTokens           int `json:"max_tokens"`
			MaxCompletionTokens int `json:"max_completion_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body.MaxTokens+body.MaxCompletionTokens)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	lz := provider.NewFromEnvLazy(provider.Endpoint{Name: "tsubasa", BaseURL: srv.URL + "/v1", Key: "k", Model: model, Limits: limits})
	r := NewRunner(lz)
	r.SetPin(model)
	return r, &got
}

func toolsReq(n, maxTok int) wire.Request {
	req := userReq("Reply with a short greeting, without tools.")
	req.MaxTokens = maxTok
	for i := 0; i < n; i++ {
		req.Tools = append(req.Tools, wire.ToolDef{Name: fmt.Sprintf("tool_%d", i),
			Description: strings.Repeat("does a thing. ", 20),
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}})
	}
	return req
}

// Issue #15: a 32K endpoint with an 8K output cap must receive at most 8K of
// output budget, and never more than the window leaves after the request.
func TestEndpointLimitsReachTheWire(t *testing.T) {
	r, got := endpointRunner(t, provider.Limits{ContextWindow: 32768, MaxOutput: 8192}, "tsubasa-fast")
	if _, err := r.Complete(context.Background(), MainLoop, toolsReq(30, 16384)); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0] != 8192 {
		t.Fatalf("output budget on the wire = %v, want the endpoint's 8192 cap", *got)
	}

	// A window that the request nearly fills: output shrinks to the room left.
	r, got = endpointRunner(t, provider.Limits{ContextWindow: 6000}, "tsubasa-pro")
	if _, err := r.Complete(context.Background(), MainLoop, toolsReq(30, 16384)); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0] <= 0 || (*got)[0] >= 6000 {
		t.Fatalf("output budget = %v, want the room the 6000-token window leaves", *got)
	}
}

// A request that cannot fit at all never reaches the server: it is a context
// overflow, which the loop answers by compacting.
func TestEndpointOverflowIsRefusedBeforeDispatch(t *testing.T) {
	r, got := endpointRunner(t, provider.Limits{ContextWindow: 1000}, "tsubasa-pro")
	_, err := r.Complete(context.Background(), MainLoop, toolsReq(30, 16384))
	if !errors.Is(err, wire.ErrContextOverflow) {
		t.Fatalf("err = %v, want ErrContextOverflow", err)
	}
	if len(*got) != 0 {
		t.Fatalf("an overflowing request reached the server: %v", *got)
	}
}

// No declared limits: the request goes out as asked.
func TestEndpointWithoutLimitsIsUntouched(t *testing.T) {
	r, got := endpointRunner(t, provider.Limits{}, "local-model")
	if _, err := r.Complete(context.Background(), MainLoop, toolsReq(30, 16384)); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0] != 16384 {
		t.Fatalf("output budget = %v, want the requested 16384", *got)
	}
}
