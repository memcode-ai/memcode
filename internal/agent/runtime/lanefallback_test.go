package runtime

import (
	"context"
	"os/exec"
	"testing"

	"github.com/memcode-ai/memcode/internal/config"
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

// laneRoot is an initialized project (git + .memcode) so the card's "don't
// ask again" persists to a temp config — never the developer's real one.
func laneRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if _, err := config.Init(root, false); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	return root
}

func answer(label string) func(context.Context, AskRequest) AskResponse {
	return func(context.Context, AskRequest) AskResponse { return AskResponse{Answer: label} }
}

// The exhaustion card offers Continue / Continue and don't ask again / Cancel.
// Headless and no-gateway fail closed.
func TestConsentLaneFallbackGuards(t *testing.T) {
	s := &Session{purpose: llm.MainLoop, root: laneRoot(t), turn: newTurnState()}
	if s.consentLaneFallback(context.Background(), exhErr(false)) {
		t.Fatal("consented with no fallback path")
	}
	if s.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("headless session consented")
	}
	var got []string
	s.ask = func(_ context.Context, r AskRequest) AskResponse {
		for _, o := range r.Options {
			got = append(got, o.Label)
		}
		return AskResponse{}
	}
	if s.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("empty answer (Esc) consented")
	}
	want := []string{optLaneContinue, optLaneAlways, optLaneCancel}
	if len(got) != len(want) {
		t.Fatalf("options = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("options = %q, want %q", got, want)
		}
	}
}

// Continue serves this turn on the gateway but is NOT remembered: the next
// exhaustion asks again (the window may have reset in between).
func TestConsentLaneFallbackContinueIsOneTurn(t *testing.T) {
	s := &Session{purpose: llm.MainLoop, root: laneRoot(t), turn: newTurnState()}
	s.ask = answer(optLaneContinue)
	if !s.consentLaneFallback(context.Background(), exhErr(true)) || s.turn.laneBypass != "gateway" {
		t.Fatalf("continue: bypass = %q, want gateway", s.turn.laneBypass)
	}
	asked := false
	s.ask = func(context.Context, AskRequest) AskResponse { asked = true; return AskResponse{Answer: optLaneCancel} }
	s.turn = newTurnState()
	if s.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("cancel consented")
	}
	if !asked {
		t.Fatal("one-turn continue was remembered — the next exhaustion must ask again")
	}
}

// Cancel stops the turn and is not remembered either.
func TestConsentLaneFallbackCancelNotSticky(t *testing.T) {
	s := &Session{purpose: llm.MainLoop, root: laneRoot(t), turn: newTurnState()}
	s.ask = answer(optLaneCancel)
	if s.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("cancel consented")
	}
	s.ask = answer(optLaneContinue)
	if !s.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("cancel stuck — a later continue was refused")
	}
}

// "Don't ask again" persists per vendor to .memcode/config.json: later
// exhaustions (even in a new session) continue without asking; other
// vendors still ask.
func TestConsentLaneFallbackDontAskAgainPersists(t *testing.T) {
	root := laneRoot(t)
	s := &Session{purpose: llm.MainLoop, root: root, turn: newTurnState()}
	s.ask = answer(optLaneAlways)
	if !s.consentLaneFallback(context.Background(), exhErr(true)) {
		t.Fatal("don't-ask-again refused")
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LaneFallback["anthropic"] != "gateway" {
		t.Fatalf("lane_fallback = %v, want anthropic→gateway", cfg.LaneFallback)
	}

	fresh := &Session{purpose: llm.MainLoop, root: root, turn: newTurnState()}
	fresh.ask = func(context.Context, AskRequest) AskResponse {
		t.Fatal("remembered choice re-asked")
		return AskResponse{}
	}
	if !fresh.consentLaneFallback(context.Background(), exhErr(true)) || fresh.turn.laneBypass != "gateway" {
		t.Fatal("remembered choice not applied")
	}

	other := exhErr(true)
	other.Lane = provider.LaneInfo{Vendor: "openai", Name: "codex", Kind: "sub"}
	asked := false
	fresh.ask = func(context.Context, AskRequest) AskResponse { asked = true; return AskResponse{} }
	fresh.consentLaneFallback(context.Background(), other)
	if !asked {
		t.Fatal("another vendor's exhaustion must still ask")
	}
}
