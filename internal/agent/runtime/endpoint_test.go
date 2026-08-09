package runtime

// Endpoint-mode capability gating (one-wire Phase C): off the memcode gateway
// there is no server-side search backend, so the web_search tool def must not
// be advertised at all — the same pattern as the artifact tool's account gate.
// fetch stays (a local HTTP fetch; it merely loses its server-side escalation),
// and everything else in the matrix degrades at the call site.

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
)

// endpointProvider is a fake with the Endpointer capability — the runtime's
// whole view of endpoint mode.
type endpointProvider struct{ captureProvider }

func (endpointProvider) Endpoint() (provider.Endpoint, bool) {
	return provider.Endpoint{Name: "localhost:11434", BaseURL: "http://localhost:11434/v1", Model: "qwen3:4b"}, true
}

func endpointSession(t *testing.T) *Session {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newSess(st, &endpointProvider{}, t.TempDir(), "qwen3:4b", permissions.ModeAsk, io.Discard)
}

// TestWebSearchHiddenOffMemcode: the def vanishes in endpoint mode; fetch and
// the rest of the executive surface stay.
func TestWebSearchHiddenOffMemcode(t *testing.T) {
	s := endpointSession(t)
	defs := s.toolDefs()
	if hasTool(defs, tools.WebSearch) {
		t.Error("web_search must NOT be advertised off the memcode backend")
	}
	if !hasTool(defs, tools.Fetch) {
		t.Error("fetch is local and must stay advertised in endpoint mode")
	}
	if !s.allowTool(tools.ReadFile) || !s.allowTool(tools.Bash) {
		t.Error("endpoint mode must not disturb the ordinary tool surface")
	}

	// The hosted/test-fake baseline still advertises it (no behavior change).
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hosted := newSess(st, &captureProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	if !hasTool(hosted.toolDefs(), tools.WebSearch) {
		t.Error("hosted sessions must keep the web_search def")
	}
}

// TestEndpointSeamForwarding: Session.Endpoint forwards the provider
// capability, and a provider without it reports hosted (test fakes count as
// connected AND not-endpoint — no test churn).
func TestEndpointSeamForwarding(t *testing.T) {
	s := endpointSession(t)
	ep, ok := s.Endpoint()
	if !ok || ep.Name != "localhost:11434" || !s.endpointMode() {
		t.Fatalf("endpoint seam broken: %+v ok=%v", ep, ok)
	}
	plain := &Session{turn: newTurnState()}
	if _, ok := plain.Endpoint(); ok || plain.endpointMode() {
		t.Error("a provider without the capability must read as not-endpoint")
	}
	if !s.Connected() {
		t.Error("endpoint sessions count as connected (fakes lack Connector, so this is the default-true path)")
	}
}
