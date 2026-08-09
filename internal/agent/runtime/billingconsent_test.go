package runtime

// The credits-consent contract: after a BYOK key failure, ONLY an explicit
// per-turn yes moves the turn onto memcode credits (the request then carries
// BillingLane "credits"); headless and sub-agent sessions keep fail-the-turn.

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

func newConsentSession(t *testing.T, prov *byokFlakyProv) *Session {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAllowAll, io.Discard)
}

// byokFlakyProv fails with ErrByokKeyFailed until a request arrives with
// BillingLane "credits", recording every lane it saw.
type byokFlakyProv struct {
	lanes []string
}

func (p *byokFlakyProv) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	p.lanes = append(p.lanes, r.BillingLane)
	if r.BillingLane != "credits" {
		return wire.Response{}, wire.ErrByokKeyFailed
	}
	return wire.Response{StopReason: "end_turn", Model: "sonnet", Backend: "anthropic",
		Blocks: []wire.Block{wire.TextBlock("done")}, InputTokens: 1, OutputTokens: 1}, nil
}

func TestCreditsConsentRetriesTurn(t *testing.T) {
	prov := &byokFlakyProv{}
	s := newConsentSession(t, prov)
	asked := 0
	s.SetAsker(func(_ context.Context, req AskRequest) AskResponse {
		asked++
		return AskResponse{Answer: "Retry on credits"}
	})
	out, err := s.Run(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = out
	if asked != 1 {
		t.Fatalf("asked=%d, want exactly one consent question", asked)
	}
	if len(prov.lanes) < 2 || prov.lanes[0] != "" || prov.lanes[len(prov.lanes)-1] != "credits" {
		t.Fatalf("lanes=%v, want byok-preferred then an explicit credits retry", prov.lanes)
	}
}

func TestCreditsConsentDeclinedFailsTurn(t *testing.T) {
	prov := &byokFlakyProv{}
	s := newConsentSession(t, prov)
	s.SetAsker(func(_ context.Context, req AskRequest) AskResponse {
		return AskResponse{Answer: "Stop the turn"}
	})
	if _, err := s.Run(context.Background(), "do the thing"); err != nil {
		t.Fatalf("declined consent must consume the error (user-action notice), got %v", err)
	}
	for _, l := range prov.lanes {
		if l == "credits" {
			t.Fatal("declined consent must never send the credits lane")
		}
	}
}

func TestCreditsConsentHeadlessNeverAsks(t *testing.T) {
	prov := &byokFlakyProv{}
	s := newConsentSession(t, prov)
	s.SetAsker(nil) // headless
	if _, err := s.Run(context.Background(), "do the thing"); err != nil {
		t.Fatalf("headless byok failure must be consumed as a notice, got %v", err)
	}
	for _, l := range prov.lanes {
		if l == "credits" {
			t.Fatal("headless sessions must never retry on credits")
		}
	}
}
