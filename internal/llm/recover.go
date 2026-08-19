package llm

import (
	"context"
	"errors"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// recover.go — the CLI-side RECOVERY EXECUTOR: when a serve fails with a
// model-class error (the backend 5xx'd, the lane is overloaded, the request
// died after the transport's own retries), the agent walks the catalog's
// fallback chain and retries the SAME call on the next capable, fundable
// model — Claude Code's client-owned fallback, adopted exactly: the fallback
// applies to the CURRENT call only (the next turn re-selects the primary),
// and it fires only when NOTHING was emitted (Complete is atomic; Stream
// tracks first emission).
//
// Errors with their own policy stay OUT of the chain (the Claude Code line
// between fallback-eligible and terminal classes): billing/entitlement 402s,
// BYOK key failures (never silently retried on credits — the CLI may OFFER an
// explicit consented credits retry via the billing-lane extension),
// auth/sign-out, context overflow (compaction owns it), and mid-stream cuts
// (the loop's same-call retry owns those).

// maxModelFallbacks bounds one call's chain walk.
const maxModelFallbacks = 2

// terminalForFallback reports errors the fallback chain must never touch —
// they carry their own recovery policy elsewhere.
func terminalForFallback(err error) bool {
	for _, sentinel := range []error{
		wire.ErrContextOverflow,    // compact-and-retry (runLoop)
		wire.ErrStreamIncomplete,   // same-call transport retry (runLoop)
		wire.ErrInsufficientCredit, // user action
		wire.ErrSubscriptionRequired,
		wire.ErrAccountLocked,
		wire.ErrByokKeyFailed, // /apikeys, or an explicit consented credits retry — never automatic
		wire.ErrUnauthorized,  // signed out
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	// Lane sentinels carry their own policy: ErrLaneExhausted raises the
	// runtime's fallback-choice card; ErrNoLane is a user-action error. The
	// automatic walk must never touch either.
	var exh *provider.ErrLaneExhausted
	var noLane *provider.ErrNoLane
	if errors.As(err, &exh) || errors.As(err, &noLane) {
		return true
	}
	return errors.Is(err, provider.ErrNotLoggedIn) || errors.Is(err, provider.ErrGatewayOnly)
}

// billingClass reports the errors that mean the org's billing state just
// changed — the control-plane snapshot is stale the moment one arrives.
func billingClass(err error) bool {
	return errors.Is(err, wire.ErrInsufficientCredit) ||
		errors.Is(err, wire.ErrSubscriptionRequired) ||
		errors.Is(err, wire.ErrAccountLocked) ||
		errors.Is(err, wire.ErrByokKeyFailed) ||
		errors.Is(err, wire.ErrUnauthorized)
}

// nextFallback picks the next chain member that can actually take THIS call:
// servable here, capability-compatible with the payload, and fundable under
// the org's credit state. "" ends the walk.
func nextFallback(label string, tried map[string]bool, req wire.Request, info provider.ModelsInfo) string {
	estimate := estimateTokens(req)
	needVision := hasBlock(req, "image")
	needPDF := hasBlock(req, "document")
	keyed := info.ByokVendorSet()
	for _, fb := range catalog.FallbackChain(label) {
		if tried[fb] {
			continue
		}
		f, ok := info.Fact(fb)
		if !ok {
			continue // not servable on this backend
		}
		if needVision && !f.Vision {
			continue
		}
		if needPDF && !f.PDF {
			continue
		}
		if f.Window > 0 && estimate > f.Window {
			continue
		}
		if info.CreditsExhausted && (len(keyed) > 0 || len(info.SubVendors) > 0) &&
			!fundedVendor(f.Vendor, keyed, info) {
			continue // the $0 rule holds mid-recovery too
		}
		return fb
	}
	return ""
}

// runWithRecovery executes one call with the fallback walk. call runs one
// attempt against the label in req.Pin; emitted reports whether any output
// reached the user (streamed text) — once true, recovery stops cold.
func (r *Runner) runWithRecovery(ctx context.Context, req wire.Request, res resolved, info provider.ModelsInfo,
	emitted func() bool, call func(wire.Request) (wire.Response, error)) (wire.Response, error) {

	tried := map[string]bool{res.label: true}
	label := res.label
	reason := ""
	for attempt := 0; ; attempt++ {
		resp, err := call(req)
		if err == nil {
			r.finalize(&resp, res, reason)
			return resp, nil
		}
		if billingClass(err) {
			r.sel.Invalidate() // credits/keys state just changed under us
		}
		if ctx.Err() != nil || terminalForFallback(err) || emitted() || attempt >= maxModelFallbacks {
			return resp, err
		}
		next := nextFallback(label, tried, req, info)
		if next == "" {
			return resp, err
		}
		reason = "model_error: " + clipErr(err)
		tried[next] = true
		label = next
		req.Pin = next
	}
}

// finalize stamps the client-side serving truth the footer/⇄ line read: the
// absorb/fallback reason this policy decided, the requested label, and the
// backend derived from the served label's vendor (the one-wire response
// extension doesn't carry backend — the client knows).
func (r *Runner) finalize(resp *wire.Response, res resolved, fallbackReason string) {
	if resp.FallbackReason == "" {
		if fallbackReason != "" {
			resp.FallbackReason = fallbackReason
		} else if res.reason != "" {
			resp.FallbackReason = res.reason
		}
	}
	if resp.RequestedModel == "" {
		resp.RequestedModel = res.label
	}
	if resp.Backend == "" && resp.Model != "" {
		switch v := catalog.ModelVendor(resp.Model); v {
		case "fireworks":
			resp.Backend = "cheap"
		case "":
			// Uncataloged label (catalog skew between CLI and gateway): tag
			// honestly instead of leaving "" — an empty backend used to fall
			// into the ledger's stale "anthropic" default AND skip the
			// footer's served-state recording entirely.
			resp.Backend = "unknown"
		default:
			resp.Backend = v
		}
	}
}

// clipErr bounds an upstream error message for the ⇄ reason line.
func clipErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
