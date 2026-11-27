package llm

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"time"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/providers/provcore"
	"github.com/memcode-ai/memcode/internal/wire"
)

// recover.go — the CLI-side RECOVERY EXECUTOR. When a serve fails with an
// infrastructure-class error (the backend 5xx'd or was unavailable, the stream
// stalled, the request died after the transport's own retries), the SAME call
// is retried on the SAME model after a backoff. The model never changes: the
// user pinned it, and quietly moving a turn to another vendor (Gemini 4 Carbon
// to Sonnet on a Claude subscription, 2026-11-27) is exactly what pinning
// forbids. When the retries are spent the error surfaces, naming the model.
//
// Retries fire only when NOTHING was emitted, and never for errors with their
// own policy: billing/entitlement 402s, BYOK key failures, auth/sign-out,
// requests WE malformed (4xx), context overflow (compaction owns it) and
// mid-stream cuts (the loop's same-call retry owns those).

// terminalForFallback reports errors the fallback chain must never touch —
// they carry their own recovery policy elsewhere.
// IsTerminal reports whether an error will fail identically no matter which
// model receives it — billing and auth state, or a request WE malformed.
//
// Callers outside the fallback walk need this too. A delegated worker that dies
// on a terminal error is not a retryable tool failure: handing "agent failed"
// back to the model invites it to try again, and it did — a Gemini
// thought-signature 400 was retried roughly 150 times over 12 minutes, because
// nothing in the loop could tell "this tool had a bad day" from "this will
// never work".
func IsTerminal(err error) bool {
	if err == nil {
		return false
	}
	// NOT an alias for terminalForFallback. That answers "should the fallback
	// walk stop here?", and several of its cases stop the walk precisely BECAUSE
	// something else recovers them: a context overflow compacts and retries, an
	// incomplete stream retries the same call, an exhausted lane raises the
	// user's fallback-choice card. Killing the turn on those would break the
	// recovery instead of surfacing a cause.
	for _, recoverable := range []error{
		wire.ErrContextOverflow,  // compact-and-retry, inside the worker
		wire.ErrStreamIncomplete, // same-call transport retry
	} {
		if errors.Is(err, recoverable) {
			return false
		}
	}
	var exh *provider.ErrLaneExhausted
	if errors.As(err, &exh) {
		return false // raises the fallback-choice card; the user decides
	}
	return terminalForFallback(err)
}

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
	if errors.Is(err, provider.ErrNotLoggedIn) || errors.Is(err, provider.ErrGatewayOnly) {
		return true
	}
	return terminalStatus(err)
}

// terminalStatus classifies a provider's HTTP status. The rule the whole
// fallback chain rests on:
//
//	The pinned model is authoritative. Fallbacks exist to survive
//	INFRASTRUCTURE failure, not to reinterpret the request.
//
// A 4xx that describes OUR request is not an infrastructure failure. Retrying
// it on another model sends the same malformed conversation and fails the same
// way — which is exactly what happened when a Gemini thought-signature 400 was
// walked: one bug became 298 failed calls across two models before anything
// gave up.
//
// Deliberately NOT "all 4xx are terminal": 408 and 429 are timing, not shape,
// and a different model genuinely may serve them.
//
//	400 malformed / invalid argument   -> terminal
//	401 / 403 auth                     -> terminal
//	404 unknown model or endpoint      -> terminal (for this model)
//	408 timeout, 429 rate limit        -> walk the chain
//	5xx, transport, timeouts           -> walk the chain
//	capability gap                     -> refused earlier, never reaches here
func terminalStatus(err error) bool {
	code, _, ok := provcore.APIErrorInfo(err)
	if !ok {
		return false // not an HTTP-shaped error: a transport failure, which walks
	}
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return code >= 400 && code < 500
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

// sameModelRetryDelays are the waits before each extra attempt on the same
// model, after the transport's own retries are spent. Exponential and jittered
// (see retryDelay) because the usual cause is an overloaded upstream, which a
// quick retry only hits again. A var so tests can shorten it.
var sameModelRetryDelays = []time.Duration{10 * time.Second, 30 * time.Second}

// SetSameModelRetryDelays replaces the same-model retry schedule and returns
// a func restoring it. For tests outside this package; production never calls it.
func SetSameModelRetryDelays(d ...time.Duration) (restore func()) {
	old := sameModelRetryDelays
	sameModelRetryDelays = d
	return func() { sameModelRetryDelays = old }
}

// retryDelay is one wait with equal jitter: [d/2, d).
func retryDelay(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)))
}

// runWithRecovery executes one call, retrying the SAME model on
// infrastructure-class failures. call runs one attempt against req.Pin;
// emitted reports whether any output reached the user (streamed text), and
// once it has, nothing is retried.
func (r *Runner) runWithRecovery(ctx context.Context, req wire.Request, res resolved, _ provider.ModelsInfo,
	emitted func() bool, call func(wire.Request) (wire.Response, error)) (wire.Response, error) {

	for attempt := 0; ; attempt++ {
		resp, err := call(req)
		if err == nil {
			r.finalize(&resp, res, "")
			return resp, nil
		}
		if billingClass(err) {
			r.sel.Invalidate() // credits/keys state just changed under us
		}
		if ctx.Err() != nil || terminalForFallback(err) || emitted() || attempt >= len(sameModelRetryDelays) {
			return resp, err
		}
		delay := retryDelay(sameModelRetryDelays[attempt])
		if r.retryNotify != nil {
			r.retryNotify(attempt+1, err, delay)
		}
		select {
		case <-ctx.Done():
			return resp, ctx.Err()
		case <-time.After(delay):
		}
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
