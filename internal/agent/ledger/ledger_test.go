package ledger

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/llm"
)

// fakeReader is a test stand-in for *llm.Ledger. It returns hardcoded values so the
// pure aggregation functions (SpendByBackend, SpendByPurpose, CacheStats, Tokens) can
// be tested without a DB, a provider, or a real model call. Implements ledger.Reader.
type fakeReader struct {
	total     llm.Stat
	byBackend map[string]llm.BackendStat
	byPurpose map[llm.Purpose]llm.Stat
}

func (f *fakeReader) Total() llm.Stat                       { return f.total }
func (f *fakeReader) ByBackend() map[string]llm.BackendStat { return f.byBackend }
func (f *fakeReader) ByPurpose() map[llm.Purpose]llm.Stat   { return f.byPurpose }

// TestSpendByBackend verifies the aggregation: per-backend stats are turned into
// BackendSpend structs, sorted by calls (busiest first), with AvgLatencyMS computed
// as total-latency / calls.
func TestSpendByBackend(t *testing.T) {
	r := &fakeReader{
		byBackend: map[string]llm.BackendStat{
			"anthropic": {
				Calls:     5,
				In:        1000,
				Out:       200,
				CacheRead: 500,
				LatencyMS: 10000, // 5 calls → avg 2000
				USD:       0.15,
			},
			"cheap": {
				Calls:     2,
				In:        400,
				Out:       80,
				LatencyMS: 4000, // 2 calls → avg 2000
				USD:       0.02, // the cheap lane is token-billed — real money
			},
		},
	}

	got := SpendByBackend(r)
	if len(got) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(got))
	}
	// Sorted by calls desc: anthropic (5) first, cheap (2) second.
	if got[0].Backend != "anthropic" {
		t.Errorf("expected anthropic first (5 calls), got %q (%d calls)", got[0].Backend, got[0].Calls)
	}
	if got[1].Backend != "cheap" {
		t.Errorf("expected cheap second (2 calls), got %q (%d calls)", got[1].Backend, got[1].Calls)
	}
	// AvgLatencyMS = total / calls.
	if got[0].AvgLatencyMS != 2000 {
		t.Errorf("anthropic AvgLatencyMS = %d, want 2000", got[0].AvgLatencyMS)
	}
	if got[1].AvgLatencyMS != 2000 {
		t.Errorf("cheap AvgLatencyMS = %d, want 2000", got[1].AvgLatencyMS)
	}
	// Token fields pass through.
	if got[0].In != 1000 || got[0].Out != 200 || got[0].CacheRead != 500 {
		t.Errorf("anthropic token fields wrong: in=%d out=%d cacheRead=%d", got[0].In, got[0].Out, got[0].CacheRead)
	}
	if got[1].USD != 0.02 {
		t.Errorf("cheap USD = %v, want 0.02 (cheap-lane spend passes through)", got[1].USD)
	}
}

// TestSpendByBackendZeroCalls verifies AvgLatencyMS is 0 (not a divide-by-zero
// panic) when a backend has zero calls.
func TestSpendByBackendZeroCalls(t *testing.T) {
	r := &fakeReader{
		byBackend: map[string]llm.BackendStat{
			"empty": {Calls: 0, LatencyMS: 0},
		},
	}
	got := SpendByBackend(r)
	if len(got) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(got))
	}
	if got[0].AvgLatencyMS != 0 {
		t.Errorf("zero-calls AvgLatencyMS = %d, want 0 (no divide-by-zero)", got[0].AvgLatencyMS)
	}
}

// TestSpendByPurpose verifies per-purpose aggregation: stats are turned into
// PurposeSpend structs, sorted by USD descending (most expensive first).
func TestSpendByPurpose(t *testing.T) {
	r := &fakeReader{
		byPurpose: map[llm.Purpose]llm.Stat{
			llm.MainLoop: {Calls: 10, In: 5000, Out: 1000, USD: 0.50},
			llm.Explore:  {Calls: 5, In: 2000, Out: 200, USD: 0.10},
			llm.Review:   {Calls: 2, In: 800, Out: 100, USD: 0.05},
		},
	}

	got := SpendByPurpose(r)
	if len(got) != 3 {
		t.Fatalf("expected 3 purposes, got %d", len(got))
	}
	// Sorted by USD desc: main_loop (0.50) > explore (0.10) > review (0.05).
	if got[0].Purpose != string(llm.MainLoop) {
		t.Errorf("expected main_loop first (highest USD), got %q", got[0].Purpose)
	}
	if got[1].Purpose != string(llm.Explore) {
		t.Errorf("expected explore second, got %q", got[1].Purpose)
	}
	if got[2].Purpose != string(llm.Review) {
		t.Errorf("expected review third, got %q", got[2].Purpose)
	}
	// Fields pass through.
	if got[0].Calls != 10 || got[0].In != 5000 || got[0].USD != 0.50 {
		t.Errorf("main_loop fields wrong: calls=%d in=%d usd=%v", got[0].Calls, got[0].In, got[0].USD)
	}
}

// TestCacheStats verifies the cumulative prompt-cache token extraction.
func TestCacheStats(t *testing.T) {
	r := &fakeReader{
		total: llm.Stat{CacheRead: 12000, CacheWrite: 3000},
	}
	read, write := CacheStats(r)
	if read != 12000 {
		t.Errorf("cache read = %d, want 12000", read)
	}
	if write != 3000 {
		t.Errorf("cache write = %d, want 3000", write)
	}
}

// TestTokens verifies the total token flow: input includes cache reads (total input
// the model saw), output is straight from Total().
func TestTokens(t *testing.T) {
	r := &fakeReader{
		total: llm.Stat{In: 8000, Out: 1500, CacheRead: 4000, CacheWrite: 1000},
	}
	in, out := Tokens(r)
	// in = In + CacheRead (total tokens the model saw as input).
	if in != 12000 {
		t.Errorf("total in = %d, want 12000 (In 8000 + CacheRead 4000)", in)
	}
	if out != 1500 {
		t.Errorf("total out = %d, want 1500", out)
	}
}

// TestSpend verifies the Spend helper returns the raw total fields.
func TestSpend(t *testing.T) {
	r := &fakeReader{
		total: llm.Stat{In: 100, Out: 50, CacheRead: 20, CacheWrite: 10, USD: 0.42},
	}
	in, out, cr, cw, usd := Spend(r)
	if in != 100 || out != 50 || cr != 20 || cw != 10 || usd != 0.42 {
		t.Errorf("Spend = (in=%d out=%d cr=%d cw=%d usd=%v), want (100, 50, 20, 10, 0.42)", in, out, cr, cw, usd)
	}
}
