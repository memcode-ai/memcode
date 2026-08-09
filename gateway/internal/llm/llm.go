// Package llm is the metered model-execution gateway: the ONE path every model
// call goes through. Calling a model directly via provider.Complete/Stream is
// banned (a guard test enforces it) — instead callers hold a *Runner and pass a
// Purpose, so token/cost accounting (and, later, latency, route metadata, cost
// guardrails, and learning metrics) is automatic and impossible to bypass.
//
// Architecture: provider (wire) → Runner (meters every call) → Ledger (one shared
// record) → /cost, /route, guardrails, learning. One Runner per top-level session;
// sub-agents share it, so their spend lands in the same Ledger with no manual
// rollup.
package llm

import (
	"context"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/gateway/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Purpose labels WHY a model call was made, so cost/latency can be attributed per
// kind of work (and a future router can learn which purposes Haiku handles well).
type Purpose string

const (
	MainLoop Purpose = "main_loop" // the executive turn loop
	Reflect  Purpose = "reflect"   // plan-mode reflection gate
	Synth    Purpose = "synth"     // plan synthesis
	Explore  Purpose = "explore"   // read-only scout sub-agent
	Overview Purpose = "overview"  // current-state synthesis
	Classify Purpose = "classify"  // cheap routing/safety classifier
	Predict  Purpose = "predict"   // next-step prediction
	Learn    Purpose = "learn"     // claim/knowledge extraction
	Route    Purpose = "route"     // turn router (future)
	Compact  Purpose = "compact"   // context compaction (future)
	Other    Purpose = "other"     // unlabelled — still counted
)

// Stat is an aggregate of usage + estimated cost (total or per-purpose).
type Stat struct {
	Calls                          int
	In, Out, CacheRead, CacheWrite int
	USD                            float64
}

func (s *Stat) add(model string, p Purpose, r wire.Response) {
	s.Calls++
	s.In += r.InputTokens
	s.Out += r.OutputTokens
	s.CacheRead += r.CacheReadTokens
	s.CacheWrite += r.CacheWriteTokens
	s.USD += catalog.CostUSD(model, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens)
}

// BackendStat aggregates per serving backend (fireworks | anthropic | openai |
// …) — where the tokens actually ran and what they cost at the served model's
// rate card. Every lane is token-billed (Fireworks + frontier APIs; the
// self-hosted/GPU-hours era is gone).
type BackendStat struct {
	Calls     int
	In, Out   int
	USD       float64        // actual at the served model's rate card
	LatencyMS int64          // summed wall-clock; divide by Calls for the mean
	Reasons   map[string]int // fallback reasons (cheap_lane_error/cheap_lane_overflow/…)
}

// Ledger is the single, concurrency-safe record of all model usage for a run.
// Shared across a session and its sub-agents.
type Ledger struct {
	mu        sync.Mutex
	total     Stat
	byPurpose map[Purpose]Stat
	byBackend map[string]BackendStat
}

func newLedger() *Ledger {
	return &Ledger{byPurpose: map[Purpose]Stat{}, byBackend: map[string]BackendStat{}}
}

func (l *Ledger) record(reqModel string, p Purpose, r wire.Response, latency time.Duration) {
	if p == "" {
		p = Other
	}
	// Price what actually ran: the router may have re-targeted the call at the
	// self-hosted pool, and resp.Model/Backend are the provider's ground truth.
	model := r.Model
	if model == "" {
		model = reqModel // legacy providers that don't tag — price as requested
	}
	backend := r.Backend
	if backend == "" {
		backend = "anthropic"
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.total.add(model, p, r)
	s := l.byPurpose[p]
	s.add(model, p, r)
	l.byPurpose[p] = s

	b := l.byBackend[backend]
	b.Calls++
	b.In += r.InputTokens + r.CacheReadTokens
	b.Out += r.OutputTokens
	b.USD += catalog.CostUSD(model, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens)
	b.LatencyMS += latency.Milliseconds()
	if r.FallbackReason != "" {
		if b.Reasons == nil {
			b.Reasons = map[string]int{}
		}
		b.Reasons[r.FallbackReason]++
	}
	l.byBackend[backend] = b
}

// Total returns the aggregate usage + cost across every metered call.
func (l *Ledger) Total() Stat {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// ByPurpose returns a snapshot of per-purpose usage (for /cost --by-purpose, /route,
// and learning).
func (l *Ledger) ByPurpose() map[Purpose]Stat {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[Purpose]Stat, len(l.byPurpose))
	for k, v := range l.byPurpose {
		out[k] = v
	}
	return out
}

// ByBackend returns a snapshot of per-backend usage — who actually served the
// session's calls, at what latency, and what the per-token bill would have been
// (for /cost backends and the GPU-economics decision).
func (l *Ledger) ByBackend() map[string]BackendStat {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]BackendStat, len(l.byBackend))
	for k, v := range l.byBackend {
		// clone the reasons map so callers can't race the ledger's copy
		if v.Reasons != nil {
			r := make(map[string]int, len(v.Reasons))
			for rk, rv := range v.Reasons {
				r[rk] = rv
			}
			v.Reasons = r
		}
		out[k] = v
	}
	return out
}

// ModelRunner is the ONLY interface a model consumer should depend on. Every call
// carries a Purpose and is metered. (Consumers take this, never provider.ModelProvider.)
type ModelRunner interface {
	Complete(ctx context.Context, p Purpose, req wire.Request) (wire.Response, error)
	Stream(ctx context.Context, p Purpose, req wire.Request, h wire.StreamHandler) (wire.Response, error)
	Ledger() *Ledger
}

// Runner is the metered gateway over a provider.ModelProvider.
type Runner struct {
	prov   provider.ModelProvider
	ledger *Ledger
}

// NewRunner wraps a provider with a fresh ledger. Construct ONE at the top level
// (the cmd boundary) and thread it everywhere — sub-agents share it.
func NewRunner(prov provider.ModelProvider) *Runner {
	return &Runner{prov: prov, ledger: newLedger()}
}

// Ledger exposes the shared usage record.
func (r *Runner) Ledger() *Ledger { return r.ledger }

// Fork returns a NEW Runner that shares this one's Ledger (and provider connection)
// but is otherwise its own object. Sub-agents Fork the parent's runner instead of
// sharing the pointer — so the only thing shared across the main loop and its
// scouts is the passive central Ledger, never an executor with per-context state.
func (r *Runner) Fork() *Runner { return &Runner{prov: r.prov, ledger: r.ledger} }

// Provider returns the wrapped provider — ONLY for capability assertions that aren't
// model calls (e.g. provider.WebSearcher / WebFetcher detection, doctor checks).
// Never use it to call Complete/Stream directly; that bypasses metering.
func (r *Runner) Provider() provider.ModelProvider { return r.prov }

// hasUsage reports whether a response carries any billed tokens — the signal to meter it
// EVEN on error. A stream cut or a client cancel mid-generation still bills the vendor for the
// tokens produced so far; recording only on the success path (the old behavior) left exactly
// those expensive failures invisible in /status and the org bill.
func hasUsage(r wire.Response) bool {
	return r.InputTokens > 0 || r.OutputTokens > 0 || r.CacheReadTokens > 0 || r.CacheWriteTokens > 0
}

// Complete runs a non-streamed call and meters it (usage, cost, latency, backend).
func (r *Runner) Complete(ctx context.Context, p Purpose, req wire.Request) (wire.Response, error) {
	start := time.Now()
	resp, err := r.prov.Complete(ctx, req)
	// Meter whenever the provider reported usage, success OR error — see hasUsage.
	if hasUsage(resp) {
		r.ledger.record(req.Model, p, resp, time.Since(start))
	}
	return resp, err
}

// Stream runs a streamed call (live deltas via h) and meters the final usage. Falls
// back to Complete if the underlying provider can't stream.
func (r *Runner) Stream(ctx context.Context, p Purpose, req wire.Request, h wire.StreamHandler) (wire.Response, error) {
	s, ok := r.prov.(provider.Streamer)
	if !ok {
		return r.Complete(ctx, p, req)
	}
	start := time.Now()
	resp, err := s.Stream(ctx, req, h)
	if hasUsage(resp) {
		r.ledger.record(req.Model, p, resp, time.Since(start))
	}
	return resp, err
}

// Meter records usage for a call made OUTSIDE Complete/Stream — a side-channel like the
// advisor or the web tools — so its spend lands in the same shared Ledger. reqModel is the
// counterfactual basis (usually the same as the served model for a side-channel).
func (r *Runner) Meter(p Purpose, reqModel string, resp wire.Response, latency time.Duration) {
	if hasUsage(resp) {
		r.ledger.record(reqModel, p, resp, latency)
	}
}
