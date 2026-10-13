package runtime

// providerlabel_test.go — the footer's "provider · model" segment names WHO is
// serving before WHAT model: a custom endpoint's name (a subscription login or
// an own key) wins, else it falls back to the serving model's catalog vendor
// so the hosted/default path isn't bare either.

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
)

// namedEndpointProvider is a fake Endpointer that reports whatever name/model
// the test gives it — standing in for ollama, a claude-sub login, etc.
type namedEndpointProvider struct {
	captureProvider
	name  string
	model string
}

func (p *namedEndpointProvider) Endpoint() (provider.Endpoint, bool) {
	return provider.Endpoint{Name: p.name, Model: p.model}, true
}

func labelSession(t *testing.T, prov provider.ModelProvider, model string) *Session {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newSess(st, prov, t.TempDir(), model, permissions.ModeAsk, io.Discard)
}

// TestProviderLabelCustomEndpoint: a custom endpoint's name wins and runs
// through ServingLabel — "ollama" stays "ollama", "claude-sub" reads "claude".
func TestProviderLabelCustomEndpoint(t *testing.T) {
	cases := []struct {
		epName string
		want   string
	}{
		{"ollama", "ollama"},
		{"claude-sub", "claude"},
		{"codex", "codex"},
		{"anthropic", "anthropic"},
	}
	for _, c := range cases {
		s := labelSession(t, &namedEndpointProvider{name: c.epName, model: "qwen3:4b"}, "qwen3:4b")
		if got := s.ProviderLabel(); got != c.want {
			t.Errorf("ProviderLabel() with endpoint %q = %q, want %q", c.epName, got, c.want)
		}
	}
}

// TestProviderLabelHostedFallsBackToCatalogVendor: no custom endpoint ⇒ the
// serving model's catalog vendor names the provider (hosted-memcode default).
func TestProviderLabelHostedFallsBackToCatalogVendor(t *testing.T) {
	s := labelSession(t, &captureProvider{}, "sonnet")
	if got := s.ProviderLabel(); got != "anthropic" {
		t.Errorf("ProviderLabel() hosted on sonnet = %q, want %q", got, "anthropic")
	}
}
