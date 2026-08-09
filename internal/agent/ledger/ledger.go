// Package ledger holds the cost-accounting readout types and pure aggregation
// logic carved off the agent Session god-object. The Session holds a *llm.Ledger
// (the metered gateway's shared record); this package defines the display types
// (BackendSpend, PurposeSpend) and the functions that turn ledger snapshots into
// sorted, display-ready slices. The functions take a Reader interface (satisfied
// by *llm.Ledger) so the ledger package depends only on llm, not on runtime.
package ledger

import (
	"sort"

	"github.com/memcode-ai/memcode/internal/llm"
)

// Reader is the subset of *llm.Ledger the aggregation functions need. *llm.Ledger
// satisfies it; tests can substitute a fake. The ledger package depends on llm
// for Stat/BackendStat/Purpose, but never on runtime — so the import direction
// stays runtime → ledger → llm (a clean leaf below the hub).
type Reader interface {
	Total() llm.Stat
	ByBackend() map[string]llm.BackendStat
	ByPurpose() map[llm.Purpose]llm.Stat
}

// BackendSpend is one serving backend's slice of the session ledger (for /cost):
// who actually ran the calls, how fast, and what it cost at the served model's
// rate card (every backend is token-billed — the cheap lane + frontier APIs).
type BackendSpend struct {
	Backend                                    string
	Calls                                      int
	In, Out, CacheRead, CacheWrite             int
	USD                                        float64
	InUSD, OutUSD, CacheReadUSD, CacheWriteUSD float64
	AvgLatencyMS                               int64
	Reasons                                    map[string]int
}

// PurposeSpend is one purpose's slice of the session ledger (for /cost --by-purpose).
type PurposeSpend struct {
	Purpose                        string
	Calls                          int
	In, Out, CacheRead, CacheWrite int
	USD                            float64
}

// CacheStats returns cumulative prompt-cache token counts (read ≈ 10% cost; write =
// tokens written to the cache). Shown under /debug.
func CacheStats(r Reader) (read, write int) {
	t := r.Total()
	return t.CacheRead, t.CacheWrite
}

// Tokens returns cumulative session token flow: total input the model saw (uncached
// + cache reads) and total output. Footer "↑in ↓out".
func Tokens(r Reader) (in, out int) {
	t := r.Total()
	return t.In + t.CacheRead, t.Out
}

// Spend returns the session's token breakdown and estimated cost in USD (priced per
// response under each call's model; rates are approximate). Powers /cost.
func Spend(r Reader) (in, out, cacheRead, cacheWrite int, usd float64) {
	t := r.Total()
	return t.In, t.Out, t.CacheRead, t.CacheWrite, t.USD
}

// SpendByBackend returns per-backend usage, busiest first. One entry ("anthropic")
// in a classic session; two once the hybrid router is live.
func SpendByBackend(r Reader) []BackendSpend {
	bb := r.ByBackend()
	out := make([]BackendSpend, 0, len(bb))
	for b, st := range bb {
		avg := int64(0)
		if st.Calls > 0 {
			avg = st.LatencyMS / int64(st.Calls)
		}
		out = append(out, BackendSpend{
			Backend: b, Calls: st.Calls, In: st.In, Out: st.Out,
			CacheRead: st.CacheRead, CacheWrite: st.CacheWrite,
			USD:   st.USD,
			InUSD: st.InUSD, OutUSD: st.OutUSD, CacheReadUSD: st.CacheReadUSD, CacheWriteUSD: st.CacheWriteUSD,
			AvgLatencyMS: avg, Reasons: st.Reasons,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Calls > out[j].Calls })
	return out
}

// SpendByPurpose returns per-purpose usage, most expensive first — so /cost can show
// where the money actually went (e.g. explore scouts vs the main loop vs synthesis).
func SpendByPurpose(r Reader) []PurposeSpend {
	bp := r.ByPurpose()
	out := make([]PurposeSpend, 0, len(bp))
	for p, st := range bp {
		out = append(out, PurposeSpend{string(p), st.Calls, st.In, st.Out, st.CacheRead, st.CacheWrite, st.USD})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].USD > out[j].USD })
	return out
}
