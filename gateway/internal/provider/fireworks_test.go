package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Fireworks is the ONE cheap-lane provider (the self-hosted VLLM type is gone
// post-cutover). This locks the abstraction: ModelProvider + Streamer.
func TestFireworksIsModelProvider(t *testing.T) {
	var _ ModelProvider = (*Fireworks)(nil)
	var _ Streamer = (*Fireworks)(nil)
}

func TestFireworksStampsWireBackend(t *testing.T) {
	srv, _ := captureServer(t, http.StatusOK, func(w http.ResponseWriter, _ oaRequest) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	defer srv.Close()
	f := NewFireworks(srv.URL+"/v1", "fw-key", "accounts/fireworks/models/m")
	resp, err := f.Complete(context.Background(), wire.Request{
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// The wire Backend is "fireworks" — the protocol value the CLI's cheap-lane
	// behaviors (budget learning, plan-review gate) and served-by UI match on.
	// (Renamed from the historical "vllm" on 2026-07-14; CLIs accept both.)
	if resp.Backend != "fireworks" {
		t.Fatalf("Backend = %q, want \"fireworks\" (wire value)", resp.Backend)
	}
	if resp.Model != "accounts/fireworks/models/m" {
		t.Errorf("Model = %q", resp.Model)
	}
}

func TestFireworksSendsBearerKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer srv.Close()
	f := NewFireworks(srv.URL, "fw-secret-key", "m")
	if _, err := f.Complete(context.Background(), wire.Request{
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer fw-secret-key" {
		t.Errorf("authorization = %q, want Bearer fw-secret-key", gotAuth)
	}
}

func TestNewFromEnvFireworks(t *testing.T) {
	t.Run("fireworks requires url, key, and model → *Fireworks", func(t *testing.T) {
		t.Setenv(EnvProvider, "fireworks")
		t.Setenv(EnvAPIKey, "") // independence: no Anthropic key needed
		t.Setenv(EnvFireworksURL, "")
		t.Setenv(EnvFireworksKey, "")
		t.Setenv(EnvFireworksModel, "")
		if _, err := NewFromEnv(); err == nil {
			t.Fatal("want error without url")
		}
		t.Setenv(EnvFireworksURL, "https://api.fireworks.ai/inference/v1")
		if _, err := NewFromEnv(); err == nil {
			t.Fatal("want error without key (fireworks requires the API key)")
		}
		t.Setenv(EnvFireworksKey, "fw-key")
		if _, err := NewFromEnv(); err == nil {
			t.Fatal("want error without model")
		}
		t.Setenv(EnvFireworksModel, "accounts/fireworks/models/m")
		p, err := NewFromEnv()
		if err != nil {
			t.Fatalf("NewFromEnv: %v", err)
		}
		f, ok := p.(*Fireworks)
		if !ok {
			t.Fatalf("provider = %T, want *Fireworks", p)
		}
		if f.Model() != "accounts/fireworks/models/m" {
			t.Errorf("model = %q", f.Model())
		}
		defer SetModels("", "", "") // restore package globals for other tests
	})
}

func TestEffectiveModelCollapsesTiersOnFireworks(t *testing.T) {
	t.Setenv(EnvProvider, "fireworks")
	t.Setenv(EnvFireworksModel, "accounts/fireworks/models/m")
	for _, tier := range []string{"opus", "sonnet", "haiku", catalog.ModelOpus} {
		if got := EffectiveModel(tier); got != "accounts/fireworks/models/m" {
			t.Errorf("EffectiveModel(%q) = %q", tier, got)
		}
	}
}
