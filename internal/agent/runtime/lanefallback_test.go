package runtime

import (
	"context"
	"testing"

	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
)

func exhErr(canFallback bool) *provider.ErrLaneExhausted {
	return &provider.ErrLaneExhausted{
		Lane:        provider.LaneInfo{Vendor: "anthropic", Name: "claude-sub", Kind: "sub"},
		Status:      429,
		CanFallback: canFallback,
	}
}

// The exhaustion card: stop is the default, "continue" flips the turn onto
// the gateway bypass and sticks for the vendor, headless fails closed.
func TestConsentLaneFallback(t *testing.T) {
	s := &Session{purpose: llm.MainLoop}

	// No gateway → nothing to offer.
	if s.consentLaneFallback(context.Background(), exhErr(false)) {
		t.Fatal("consented with no fallback path")
	}

	// Headless (no asker) → fail closed.
	if s.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("headless session consented")
	}

	// Explicit continue → bypass set + sticky.
	s.ask = func(ctx context.Context, r AskRequest) AskResponse {
		if r.Options[0].Label != "Stop the turn" {
			t.Fatalf("stop must be the FIRST (default) option, got %q", r.Options[0].Label)
		}
		return AskResponse{Answer: "Continue on memcode credits"}
	}
	s.turn = newTurnState()
	if !s.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("explicit continue refused")
	}
	if s.turn.laneBypass != "gateway" {
		t.Fatalf("laneBypass = %q", s.turn.laneBypass)
	}

	// Sticky: a second exhaustion for the vendor never re-asks.
	s.ask = func(ctx context.Context, r AskRequest) AskResponse {
		t.Fatal("sticky choice re-asked")
		return AskResponse{}
	}
	s.turn = newTurnState()
	if !s.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("sticky continue not applied")
	}

	// Empty answer (Esc) → stop, and THAT sticks too.
	s2 := &Session{purpose: llm.MainLoop, turn: newTurnState()}
	s2.ask = func(ctx context.Context, r AskRequest) AskResponse { return AskResponse{} }
	if s2.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("empty answer consented")
	}
	s2.ask = func(ctx context.Context, r AskRequest) AskResponse {
		t.Fatal("sticky stop re-asked")
		return AskResponse{}
	}
	if s2.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("sticky stop consented")
	}
}
