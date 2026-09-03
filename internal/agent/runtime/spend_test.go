package runtime

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// usageProvider returns a response with fixed token usage so the metered runner has
// something to record.
type usageProvider struct{ in, out int }

func (u usageProvider) Complete(_ context.Context, _ wire.Request) (wire.Response, error) {
	return wire.Response{InputTokens: u.in, OutputTokens: u.out, StopReason: "end_turn"}, nil
}

// TestSpendByPurpose: the per-purpose breakdown aggregates and sorts by cost, and
// the session total counts EVERY purpose (including sub-agent-style explore calls).
func TestSpendByPurpose(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, usageProvider{in: 1000, out: 200}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	// main_loop on Opus (pricey) once; explore on Haiku (cheap) twice.
	s.runner.Complete(ctx, llm.MainLoop, wire.Request{Model: catalog.ModelOpus})
	s.runner.Complete(ctx, llm.Explore, wire.Request{Model: catalog.ModelHaiku})
	s.runner.Complete(ctx, llm.Explore, wire.Request{Model: catalog.ModelHaiku})

	by := s.SpendByPurpose()
	if len(by) != 2 {
		t.Fatalf("want 2 purposes, got %d: %+v", len(by), by)
	}
	// Sorted by cost desc → Opus main_loop first despite explore having more calls.
	if by[0].Purpose != string(llm.MainLoop) {
		t.Errorf("most-expensive purpose should be main_loop, got %q", by[0].Purpose)
	}
	if by[1].Purpose != string(llm.Explore) || by[1].Calls != 2 {
		t.Errorf("explore should be 2 calls, got %+v", by[1])
	}
	// The total counts all three calls.
	if in, out, _, _, usd := s.Spend(); in != 3000 || out != 600 || usd <= 0 {
		t.Errorf("total wrong: in=%d out=%d usd=%v", in, out, usd)
	}
}

// TestLoopPurposeFlows: a session's main loop tags the ledger with s.purpose — so a
// scout sub-session (purpose=explore) attributes separately from the main loop.
func TestLoopPurposeFlows(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, usageProvider{in: 500, out: 100}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.purpose = llm.Explore // emulate an explore scout
	if _, err := s.complete(ctx, s.purpose, wire.Request{Model: catalog.ModelSonnet}, 0, true); err != nil {
		t.Fatal(err)
	}
	by := s.SpendByPurpose()
	if len(by) != 1 || by[0].Purpose != string(llm.Explore) {
		t.Fatalf("main-loop call should tag as explore, got %+v", by)
	}
}

// TestScoutModelDefaultsLuna: read-only scouts default to the cheap model (Luna), and
// TestScoutModelDefaultsLuna is DELETED. Which model a scout runs on is POLICY
// now (agent.explore, inheriting agent.delegated, inheriting the primary pin),
// covered by internal/policy's resolution tests and by TestExploreNarrowsDelegated.
