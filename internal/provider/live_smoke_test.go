package provider_test

import (
	"context"
	"fmt"
	"github.com/memcode-ai/memcode/catalog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/provider"
	compat "github.com/memcode-ai/memcode/internal/providers/compat"
	"github.com/memcode-ai/memcode/internal/wire"
)

// TestLiveGatewayCompatSmoke runs ONE real streamed turn against the deployed
// gateway (the one wire at /v1, a concrete label — all-policy-client-side).
// Env-gated so ordinary runs skip: MEMCODE_API_TOKEN and MEMCODE_LIVE_SMOKE=1.
// Asserts the three things the wire promises the runtime: assistant text,
// usage counts, and the memcode extension object on the final chunk.
func TestLiveGatewayCompatSmoke(t *testing.T) {
	token := os.Getenv("MEMCODE_API_TOKEN")
	if token == "" || os.Getenv("MEMCODE_LIVE_SMOKE") == "" {
		t.Skip("live smoke skipped: set MEMCODE_API_TOKEN and MEMCODE_LIVE_SMOKE=1 to run")
	}
	base := os.Getenv("MEMCODE_API_URL")
	if base == "" {
		base = "https://code.memcode.ai"
	}
	tr := compat.New(compat.Config{BaseURL: strings.TrimRight(base, "/") + "/v1", Token: token, Memcode: true})

	// Pick a FUNDABLE label the way the selection policy would: at $0 with
	// BYOK keys, a byok-covered model (zero-debit); else the standard role.
	label := "glm-5p2"
	mctx, mcancel := context.WithTimeout(context.Background(), 15*time.Second)
	info, ierr := provider.FetchModels(mctx)
	mcancel()
	if ierr != nil {
		t.Fatalf("control plane unavailable: %v", ierr)
	}
	// The smoke test used to serve whatever played the "standard" ROLE. Roles
	// are gone; probe the catalog's utility model, which is always servable.
	if u := catalog.UtilityModel(); u != "" {
		label = u
	}
	if info.CreditsExhausted {
		found := ""
		for _, m := range info.Models {
			if m.Byok {
				found = m.Label
				break
			}
		}
		if found == "" {
			t.Skip("live smoke: wallet empty and no BYOK key — nothing fundable to serve on")
		}
		label = found
	}
	t.Logf("smoke label: %s (credits_exhausted=%v)", label, info.CreditsExhausted)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var deltas int
	resp, err := tr.Stream(ctx, wire.Request{
		Session:   fmt.Sprintf("one-wire-smoke-%d", time.Now().UnixNano()),
		Pin:       label, // the agent names a concrete, fundable label — no server-side Automatic
		MaxTokens: 256,
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{
			wire.TextBlock("Reply with exactly one word: pong"),
		}}},
	}, wire.StreamHandler{Text: func(string) { deltas++ }})
	if err != nil {
		t.Fatalf("live compat turn failed: %v", err)
	}
	if resp.Text() == "" {
		t.Error("live turn returned no assistant text")
	}
	if deltas == 0 {
		t.Error("no streamed text deltas arrived")
	}
	if resp.InputTokens+resp.CacheReadTokens == 0 || resp.OutputTokens == 0 {
		t.Errorf("usage missing: in=%d cr=%d out=%d", resp.InputTokens, resp.CacheReadTokens, resp.OutputTokens)
	}
	if resp.ContextWindow == 0 && resp.InputBudget == 0 && resp.Pool == "" {
		t.Error("memcode extension object absent from the final chunk")
	}
	t.Logf("live smoke ok: model=%s text=%q deltas=%d in=%d cr=%d out=%d window=%d budget=%d pool=%q byok=%v",
		resp.Model, resp.Text(), deltas, resp.InputTokens, resp.CacheReadTokens, resp.OutputTokens,
		resp.ContextWindow, resp.InputBudget, resp.Pool, resp.BYOK)
}
