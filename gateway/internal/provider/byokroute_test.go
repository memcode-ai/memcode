package provider

// BYOK-first routing tests: the presence-gated provider swap, the zero-BYOK
// invariant (no keys → byte-identical routing), and fail-the-turn (a BYOK
// failure is never absorbed onto memcode's keys).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/gateway/internal/identity"
	"github.com/memcode-ai/memcode/internal/wire"
)

// fakeKeys is an in-memory ByokKeys.
type fakeKeys struct {
	keys     map[string]string // vendor → key
	err      error
	invalids []string
}

func (f *fakeKeys) Key(_ context.Context, _, _, vendor string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	k, ok := f.keys[vendor]
	if !ok {
		return "", "", errors.New("no key stored")
	}
	return k, "v1", nil
}
func (f *fakeKeys) MarkInvalid(_, _, vendor string) { f.invalids = append(f.invalids, vendor) }

func byokCtx(vendors ...string) context.Context {
	return identity.With(context.Background(), identity.Info{
		OrgID: "org-1", UserID: "user-1", KeyedVendors: vendors,
	})
}

func testHybrid(t *testing.T, cheapURL string) *Hybrid {
	t.Helper()
	base := NewOpenAI("gateway-key")
	strong := StrongTiers{"openai": {Vendor: "openai", Provider: base}}
	return NewHybrid(strong, cheapURL, "gateway-fw-key", "accounts/fireworks/models/m")
}

// TestByokTierSwapsProviderOnly: the swap touches ONLY the Provider — vendor
// label and model triple are untouched, so routing doctrine is unchanged.
func TestByokTierSwapsProviderOnly(t *testing.T) {
	h := testHybrid(t, "http://unused")
	h.SetByok(&fakeKeys{keys: map[string]string{"openai": "sk-user"}})
	orig := h.strong.StrongTierFor("openai")

	tier, isByok, err := h.byokStrongTier(byokCtx("openai"), orig, "")
	if err != nil {
		t.Fatal(err)
	}
	if !isByok {
		t.Fatal("presence gate says openai — the swap must fire")
	}
	if tier.Provider == orig.Provider {
		t.Fatal("Provider must be the user-keyed instance")
	}
	if tier.Vendor != orig.Vendor {
		t.Fatalf("vendor must be untouched: %+v vs %+v", tier, orig)
	}

	// Same (org,user,vendor,version) → cached instance, not a rebuild.
	tier2, _, _ := h.byokStrongTier(byokCtx("openai"), orig, "")
	if tier2.Provider != tier.Provider {
		t.Fatal("second resolve must hit the provider cache")
	}
}

// TestZeroByokInvariant: no byok wiring, or no keys for the vendor → the
// original tier rides through untouched with isByok=false and no error.
func TestZeroByokInvariant(t *testing.T) {
	h := testHybrid(t, "http://unused")
	orig := h.strong.StrongTierFor("openai")

	// (a) BYOK never wired (h.byok == nil).
	tier, isByok, err := h.byokStrongTier(byokCtx("openai"), orig, "")
	if err != nil || isByok || tier.Provider != orig.Provider {
		t.Fatalf("nil byok must be a pure pass-through: %v %v", isByok, err)
	}

	// (b) Wired, but the user has no key for this vendor.
	h.SetByok(&fakeKeys{keys: map[string]string{"anthropic": "sk-a"}})
	tier, isByok, err = h.byokStrongTier(byokCtx("anthropic"), orig, "") // openai tier, anthropic key
	if err != nil || isByok || tier.Provider != orig.Provider {
		t.Fatalf("no key for the tier vendor must pass through: %v %v", isByok, err)
	}

	// (c) Wired, keys exist, but the request has NO identity (defensive).
	tier, isByok, err = h.byokStrongTier(context.Background(), orig, "")
	if err != nil || isByok || tier.Provider != orig.Provider {
		t.Fatalf("identity-less context must pass through: %v %v", isByok, err)
	}
}

// TestByokResolveFailureFailsTurn: a key that can't be fetched (or was proven
// invalid) kills the turn with an actionable ByokError — serveStrong never
// reaches any provider.
func TestByokResolveFailureFailsTurn(t *testing.T) {
	h := testHybrid(t, "http://unused")
	h.SetByok(&fakeKeys{err: errors.New("key vault down")})

	_, err := h.serveStrong(byokCtx("openai"), wire.Request{Model: catalog.ModelTerra}, nil, h.strong.StrongTierFor("openai"))
	be := AsByokError(err)
	if be == nil {
		t.Fatalf("expected ByokError, got %v", err)
	}
	if !strings.Contains(be.Error(), "/apikeys") {
		t.Fatalf("message must point at /apikeys: %v", be)
	}

	// Fast-fail path: recently-rejected key.
	h.SetByok(&fakeKeys{err: ErrKeyInvalid})
	_, err = h.serveStrong(byokCtx("openai"), wire.Request{Model: catalog.ModelTerra}, nil, h.strong.StrongTierFor("openai"))
	be = AsByokError(err)
	if be == nil || !be.Auth {
		t.Fatalf("ErrKeyInvalid must map to an auth-shaped ByokError: %v", err)
	}
}

// TestByokCheapLaneUsesUserKey: with a fireworks BYOK key the cheap lane calls
// the SAME endpoint with the USER's bearer, and the response is stamped BYOK.
func TestByokCheapLaneUsesUserKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	h := testHybrid(t, srv.URL)
	h.SetByok(&fakeKeys{keys: map[string]string{"fireworks": "fw-user-key"}})

	resp, isByok, err := h.cheapComplete(byokCtx("fireworks"), wire.Request{
		Model:    "accounts/fireworks/models/m",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !isByok || !resp.BYOK || resp.BYOKVendor != "fireworks" {
		t.Fatalf("byok stamps missing: isByok=%v resp=%+v", isByok, resp.BYOK)
	}
	if gotAuth != "Bearer fw-user-key" {
		t.Fatalf("cheap lane must carry the USER's key, got %q", gotAuth)
	}
}

// TestByokCheapLaneFailureIsFatal: a 401 on the user's fireworks key fails the
// turn (no absorb) and marks the key invalid.
func TestByokCheapLaneFailureIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	h := testHybrid(t, srv.URL)
	fk := &fakeKeys{keys: map[string]string{"fireworks": "fw-bad"}}
	h.SetByok(fk)

	_, isByok, err := h.cheapComplete(byokCtx("fireworks"), wire.Request{
		Model:    "accounts/fireworks/models/m",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}},
	})
	if !isByok {
		t.Fatal("lane must report byok")
	}
	be := AsByokError(err)
	if be == nil {
		t.Fatalf("expected ByokError, got %v", err)
	}
	if !be.Auth || len(fk.invalids) == 0 || fk.invalids[0] != "fireworks" {
		t.Fatalf("auth failure must mark the key invalid: auth=%v invalids=%v", be.Auth, fk.invalids)
	}
	if !strings.Contains(be.Error(), "/apikeys") {
		t.Fatalf("message must be actionable: %v", be)
	}
}

// TestByokNoKeyCheapLaneUsesGateway: without a fireworks key the cheap lane is
// exactly the gateway's own (zero-BYOK invariant on the cheap path).
func TestByokNoKeyCheapLaneUsesGateway(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer srv.Close()

	h := testHybrid(t, srv.URL)
	h.cheap = NewFireworks(srv.URL, "gateway-fw-key", "accounts/fireworks/models/m")
	h.SetByok(&fakeKeys{keys: map[string]string{"openai": "sk-user"}}) // openai only

	resp, isByok, err := h.cheapComplete(byokCtx("openai"), wire.Request{
		Model:    "accounts/fireworks/models/m",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if isByok || resp.BYOK {
		t.Fatal("no fireworks key → gateway lane, no byok stamp")
	}
	if gotAuth != "Bearer gateway-fw-key" {
		t.Fatalf("gateway lane must carry the gateway key, got %q", gotAuth)
	}
}

func TestIsAuthShaped(t *testing.T) {
	for _, yes := range []string{"HTTP 401 unauthorized", "invalid api key", "authentication_error: bad key", "PERMISSION DENIED", "API key not valid"} {
		if !isAuthShaped(errors.New(yes)) {
			t.Errorf("%q must classify as auth", yes)
		}
	}
	for _, no := range []string{"connection refused", "rate limited: 429", "500 internal server error", "context deadline exceeded"} {
		if isAuthShaped(errors.New(no)) {
			t.Errorf("%q must NOT classify as auth", no)
		}
	}
}

// TestSanitizeKeepsByokBoolZeroesVendor: Response.BYOK rides the wire (the key
// owner's own information — the CLI footer's per-turn byok segment), while the
// vendor stays server-internal (cheap-lane anonymity).
func TestSanitizeKeepsByokBoolZeroesVendor(t *testing.T) {
	resp := wire.Response{Model: catalog.ModelTerra, BYOK: true, BYOKVendor: "openai"}
	SanitizeResponse(&resp)
	if !resp.BYOK {
		t.Fatal("SanitizeResponse must keep the BYOK bool — the footer's per-turn signal")
	}
	if resp.BYOKVendor != "" {
		t.Fatal("SanitizeResponse must zero the BYOK vendor")
	}
}

// TestByokDefaultArmIsTurnFatal: a vendor that reaches the construction switch
// without an arm must KILL the turn — the old `default: return tier, false, nil`
// silently served a keyed user on the gateway's own keys, the exact silent
// fallback the doctrine header forbids.
func TestByokDefaultArmIsTurnFatal(t *testing.T) {
	h := testHybrid(t, "http://unused")
	h.SetByok(&fakeKeys{keys: map[string]string{"newvendor": "k-new"}})

	tier, isByok, err := h.byokStrongTier(byokCtx("newvendor"), StrongTier{Vendor: "newvendor"}, "")
	be := AsByokError(err)
	if be == nil {
		t.Fatalf("vendor without a construction path must be a turn-fatal ByokError, got isByok=%v err=%v", isByok, err)
	}
	if isByok {
		t.Fatal("a failed construction must not report byok-served")
	}
	if tier.Provider != nil {
		t.Fatal("no provider may be constructed for an unknown vendor")
	}
	if !strings.Contains(be.Error(), "/apikeys") || !strings.Contains(be.Error(), "newvendor") {
		t.Fatalf("message must name the vendor and point at /apikeys: %v", be)
	}
}

// TestByokConstructionPathForEveryVendor: every vendor a user can store a key
// for (ByokVendors, derived from the models.json catalog) must have a BYOK
// construction path — byokStrongTier for the strong vendors, byokFireworks for
// the cheap lane. A new catalog vendor without an arm fails HERE, at build
// time, instead of silently serving that user's turns on memcode's keys (the
// pre-fix default arm) or 422-ing every turn in prod (the post-fix default arm).
func TestByokConstructionPathForEveryVendor(t *testing.T) {
	for _, vendor := range ByokVendors() {
		h := testHybrid(t, "http://unused")
		h.SetByok(&fakeKeys{keys: map[string]string{vendor: "k-" + vendor}})
		ctx := byokCtx(vendor)

		if vendor == "fireworks" {
			fw, err := h.byokFireworks(ctx, identity.From(ctx))
			if err != nil || fw == nil {
				t.Errorf("fireworks: cheap-lane BYOK construction failed: %v", err)
			}
			continue
		}
		tier, isByok, err := h.byokStrongTier(ctx, StrongTier{Vendor: vendor}, "")
		if err != nil {
			t.Errorf("%s: no BYOK construction path (turn-fatal default arm hit): %v", vendor, err)
			continue
		}
		if !isByok || tier.Provider == nil {
			t.Errorf("%s: construction must yield a user-keyed provider (isByok=%v)", vendor, isByok)
		}
	}
}

// --- billing-lane enforcement (memcode_billing) ---

// "credits" skips BYOK injection even when a key exists: an explicit,
// consented serve on the gateway's keys. The gateway enforces the lane, it never picks one.
func TestBillingLaneCreditsSkipsInjection(t *testing.T) {
	h := testHybrid(t, "http://unused")
	h.SetByok(&fakeKeys{keys: map[string]string{"openai": "sk-user"}})
	orig := h.strong.StrongTierFor("openai")

	tier, isByok, err := h.byokStrongTier(byokCtx("openai"), orig, "credits")
	if err != nil {
		t.Fatal(err)
	}
	if isByok || tier.Provider != orig.Provider {
		t.Fatalf("credits lane must serve on the gateway provider: isByok=%v", isByok)
	}
}

// "byok_only" without a key for the serving vendor is turn-fatal — never a
// silent fall to credits.
func TestBillingLaneByokOnlyWithoutKeyIsFatal(t *testing.T) {
	h := testHybrid(t, "http://unused")
	h.SetByok(&fakeKeys{keys: map[string]string{"anthropic": "sk-a"}}) // no openai key

	_, _, err := h.byokStrongTier(byokCtx("anthropic"), h.strong.StrongTierFor("openai"), "byok_only")
	be := AsByokError(err)
	if be == nil {
		t.Fatalf("byok_only without a key must be a ByokError, got %v", err)
	}
	if !strings.Contains(be.Error(), "openai") {
		t.Fatalf("message must name the missing vendor: %v", be)
	}
}

// The cheap lane honors the lane too: "credits" with a fireworks key still
// serves on the gateway's key.
func TestBillingLaneCreditsOnCheapLane(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer srv.Close()

	h := testHybrid(t, srv.URL)
	h.cheap = NewFireworks(srv.URL, "gateway-fw-key", "accounts/fireworks/models/m")
	h.SetByok(&fakeKeys{keys: map[string]string{"fireworks": "fw-user-key"}})

	resp, isByok, err := h.cheapComplete(byokCtx("fireworks"), wire.Request{
		Model:       "accounts/fireworks/models/m",
		BillingLane: "credits",
		Messages:    []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if isByok || resp.BYOK {
		t.Fatal("credits lane must not stamp byok")
	}
	if gotAuth != "Bearer gateway-fw-key" {
		t.Fatalf("credits lane must carry the gateway key, got %q", gotAuth)
	}
}
