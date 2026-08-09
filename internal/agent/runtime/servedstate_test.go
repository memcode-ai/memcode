package runtime

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// TestServedByokPerTurn locks the footer-clearing contract: recordServed writes
// the byok flag unconditionally from every response, so a BYOK-served turn
// lights the footer segment and the next non-BYOK turn clears it — nothing
// sticky.
func TestServedByokPerTurn(t *testing.T) {
	s := &Session{}
	if s.ServedByok() {
		t.Fatal("fresh session must not claim a byok serve")
	}
	s.recordServed(func(v *servedState) { v.backend, v.byok = "cheap", true })
	if !s.ServedByok() {
		t.Fatal("a BYOK-served turn must set ServedByok")
	}
	s.recordServed(func(v *servedState) { v.backend, v.byok = "openai", false })
	if s.ServedByok() {
		t.Fatal("a non-BYOK turn must CLEAR ServedByok — the flag is strictly per-turn")
	}
}

// TestServedDisplayRace exercises the real concurrency the normal tests miss: the engine
// goroutine writes the serving telemetry + turn effort mid-turn while the TUI render
// goroutine reads the footer accessors every frame. Run under `go test -race` — it stays
// green only because dispMu guards both sides. (The other tests drive the model
// synchronously and never run this path, which is why a plain `-race` was falsely clean.)
func TestServedDisplayRace(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	s := newSess(st, &captureProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	const iters = 3000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // engine: writes served + turnEffort during a turn
		defer wg.Done()
		for i := 0; i < iters; i++ {
			s.recordServed(func(v *servedState) {
				v.ctxTokens, v.backend, v.model, v.pool, v.ctxWindow = i, "vllm", "kimi-k2", "pool", 64000
			})
			s.setTurnEffort(wire.EffortHigh)
		}
	}()
	go func() { // render: the footer accessors, read every frame
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = s.ContextTokens()
			_ = s.ContextWindow()
			_ = s.ServingModel()
			_ = s.ServedBy()
			_ = s.ThinkingEffort()
		}
	}()
	wg.Wait()
}
