package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

type fakeProv struct {
	resp wire.Response
	err  error
}

func (f fakeProv) Complete(ctx context.Context, req wire.Request) (wire.Response, error) {
	return f.resp, f.err
}

// A call that ERRORS but reported partial usage (a cancelled/failed stream still bills) must
// still be metered — the success-only gate left exactly those expensive failures invisible.
func TestCompleteMetersPartialUsageOnError(t *testing.T) {
	r := NewRunner(fakeProv{
		resp: wire.Response{Model: "m", InputTokens: 100, OutputTokens: 50, Backend: "anthropic"},
		err:  errors.New("stream cut"),
	})
	if _, err := r.Complete(context.Background(), MainLoop, wire.Request{Model: "m"}); err == nil {
		t.Fatal("expected the error to propagate")
	}
	if got := r.Ledger().Total(); got.Calls != 1 || got.In != 100 || got.Out != 50 {
		t.Fatalf("partial usage not metered on error: %+v", got)
	}
}

// A genuine zero-usage error (nothing generated) records nothing.
func TestCompleteNoUsageNoRecord(t *testing.T) {
	r := NewRunner(fakeProv{resp: wire.Response{}, err: errors.New("connect failed")})
	_, _ = r.Complete(context.Background(), MainLoop, wire.Request{Model: "m"})
	if got := r.Ledger().Total(); got.Calls != 0 {
		t.Fatalf("empty error response should not be metered: %+v", got)
	}
}

// Meter records an out-of-band side-channel call (advisor/web tools) into the shared ledger.
func TestMeterRecordsSideChannel(t *testing.T) {
	r := NewRunner(fakeProv{})
	r.Meter("advisor", "claude-sonnet-5", wire.Response{Model: "claude-sonnet-5", InputTokens: 10, OutputTokens: 20}, 5*time.Millisecond)
	got := r.Ledger().Total()
	if got.Calls != 1 || got.In != 10 || got.Out != 20 {
		t.Fatalf("side-channel not metered: %+v", got)
	}
	// A zero-usage side-channel call (e.g. an advisor error before any tokens) is a no-op.
	r.Meter("advisor", "claude-sonnet-5", wire.Response{}, 0)
	if got := r.Ledger().Total(); got.Calls != 1 {
		t.Fatalf("empty side-channel call should not be metered: %+v", got)
	}
}
