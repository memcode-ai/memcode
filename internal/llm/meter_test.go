package llm

import (
	"context"
	"errors"
	"testing"

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
