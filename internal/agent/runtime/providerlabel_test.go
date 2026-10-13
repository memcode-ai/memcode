package runtime

// providerlabel_test.go — the footer's "provider · model" segment names WHO is
// serving before WHAT model: the lane the router will send the next turn to
// wins first (a lane never sets a custom Endpoint), then a custom endpoint's
// name (ollama, etc.), else it falls back to the serving model's catalog
// vendor so the hosted/default path isn't bare either.

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

// plannedProvider is a fake Planner: the router says the next turn on any pin
// routes to backend — standing in for Lazy's lane set.
type plannedProvider struct {
	captureProvider
	backend string
}

func (p *plannedProvider) PlannedBackend(string) string { return p.backend }

// TestProviderLabelSubscriptionLane: a subscription lane (claude/codex/
// copilot/grok) never sets a custom Endpoint — it serves via Lazy.laneSet,
// so Endpoint() reports ok=false. The footer must name the lane the NEXT turn
// routes to, from the first frame (no turn served yet) — this was the bug: a
// claude subscription showed "anthropic · sonnet" instead of "claude · sonnet".
func TestProviderLabelSubscriptionLane(t *testing.T) {
	cases := []struct {
		backend string
		want    string
	}{
		{"claude-sub", "claude"},
		{"codex", "codex"},
		{"copilot", "github"},
		{"grok-sub", "grok"},
	}
	for _, c := range cases {
		s := labelSession(t, &plannedProvider{backend: c.backend}, "sonnet")
		if got := s.ProviderLabel(); got != c.want {
			t.Errorf("ProviderLabel() routing to lane %q = %q, want %q", c.backend, got, c.want)
		}
	}
}

// TestProviderLabelOwnKeyLane: an own-key lane ("ownkey:openai") names its
// vendor plain — the footer's separate "byok" segment already says whose key
// it was, so ProviderLabel must not repeat it as "your openai key".
func TestProviderLabelOwnKeyLane(t *testing.T) {
	s := labelSession(t, &plannedProvider{backend: "ownkey:openai"}, "gpt-5")
	if got := s.ProviderLabel(); got != "openai" {
		t.Errorf("ProviderLabel() routing to own-key lane = %q, want %q", got, "openai")
	}
}

// TestProviderLabelIgnoresLastServedStamp: the label is what WILL serve, not
// what did. After a /model switch off the claude lane onto a gateway-served
// model, the stale "claude-sub" stamp from the last turn must not linger.
func TestProviderLabelIgnoresLastServedStamp(t *testing.T) {
	s := labelSession(t, &plannedProvider{backend: ""}, "gpt-6-sol")
	s.recordServed(func(v *servedState) { v.backend = "claude-sub" })
	if got := s.ProviderLabel(); got != "openai" {
		t.Errorf("ProviderLabel() after switching off the claude lane = %q, want %q", got, "openai")
	}
}
