package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// (2) An explicit delegated preference is used by EVERY delegated worker, and
// (6) nothing can vary the model per invocation.
//
// spawnAgent lives in the runtime package, so what is provable here is the seam
// it uses: a fork carrying the delegated pin serves that model for every call,
// and the parent keeps serving the primary. The runtime test asserts that
// spawnAgent actually reaches for this seam.
func TestDelegatedForkServesEveryCallAndLeavesPrimaryAlone(t *testing.T) {
	p := &scriptedProv{}
	parent := pinnedRunner(p, prodInfo(nil), "opus")
	delegated := parent.ForkWithModel("haiku")

	// Several delegated calls, of different purposes: all one model.
	for _, purpose := range []Purpose{Agent, Explore, MainLoop} {
		if _, err := delegated.Complete(context.Background(), purpose, userReq("work")); err != nil {
			t.Fatalf("%s: %v", purpose, err)
		}
	}
	for i := 0; i < 3; i++ {
		if p.requested[i] != "haiku" {
			t.Fatalf("delegated call %d ran on %q, want the delegated pin (haiku); all: %v", i, p.requested[i], p.requested)
		}
	}

	// The parent is untouched — the user's own model still serves their turns.
	if _, err := parent.Complete(context.Background(), MainLoop, userReq("mine")); err != nil {
		t.Fatal(err)
	}
	if p.requested[3] != "opus" {
		t.Fatalf("the parent ran on %q, want the primary (opus)", p.requested[3])
	}
	if parent.pin != "opus" {
		t.Fatalf("the parent's pin is now %q — a delegated pin must not leak upward", parent.pin)
	}
}

// (7) A delegated worker's capability gaps and provider failures follow exactly
// the same rules as anyone else's: refuse on a capability gap, and hop only
// along the declared fallback chain on a provider error. A delegated pin is a
// pin — it buys no special routing.
func TestDelegatedWorkerKeepsNormalFailureSemantics(t *testing.T) {
	// Capability gap: refuse, touch no provider, name the fix.
	p := &scriptedProv{}
	req := userReq("look")
	req.Messages[0].Blocks = append(req.Messages[0].Blocks, wire.ImageBlock("image/png", []byte{1}))
	worker := pinnedRunner(p, prodInfo(nil), "opus").ForkWithModel("glm-5p2")
	_, err := worker.Complete(context.Background(), Agent, req)
	if err == nil {
		t.Fatal("a vision-less delegated pin must refuse an image turn")
	}
	if len(p.requested) != 0 {
		t.Fatalf("a refused delegated turn must reach no provider, got %v", p.requested)
	}

	// Provider failure: one hop, along the declared chain, and nowhere else.
	p2 := &scriptedProv{failures: map[string]error{"glm-5p2": errors.New("lane http 500: boom")}}
	worker2 := pinnedRunner(p2, prodInfo(nil), "opus").ForkWithModel("glm-5p2")
	if _, err := worker2.Complete(context.Background(), Agent, userReq("work")); err != nil {
		t.Fatalf("the declared chain must rescue a delegated worker too: %v", err)
	}
	if len(p2.requested) != 2 || p2.requested[1] != "kimi-k2p7-code" {
		t.Fatalf("delegated fallback walk = %v, want [glm-5p2 kimi-k2p7-code]", p2.requested)
	}
}
