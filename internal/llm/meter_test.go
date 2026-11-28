package llm

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// partialUsageProv fails every call but returns the partial usage the vendor
// already billed before the cut — the shape all three native adapters produce
// on a cancelled or mid-stream-failed turn.
type partialUsageProv struct{ withUsage bool }

func (p *partialUsageProv) Complete(context.Context, wire.Request) (wire.Response, error) {
	resp := wire.Response{Model: "sonnet", Backend: "anthropic"}
	if p.withUsage {
		resp.InputTokens, resp.OutputTokens = 500, 120
		resp.CacheReadTokens = 30
	}
	return resp, errors.New("anthropic stream: connection reset")
}

func (p *partialUsageProv) Stream(ctx context.Context, r wire.Request, _ wire.StreamHandler) (wire.Response, error) {
	return p.Complete(ctx, r)
}

func (p *partialUsageProv) Endpoint() (provider.Endpoint, bool) {
	return provider.Endpoint{Name: "anthropic", BaseURL: "https://api.anthropic.com", Model: "sonnet"}, true
}

// A failed call whose response carries billed partial usage must still be
// metered — the vendor charged for those tokens, so /cost and the ledger must
// see them (previously the ledger recorded only on err == nil).
func TestPartialUsageOnErrorIsMetered(t *testing.T) {
	p := &partialUsageProv{withUsage: true}
	r := NewRunner(p)

	if _, err := r.Complete(context.Background(), MainLoop, userReq("hi")); err == nil {
		t.Fatal("want the provider error to surface")
	}
	total := r.Ledger().Total()
	if total.Calls != 1 || total.In != 500 || total.Out != 120 || total.CacheRead != 30 {
		t.Fatalf("failed call with usage not metered: %+v", total)
	}

	if _, err := r.Stream(context.Background(), MainLoop, userReq("hi"), wire.StreamHandler{}); err == nil {
		t.Fatal("want the provider error to surface")
	}
	if total = r.Ledger().Total(); total.Calls != 2 || total.In != 1000 {
		t.Fatalf("failed stream with usage not metered: %+v", total)
	}
}

// A failed call with NO usage (nothing billed — e.g. a local config error)
// must not pollute the ledger with an empty record.
func TestUsagelessErrorIsNotMetered(t *testing.T) {
	p := &partialUsageProv{withUsage: false}
	r := NewRunner(p)
	if _, err := r.Complete(context.Background(), MainLoop, userReq("hi")); err == nil {
		t.Fatal("want the provider error to surface")
	}
	if total := r.Ledger().Total(); total.Calls != 0 {
		t.Fatalf("usageless failure must not be metered: %+v", total)
	}
}

func TestProviderCacheUtilizationAndPricing(t *testing.T) {
	l := newLedger()
	if got := l.Total().CacheUtilization(); got != 0 {
		t.Fatalf("zero-token CacheUtilization = %v, want 0", got)
	}

	// 1. Anthropic cold cache: 100 uncached + 10,000 cache write, 0 cache read.
	cold := wire.Response{
		Model:            catalog.ModelSonnet,
		Backend:          "anthropic",
		InputTokens:      100,
		CacheWriteTokens: 10000,
		OutputTokens:     50,
	}
	if cold.TotalInput() != 10100 || cold.CacheUtilization() != 0 {
		t.Fatalf("cold resp: TotalInput=%d util=%v", cold.TotalInput(), cold.CacheUtilization())
	}
	l.record(catalog.ModelSonnet, MainLoop, cold, time.Millisecond)

	// 2. Anthropic warm cache: 12 uncached + 15,693 write + 93,571 read.
	warm := wire.Response{
		Model:            catalog.ModelSonnet,
		Backend:          "anthropic",
		InputTokens:      12,
		CacheWriteTokens: 15693,
		CacheReadTokens:  93571,
		OutputTokens:     100,
	}
	if warm.TotalInput() != 109276 || math.Abs(warm.CacheUtilization()-85.6281) > 0.01 {
		t.Fatalf("warm resp: TotalInput=%d util=%.4f", warm.TotalInput(), warm.CacheUtilization())
	}
	l.record(catalog.ModelSonnet, MainLoop, warm, time.Millisecond)

	// 3. Gemini: 1,000 uncached + 9,000 cache read (no cache write).
	gem := wire.Response{
		Model:           catalog.ModelGeminiFlash,
		Backend:         "gemini",
		InputTokens:     1000,
		CacheReadTokens: 9000,
		OutputTokens:    200,
	}
	if gem.TotalInput() != 10000 || math.Abs(gem.CacheUtilization()-90.0) > 1e-6 {
		t.Fatalf("gemini resp: TotalInput=%d util=%.4f", gem.TotalInput(), gem.CacheUtilization())
	}
	l.record(catalog.ModelGeminiFlash, MainLoop, gem, time.Millisecond)

	// 4. OpenAI: 2,000 uncached + 8,000 cache read (no cache write).
	oai := wire.Response{
		Model:           "gpt-6.1-terra",
		Backend:         "openai",
		InputTokens:     2000,
		CacheReadTokens: 8000,
		OutputTokens:    150,
	}
	if oai.TotalInput() != 10000 || math.Abs(oai.CacheUtilization()-80.0) > 1e-6 {
		t.Fatalf("openai resp: TotalInput=%d util=%.4f", oai.TotalInput(), oai.CacheUtilization())
	}
	l.record("gpt-6.1-terra", MainLoop, oai, time.Millisecond)

	// Verify mixed-provider cumulative totals and per-provider cache utilization & pricing.
	tot := l.Total()
	wantIn := 10100 + 109276 + 10000 + 10000
	if tot.TotalInput() != wantIn {
		t.Fatalf("mixed TotalInput = %d, want %d", tot.TotalInput(), wantIn)
	}
	wantUtil := float64(93571+9000+8000) * 100 / float64(wantIn)
	if math.Abs(tot.CacheUtilization()-wantUtil) > 1e-6 {
		t.Fatalf("mixed CacheUtilization = %.4f, want %.4f", tot.CacheUtilization(), wantUtil)
	}

	byB := l.ByBackend()
	anth := byB["anthropic"]
	if anth.CacheWriteUSD <= 0 || anth.CacheReadUSD <= 0 {
		t.Fatalf("anthropic missing cache pricing: %+v", anth)
	}
	if byB["gemini"].CacheWriteUSD != 0 || byB["gemini"].CacheReadUSD <= 0 {
		t.Fatalf("gemini cache pricing unexpected: %+v", byB["gemini"])
	}
	if byB["openai"].CacheWriteUSD != 0 || byB["openai"].CacheReadUSD <= 0 {
		t.Fatalf("openai cache pricing unexpected: %+v", byB["openai"])
	}
}
