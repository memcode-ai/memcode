package provider

// The real-doctrine compose integration: legacy-shaped side calls (compact/
// distill/…) compose through the CLI's doctrine renderer via the engine's
// Compose hook, byte-identical to the render path.

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/doctrine"
	"github.com/memcode-ai/memcode/internal/wire"
)

func TestDoctrineComposeHook(t *testing.T) {
	r, err := composeDoctrine(wire.Request{Mode: "compact",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("transcript")}}}})
	if err != nil {
		t.Fatal(err)
	}
	stable, _, err := doctrine.Compose("compact", nil, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.System != stable {
		t.Error("stable half must byte-match doctrine.Compose for the mode")
	}
	if r.SystemVolatile == "" || r.Facts != nil {
		t.Errorf("volatile must be set and facts cleared: %q %v", r.SystemVolatile, r.Facts)
	}
	// Unknown mode surfaces the compose error.
	if _, err := composeDoctrine(wire.Request{Mode: "bogus"}); err == nil {
		t.Error("unknown mode must fail loudly")
	}
}
