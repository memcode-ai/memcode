package provider

// BYOK-first routing: when the authenticated user has their OWN key for the
// vendor a turn is about to be served on, that key is used INSTEAD of the
// gateway's — and if anything about the user's key fails (resolution, or the
// vendor rejecting/erroring the call), the TURN FAILS with an actionable
// message. There is deliberately NO fallback onto memcode's keys for a failing
// BYOK key (decided 2026-07-24): the user asked for their key to be used, and
// silently spending memcode credits instead would betray that. Fallback to
// memcode keys happens only for vendors the user has no key for at all — the
// presence gate (identity.KeyedVendors) decides, which also keeps the
// zero-BYOK path byte-identical to the pre-BYOK gateway.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/gateway"
	"github.com/memcode-ai/memcode/gateway/internal/identity"
	"github.com/memcode-ai/memcode/internal/wire"
)

// ByokKeys is the key-resolution seam the Hybrid router consults, implemented
// by the composition's KeySource (a hosted vault + TTL caches). Key returns
// the live key and a stable version tag (cache key material); ErrKeyInvalid
// means a recent call proved the key bad — fail fast with the same message
// instead of re-burning a vendor call every turn.
type ByokKeys interface {
	Key(ctx context.Context, org, user, vendor string) (key, version string, err error)
	MarkInvalid(org, user, vendor string)
}

// ErrKeyInvalid re-exports the shared sentinel: a KeySource.Key rejection
// meaning the key was recently proven bad (fast-fail).
var ErrKeyInvalid = gateway.ErrKeyInvalid

// CreditsExhaustedError is a turn-fatal refusal: serving is limited to keyed
// the request was admitted only because the user has BYOK keys — but THIS
// lane would serve on memcode's keys. The server maps it to a 402 with the
// insufficient_credits code. No vendor call happened.
type CreditsExhaustedError struct{ Vendor string }

func (e *CreditsExhaustedError) Error() string {
	return fmt.Sprintf("credits exhausted — this turn routes to %s, which your API keys don't cover; add credits at memcode.ai/account/billing or add a %s key with /apikeys", e.Vendor, e.Vendor)
}

// AsCreditsExhausted unwraps a turn error to its credits refusal, if that's
// what it is.
func AsCreditsExhausted(err error) *CreditsExhaustedError {
	var ce *CreditsExhaustedError
	if errors.As(err, &ce) {
		return ce
	}
	return nil
}

// requireCredits gates a lane about to serve on MEMCODE's keys: with the
// credits-exhausted flag set on the identity (balance <= 0), the lane refuses
// instead of spending money the org doesn't have. BYOK lanes never call this.
func requireCredits(ctx context.Context, vendor string) error {
	if identity.From(ctx).LimitToKeyed {
		return &CreditsExhaustedError{Vendor: vendor}
	}
	return nil
}

// ByokError is a turn-fatal BYOK failure. The server layer maps it to a
// distinct non-retryable HTTP error, and flips the key's metadata status when
// Auth is true.
type ByokError struct {
	Vendor string
	Auth   bool  // vendor rejected the key (401/403-shaped) — mark invalid
	Err    error // underlying cause; not always user-safe, message below is
	msg    string
}

func (e *ByokError) Error() string { return e.msg }
func (e *ByokError) Unwrap() error { return e.Err }

// AsByokError unwraps a turn error to its BYOK failure, if that's what it is.
func AsByokError(err error) *ByokError {
	var be *ByokError
	if errors.As(err, &be) {
		return be
	}
	return nil
}

func byokResolveError(vendor string, err error) *ByokError {
	if errors.Is(err, ErrKeyInvalid) {
		return &ByokError{Vendor: vendor, Auth: true, Err: err,
			msg: fmt.Sprintf("your %s API key was rejected — fix or remove it with /apikeys (your own key is used first while it's set)", vendor)}
	}
	return &ByokError{Vendor: vendor, Err: err,
		msg: fmt.Sprintf("your %s API key could not be retrieved right now — try again, or re-save it with /apikeys", vendor)}
}

func byokCallError(vendor string, err error) *ByokError {
	if isAuthShaped(err) {
		return &ByokError{Vendor: vendor, Auth: true, Err: err,
			msg: fmt.Sprintf("your %s API key was rejected (%s) — fix or remove it with /apikeys", vendor, clip(err.Error(), 80))}
	}
	return &ByokError{Vendor: vendor, Err: err,
		msg: fmt.Sprintf("your %s API key hit an error (%s) — the turn was not retried on memcode's keys; fix the key or remove it with /apikeys", vendor, clip(err.Error(), 80))}
}

// isAuthShaped classifies a vendor error as an auth rejection (mark the key
// invalid) vs anything transient. String-based of necessity — four vendor SDKs,
// one seam — and biased conservative: only unmistakable auth signals count.
func isAuthShaped(err error) bool {
	s := strings.ToLower(err.Error())
	for _, sig := range []string{"401", "403", "unauthorized", "invalid api key", "invalid_api_key", "authentication", "permission denied", "api key not valid"} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// SetByok wires the key-resolution seam into the router. Nil (the default)
// disables BYOK entirely — the zero-BYOK invariant's hard floor.
func (h *Hybrid) SetByok(k ByokKeys) { h.byok = k }

// byokFor reports whether THIS request should run vendor on the user's key.
func (h *Hybrid) byokFor(ctx context.Context, vendor string) (identity.Info, bool) {
	if h.byok == nil {
		return identity.Info{}, false
	}
	who := identity.From(ctx)
	return who, who.OrgID != "" && who.UserID != "" && who.HasKey(vendor)
}

// laneByok applies the request's billing lane to the presence gate: "credits"
// skips key injection entirely (an explicit, consented serve on the gateway keys — the
// requester asked for exactly that); "byok_only" makes an absent key
// turn-fatal instead of falling to credits; "" / "byok_preferred" is the
// default BYOK-first behavior. The gateway ENFORCES the lane, it never picks
// one — silent rerouting between the user's keys and credits is impossible in
// either direction.
func (h *Hybrid) laneByok(ctx context.Context, vendor, lane string) (identity.Info, bool, error) {
	if lane == "credits" {
		return identity.From(ctx), false, nil
	}
	who, ok := h.byokFor(ctx, vendor)
	if lane == "byok_only" && !ok {
		return who, false, &ByokError{Vendor: vendor,
			Err: errors.New("byok_only requested without a key"),
			msg: fmt.Sprintf("this turn was sent byok_only, but no %s key is on file — add one with /apikeys or resend on credits", vendor)}
	}
	return who, ok, nil
}

// byokStrongTier returns the tier to serve on: the user's own provider when
// the billing lane admits it and the presence gate says they have a key for
// tier.Vendor, else the tier as-is. A non-nil error is turn-fatal.
func (h *Hybrid) byokStrongTier(ctx context.Context, tier StrongTier, lane string) (StrongTier, bool, error) {
	who, ok, err := h.laneByok(ctx, tier.Vendor, lane)
	if err != nil {
		return tier, false, err
	}
	if !ok {
		return tier, false, nil
	}
	key, version, err := h.byok.Key(ctx, who.OrgID, who.UserID, tier.Vendor)
	if err != nil {
		return tier, false, byokResolveError(tier.Vendor, err)
	}
	cacheKey := who.OrgID + "|" + who.UserID + "|" + tier.Vendor + "|" + version
	if p, ok := h.byokProviders.Load(cacheKey); ok {
		tier.Provider = p.(StrongProvider)
		return tier, true, nil
	}
	var p StrongProvider
	switch tier.Vendor {
	case "openai":
		p = NewOpenAI(key)
	case "anthropic":
		p = NewAnthropic(key)
	case "gemini":
		p = NewGemini(key) // BYOK Gemini is always the Developer API, never Vertex/SA
	case "grok":
		p = NewGrok(key)
	default:
		// Turn-fatal, NEVER a silent pass-through: we only get here when the
		// presence gate said the user HAS a key for this vendor — serving the
		// turn on the gateway's own keys instead
		// would be exactly the silent fallback the doctrine header forbids.
		// The fix is adding a construction arm above (the consistency test in
		// byokroute_test.go fails until every ByokVendors() vendor has one).
		return tier, false, &ByokError{Vendor: tier.Vendor,
			Err: fmt.Errorf("no BYOK construction path for vendor %q", tier.Vendor),
			msg: fmt.Sprintf("your %s API key can't be used yet — this vendor has no BYOK support in the gateway; remove the key with /apikeys to serve on memcode credits instead", tier.Vendor)}
	}
	h.storeByokProvider(cacheKey, p)
	tier.Provider = p
	return tier, true, nil
}

// byokFireworks returns the user-keyed cheap lane when the user brought a
// Fireworks key. Same endpoint, their key.
func (h *Hybrid) byokFireworks(ctx context.Context, who identity.Info) (*Fireworks, error) {
	key, version, err := h.byok.Key(ctx, who.OrgID, who.UserID, "fireworks")
	if err != nil {
		return nil, byokResolveError("fireworks", err)
	}
	cacheKey := who.OrgID + "|" + who.UserID + "|fireworks|" + version
	if p, ok := h.byokProviders.Load(cacheKey); ok {
		return p.(*Fireworks), nil
	}
	fw := NewFireworks(h.cheapURL, key, h.cheapModel)
	h.storeByokProvider(cacheKey, fw)
	return fw, nil
}

// storeByokProvider caches a constructed per-user provider, bounded so a churn
// of users can't grow the map without limit (drop-all reset past the cap —
// reconstruction is cheap, correctness lives in the version-keyed name).
func (h *Hybrid) storeByokProvider(key string, p any) {
	n := 0
	h.byokProviders.Range(func(_, _ any) bool { n++; return n <= byokProviderCap })
	if n > byokProviderCap {
		h.byokProviders.Range(func(k, _ any) bool { h.byokProviders.Delete(k); return true })
	}
	h.byokProviders.Store(key, p)
}

const byokProviderCap = 512

// cheapComplete serves a cheap-lane call BYOK-first per the request's billing
// lane. isByok reports which lane class served (a BYOK error is turn-fatal at
// the caller — never absorbed).
func (h *Hybrid) cheapComplete(ctx context.Context, req wire.Request) (wire.Response, bool, error) {
	who, ok, lerr := h.laneByok(ctx, "fireworks", req.BillingLane)
	if lerr != nil {
		return wire.Response{}, true, lerr
	}
	if ok {
		fw, err := h.byokFireworks(ctx, who)
		if err != nil {
			return wire.Response{}, true, err
		}
		resp, err := fw.Complete(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return wire.Response{}, true, ctx.Err()
			}
			h.markInvalidOnAuth(ctx, who, "fireworks", err)
			return wire.Response{}, true, byokCallError("fireworks", err)
		}
		resp.BYOK, resp.BYOKVendor = true, "fireworks"
		return resp, true, nil
	}
	if err := requireCredits(ctx, "fireworks"); err != nil {
		// Typed 402 — the CLI's selection policy avoids unkeyed lanes at $0
		// up-front; this is the enforcement backstop, never a reroute.
		return wire.Response{}, false, err
	}
	resp, err := h.cheap.Complete(ctx, req)
	return resp, false, err
}

func (h *Hybrid) markInvalidOnAuth(ctx context.Context, who identity.Info, vendor string, err error) {
	if isAuthShaped(err) && h.byok != nil {
		h.byok.MarkInvalid(who.OrgID, who.UserID, vendor)
	}
}
