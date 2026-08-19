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
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Purpose labels WHY a model call was made, so cost/latency can be attributed per
// kind of work (and a future router can learn which purposes Haiku handles well).
type Purpose string

const (
	MainLoop   Purpose = "main_loop"  // the executive turn loop
	Reflect    Purpose = "reflect"    // plan-mode reflection gate
	Review     Purpose = "review"     // cross-model plan critic (a 2nd cheap model reviews the draft)
	Synth      Purpose = "synth"      // plan synthesis
	Explore    Purpose = "explore"    // read-only scout sub-agent
	Agent      Purpose = "agent"      // first-class delegated sub-agent (agent tool: any tier, read-only or mutating)
	Classify   Purpose = "classify"   // cheap routing/safety classifier
	Predict    Purpose = "predict"    // next-step prediction
	Learn      Purpose = "learn"      // claim/knowledge extraction
	Compact    Purpose = "compact"    // context compaction
	Shrinkwrap Purpose = "shrinkwrap" // one-time MEMCODE.md instruction compression (own concern, not compaction)
	Other      Purpose = "other"      // unlabelled — still counted
)

// Stat is an aggregate of usage + estimated cost (total or per-purpose).
type Stat struct {
	Calls                          int
	In, Out, CacheRead, CacheWrite int
	USD                            float64
}

func (s *Stat) add(r wire.Response, usd float64) {
	s.Calls++
	s.In += r.InputTokens
	s.Out += r.OutputTokens
	s.CacheRead += r.CacheReadTokens
	s.CacheWrite += r.CacheWriteTokens
	s.USD += usd
}

// BackendStat aggregates per serving backend (cheap | anthropic | openai |
// …) — where the tokens actually ran and what they cost. USD is the real token
// bill at the served model's rates: every backend is token-billed (the cheap
// lane + frontier APIs; the self-hosted/GPU-hours and counterfactual-savings
// era is gone).
type BackendStat struct {
	Calls      int
	In, Out    int
	CacheRead  int
	CacheWrite int
	USD        float64 // actual total at the served model's rate card
	LatencyMS  int64   // summed wall-clock; divide by Calls for the mean
	Reasons    map[string]int
	// per-component USD (so /cost can PROVE where the money went — input vs
	// cache-write vs cache-read vs output — instead of hand-waving).
	InUSD, OutUSD, CacheReadUSD, CacheWriteUSD float64
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
	// Price what actually ran: an absorb or a fallback hop can serve a different
	// model, and resp.Model/Backend are the serving ground truth.
	model := r.Model
	if model == "" {
		model = reqModel // legacy providers that don't tag — price as requested
	}
	backend := r.Backend
	if backend == "" {
		backend = "unknown" // a test fake / degenerate response — never mis-bill a real vendor
	}

	// Actual token cost. Every backend is priced through the rate card — the
	// cheap lane included (the "$0, GPU-hours" premise died at the 2026-06-12
	// cutover to a hosted token API; pricing the lane at $0 hid a 3-7x
	// underreport of real session burn). ModelPricing matches the gateway's bare
	// sanitized ids ("glm-5p2") by family, so no vendor path is needed here.
	pr := catalog.ModelPricing(model)
	usd := catalog.CostUSD(model, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.total.add(r, usd)
	s := l.byPurpose[p]
	s.add(r, usd)
	l.byPurpose[p] = s

	b := l.byBackend[backend]
	b.Calls++
	b.In += r.InputTokens
	b.Out += r.OutputTokens
	b.CacheRead += r.CacheReadTokens
	b.CacheWrite += r.CacheWriteTokens
	// per-component cost at the served model's rate card (proves the breakdown)
	b.InUSD += float64(r.InputTokens) * pr.Input / 1e6
	b.OutUSD += float64(r.OutputTokens) * pr.Output / 1e6
	b.CacheReadUSD += float64(r.CacheReadTokens) * pr.CacheRead / 1e6
	b.CacheWriteUSD += float64(r.CacheWriteTokens) * pr.CacheWrite / 1e6
	b.USD += usd
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

// Runner is the metered model-policy gateway over a provider.ModelProvider.
// Since the all-policy-client-side migration it is also THE routing authority:
// every call's concrete model is selected here (lane.go semantic ladder +
// resolve.go physical resolution over the /v1/models control plane), and
// failed calls recover here (recover.go fallback walk). The backend — gateway,
// Ollama, anything OpenAI-compatible — just serves what this picks.
type Runner struct {
	prov    provider.ModelProvider
	ledger  *Ledger
	sel     *selection // control-plane snapshot + selection policy (forks share it)
	session string     // this Session's id — stamped onto every request (the wire `user` field)
	vendor  string     // per-session strong-tier vendor flavor ("" / default = steerable Automatic)
	pin     string     // pinned model label from /model ("" = Automatic)
}

// SetSession ties this Runner to a Session id so every call carries it on the wire
// (X-Memcode-Session). Called when the owning Session gets its id; forks set their own.
func (r *Runner) SetSession(id string) { r.session = id }

// SetVendor ties this Runner to a per-session strong-tier vendor the ladder
// resolves tiers within (Intent.Vendor). Called when the Session's vendor
// changes via /model. The empty string means the configured default (BYOK
// steering may prefer a keyed vendor).
func (r *Runner) SetVendor(v string) { r.vendor = v }

// SetPin ties this Runner to a pinned model label the resolver serves every
// real request on (Intent.Pin). "" = Automatic (the ladder decides).
func (r *Runner) SetPin(label string) { r.pin = label }

// NewRunner wraps a provider with a fresh ledger. Construct ONE at the top level
// (the cmd boundary) and thread it everywhere — sub-agents share it.
func NewRunner(prov provider.ModelProvider) *Runner {
	return &Runner{prov: prov, ledger: newLedger(), sel: newSelection()}
}

// Ledger exposes the shared usage record.
func (r *Runner) Ledger() *Ledger { return r.ledger }

// InvalidateModels drops the control-plane snapshot so the next call refetches
// — call after /login, /apikeys mutations, or anything else that changes the
// org's keys/credits/roles. (Billing-class errors invalidate automatically.)
func (r *Runner) InvalidateModels() { r.sel.Invalidate() }

// Fork returns a NEW Runner that shares this one's Ledger, selection state,
// and provider connection but is otherwise its own object. Sub-agents Fork the
// parent's runner instead of sharing the pointer — so what's shared across the
// main loop and its scouts is passive (the Ledger, the control-plane
// snapshot), never an executor with per-context state. The pin is inherited:
// /model pins the whole session, sub-agents included.
func (r *Runner) Fork() *Runner {
	return &Runner{prov: r.prov, ledger: r.ledger, sel: r.sel, pin: r.pin, vendor: r.vendor}
}

// Provider returns the wrapped provider — ONLY for capability assertions that aren't
// model calls (e.g. provider.WebSearcher / WebFetcher detection, doctor checks).
// Never use it to call Complete/Stream directly; that bypasses metering.
func (r *Runner) Provider() provider.ModelProvider { return r.prov }

// hostedPolicy reports whether CLI-side selection drives this provider: true
// for the hosted gateway (a backend that serves exactly what's asked, with
// the /v1/models control plane behind it). False for endpoint mode (the
// session model IS the selection — no roles, no steering) and for test fakes
// (no Endpointer capability), which keep the legacy pass-through stamping.
func (r *Runner) hostedPolicy() bool {
	ep, ok := r.prov.(provider.Endpointer)
	if !ok {
		return false
	}
	_, onEndpoint := ep.Endpoint()
	return !onEndpoint
}

// prepare stamps the per-call wire fields and — on the hosted backend — runs
// the selection policy: intent → lane → concrete label (+ the delegate
// doctrine when the cheap coding lane serves an interactive building mode).
// The chosen label rides req.Pin; the transport puts it in the wire `model`.
func (r *Runner) prepare(ctx context.Context, p Purpose, req *wire.Request) (resolved, provider.ModelsInfo, bool, error) {
	req.Purpose, req.Session = string(p), r.session
	if !r.hostedPolicy() {
		req.Pin = r.pin // endpoint mode / test fakes: session model or raw pass-through
		return resolved{}, provider.ModelsInfo{}, false, nil
	}
	info := r.sel.models(ctx)
	if lz, ok := r.prov.(provider.Laner); ok {
		info = applyLaneFacts(info, lz.Lanes(), lz.GatewayPresent())
	}
	it := wire.Intent{Purpose: string(p), Mode: req.Mode, Reasoning: req.Effort,
		Difficulty: req.Difficulty, Vendor: r.vendor, Pin: r.pin}
	if req.RoutingHint != nil {
		it.Risk = req.RoutingHint.Reason
	}
	res := resolveHosted(it, *req, info)
	if res.err != nil {
		return res, info, true, res.err
	}
	req.Pin = res.label
	scrubForeignThinking(req, res.label)
	// The delegate doctrine: the cheap coding lane, serving an interactive
	// building mode under Automatic, is told to hand non-code work to a
	// strong-tier agent. Appended to the composed STABLE half (cache-safe:
	// static per selected label). Routing-owned prose — it lives with the
	// routing decision, which is HERE now.
	if !res.pinned && (req.Mode == "chat" || req.Mode == "exec") &&
		req.SystemVolatile != "" && catalog.ModelVendor(res.label) == "fireworks" {
		if req.System != "" {
			req.System += "\n\n" + delegateDoctrine
		} else {
			req.System = delegateDoctrine
		}
	}
	return res, info, true, nil
}

// delegateDoctrine tells the cheap coding lane to route non-code work to a
// strong-tier agent rather than answering it directly (the agent tool reports
// the result back). Moved from the gateway with the rest of routing.
const delegateDoctrine = `Match the work to the model. You are the fast coding lane: strong at code, weaker at prose, writing, and open-ended non-code reasoning. When a task is not really about code, such as drafting docs or copy, writing, or general research and reasoning, hand it to a stronger model with the agent tool, agent{task:"…", tier:"strong"}, and use what it returns instead of doing it yourself. Keep the code and repo work for yourself.`

// Complete runs a non-streamed call — selection, the recovery walk, and
// metering (usage, cost, latency, backend).
func (r *Runner) Complete(ctx context.Context, p Purpose, req wire.Request) (wire.Response, error) {
	res, info, active, err := r.prepare(ctx, p, &req)
	if err != nil {
		return wire.Response{}, err
	}
	start := time.Now()
	if !active {
		resp, err := r.prov.Complete(ctx, req)
		if err == nil {
			r.ledger.record(req.Model, p, resp, time.Since(start))
		}
		return resp, err
	}
	resp, err := r.runWithRecovery(ctx, req, res, info,
		func() bool { return false }, // Complete is atomic: a failed call emitted nothing
		func(rq wire.Request) (wire.Response, error) { return r.prov.Complete(ctx, rq) })
	if err == nil {
		r.ledger.record(req.Model, p, resp, time.Since(start))
	}
	return resp, err
}

// Stream runs a streamed call (live deltas via h) with the same selection +
// recovery; the emitted-output guard stops the fallback walk the moment any
// delta reached the user. Falls back to Complete if the provider can't stream.
func (r *Runner) Stream(ctx context.Context, p Purpose, req wire.Request, h wire.StreamHandler) (wire.Response, error) {
	s, ok := r.prov.(provider.Streamer)
	if !ok {
		return r.Complete(ctx, p, req)
	}
	res, info, active, err := r.prepare(ctx, p, &req)
	if err != nil {
		return wire.Response{}, err
	}
	start := time.Now()
	if !active {
		resp, err := s.Stream(ctx, req, h)
		if err == nil {
			r.ledger.record(req.Model, p, resp, time.Since(start))
		}
		return resp, err
	}
	var emitted bool
	wrapped := h
	if h.Text != nil {
		orig := h.Text
		wrapped.Text = func(d string) { emitted = true; orig(d) }
	}
	resp, err := r.runWithRecovery(ctx, req, res, info,
		func() bool { return emitted },
		func(rq wire.Request) (wire.Response, error) { return s.Stream(ctx, rq, wrapped) })
	if err == nil {
		r.ledger.record(req.Model, p, resp, time.Since(start))
	}
	return resp, err
}
