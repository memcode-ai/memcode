package llm

// policy_test.go — the selection + recovery behaviors BEYOND ladder parity:
// capability pre-checks (vision/document/overflow absorbs, now client-side and
// visible), the $0 fundability rule, the catalog fallback walk with the
// emitted-output guard, the delegate-doctrine append, and the
// backend-uniformity contract (hosted policy vs endpoint pass-through).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// scriptedProv is a ModelProvider+Streamer+Endpointer fake: records requested
// models, fails per script, optionally reports an endpoint.
type scriptedProv struct {
	requested []string
	failures  map[string]error // label → error to return
	onEP      *provider.Endpoint
	streamed  string // text to emit via handler before failing (emitted-guard tests)
}

func (p *scriptedProv) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	p.requested = append(p.requested, r.Pin)
	if err := p.failures[r.Pin]; err != nil {
		return wire.Response{}, err
	}
	return wire.Response{StopReason: "end_turn", Model: r.Pin,
		Blocks: []wire.Block{wire.TextBlock("ok")}, InputTokens: 1, OutputTokens: 1}, nil
}

func (p *scriptedProv) Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	if p.streamed != "" && h.Text != nil {
		h.Text(p.streamed)
	}
	return p.Complete(ctx, r)
}

func (p *scriptedProv) Endpoint() (provider.Endpoint, bool) {
	if p.onEP != nil {
		return *p.onEP, true
	}
	return provider.Endpoint{}, false
}

// hostedRunner builds a Runner over a hosted scripted provider with a fixed
// control-plane snapshot (no network).
func hostedRunner(p *scriptedProv, info provider.ModelsInfo) *Runner {
	r := NewRunner(p)
	r.sel.fetch = func(context.Context) (provider.ModelsInfo, error) { return info, nil }
	return r
}

func userReq(text string) wire.Request {
	return wire.Request{Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock(text)}}}}
}

func TestHostedSelectionStampsLabels(t *testing.T) {
	p := &scriptedProv{}
	r := hostedRunner(p, prodInfo(nil))

	if _, err := r.Complete(context.Background(), MainLoop, userReq("hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Complete(context.Background(), Classify, userReq("hi")); err != nil {
		t.Fatal(err)
	}
	req := userReq("hi")
	req.RoutingHint = &wire.RoutingHint{Reason: "self_heal"}
	if _, err := r.Complete(context.Background(), MainLoop, req); err != nil {
		t.Fatal(err)
	}
	want := []string{"glm-5p2", "gpt-oss-120b", "sol"}
	for i, w := range want {
		if p.requested[i] != w {
			t.Errorf("call %d requested %q, want %q (all: %v)", i, p.requested[i], w, p.requested)
		}
	}
}

func TestVisionAbsorbIsClientSideAndVisible(t *testing.T) {
	p := &scriptedProv{}
	r := hostedRunner(p, prodInfo(nil))
	req := userReq("see")
	req.Messages[0].Blocks = append(req.Messages[0].Blocks, wire.ImageBlock("image/png", []byte{1}))

	resp, err := r.Complete(context.Background(), MainLoop, req)
	if err != nil {
		t.Fatal(err)
	}
	// glm-5p2 (no vision) → the default vendor's balanced tier, reason stamped.
	if p.requested[0] != "terra" {
		t.Fatalf("vision turn requested %q, want terra", p.requested[0])
	}
	if resp.FallbackReason != "vision" {
		t.Fatalf("FallbackReason = %q, want vision (the ⇄ line's signal)", resp.FallbackReason)
	}
}

func TestDocumentAbsorbAndCapabilityRefusal(t *testing.T) {
	p := &scriptedProv{}
	r := hostedRunner(p, prodInfo(nil))
	req := userReq("read")
	req.Messages[0].Blocks = append(req.Messages[0].Blocks, wire.DocumentBlock("application/pdf", []byte{1}))

	resp, err := r.Complete(context.Background(), MainLoop, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.requested[0] != "terra" || resp.FallbackReason != "document" {
		t.Fatalf("document turn requested %q reason %q, want terra/document", p.requested[0], resp.FallbackReason)
	}

	// Fireworks-only keys at $0: nothing PDF-capable is fundable → the
	// actionable capability refusal, client-side (the old pdfCapabilityError).
	info := prodInfo([]string{"fireworks"})
	info.CreditsExhausted = true
	r2 := hostedRunner(&scriptedProv{}, info)
	_, err = r2.Complete(context.Background(), MainLoop, req)
	if err == nil || !strings.Contains(err.Error(), "PDF") || !strings.Contains(err.Error(), "/apikeys") {
		t.Fatalf("want the actionable PDF refusal, got %v", err)
	}
}

func TestZeroCreditsSteersAutomaticOffUnkeyedLanes(t *testing.T) {
	info := prodInfo([]string{"anthropic"})
	info.CreditsExhausted = true
	p := &scriptedProv{}
	r := hostedRunner(p, info)

	// Automatic main loop resolved glm-5p2 (fireworks, unkeyed) → remapped to
	// the keyed vendor's balanced tier, visibly.
	resp, err := r.Complete(context.Background(), MainLoop, userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if p.requested[0] != "sonnet" {
		t.Fatalf("$0 automatic requested %q, want sonnet (the keyed lane)", p.requested[0])
	}
	if resp.FallbackReason != "credits_byok" {
		t.Fatalf("reason = %q, want credits_byok", resp.FallbackReason)
	}

	// A PIN is exempt: the unkeyed pin rides through — the gateway's 402 names
	// the vendor (an explicit choice is never coerced).
	r.SetPin("terra")
	if _, err := r.Complete(context.Background(), MainLoop, userReq("hi")); err != nil {
		t.Fatal(err)
	}
	if p.requested[1] != "terra" {
		t.Fatalf("$0 pin requested %q, want terra untouched", p.requested[1])
	}
}

func TestFallbackWalkOnModelError(t *testing.T) {
	p := &scriptedProv{failures: map[string]error{"glm-5p2": errors.New("lane http 500: boom")}}
	r := hostedRunner(p, prodInfo(nil))

	resp, err := r.Complete(context.Background(), MainLoop, userReq("hi"))
	if err != nil {
		t.Fatalf("the chain must rescue the call: %v", err)
	}
	// glm-5p2 fails → catalog chain: kimi-k2p7-code.
	if len(p.requested) != 2 || p.requested[1] != "kimi-k2p7-code" {
		t.Fatalf("walk = %v, want [glm-5p2 kimi-k2p7-code]", p.requested)
	}
	if !strings.HasPrefix(resp.FallbackReason, "model_error: ") {
		t.Fatalf("reason = %q, want model_error: …", resp.FallbackReason)
	}
	if resp.RequestedModel != "glm-5p2" {
		t.Fatalf("RequestedModel = %q, want the primary selection", resp.RequestedModel)
	}
}

func TestFallbackNeverTouchesTerminalErrors(t *testing.T) {
	for _, sentinel := range []error{wire.ErrInsufficientCredit, wire.ErrByokKeyFailed,
		wire.ErrContextOverflow, wire.ErrStreamIncomplete, wire.ErrUnauthorized} {
		p := &scriptedProv{failures: map[string]error{"glm-5p2": sentinel}}
		r := hostedRunner(p, prodInfo(nil))
		_, err := r.Complete(context.Background(), MainLoop, userReq("hi"))
		if !errors.Is(err, sentinel) {
			t.Errorf("%v: err = %v, want the sentinel through untouched", sentinel, err)
		}
		if len(p.requested) != 1 {
			t.Errorf("%v: %d calls — terminal errors must not walk the chain", sentinel, len(p.requested))
		}
	}
}

func TestEmittedOutputStopsFallback(t *testing.T) {
	p := &scriptedProv{
		failures: map[string]error{"glm-5p2": errors.New("lane http 500: died mid-stream")},
		streamed: "partial answer…",
	}
	r := hostedRunner(p, prodInfo(nil))
	var got strings.Builder
	_, err := r.Stream(context.Background(), MainLoop, userReq("hi"), wire.StreamHandler{
		Text: func(d string) { got.WriteString(d) },
	})
	if err == nil {
		t.Fatal("want the error to surface — output already reached the user")
	}
	if len(p.requested) != 1 {
		t.Fatalf("%d calls — once output is emitted, no silent replay on another model", len(p.requested))
	}
}

func TestDelegateDoctrineAppendsOnCheapLane(t *testing.T) {
	p := &scriptedProv{}
	r := hostedRunner(p, prodInfo(nil))

	req := userReq("build it")
	req.Mode, req.System, req.SystemVolatile = "exec", "DOCTRINE", "[today: x]"
	var seen string
	pr := &captureSystem{inner: p, out: &seen}
	r.prov = pr
	if _, err := r.Complete(context.Background(), MainLoop, req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(seen, "DOCTRINE") || !strings.Contains(seen, "Match the work to the model") {
		t.Fatalf("cheap-lane exec must append the delegate doctrine to the stable half: %q", seen)
	}

	// A pinned strong model never gets it.
	seen = ""
	r.SetPin("sonnet")
	if _, err := r.Complete(context.Background(), MainLoop, req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seen, "Match the work to the model") {
		t.Fatal("a pinned model must not get the delegate doctrine")
	}
}

// captureSystem wraps a provider to capture the outgoing System text.
type captureSystem struct {
	inner *scriptedProv
	out   *string
}

func (c *captureSystem) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	*c.out = r.System
	return c.inner.Complete(ctx, r)
}
func (c *captureSystem) Endpoint() (provider.Endpoint, bool) { return c.inner.Endpoint() }

func TestEndpointModeBypassesPolicy(t *testing.T) {
	p := &scriptedProv{onEP: &provider.Endpoint{Name: "ollama", BaseURL: "http://localhost:11434/v1", Model: "qwen3:4b"}}
	r := NewRunner(p)
	r.sel.fetch = func(context.Context) (provider.ModelsInfo, error) {
		t.Fatal("endpoint mode must never fetch the hosted control plane")
		return provider.ModelsInfo{}, nil
	}
	r.SetPin("qwen3:4b")
	if _, err := r.Complete(context.Background(), MainLoop, userReq("hi")); err != nil {
		t.Fatal(err)
	}
	if p.requested[0] != "qwen3:4b" {
		t.Fatalf("endpoint call requested %q, want the session model untouched", p.requested[0])
	}
}

func TestControlPlaneOutageDegradesToCatalog(t *testing.T) {
	p := &scriptedProv{}
	r := NewRunner(p)
	r.sel.fetch = func(context.Context) (provider.ModelsInfo, error) {
		return provider.ModelsInfo{}, errors.New("gateway down")
	}
	if _, err := r.Complete(context.Background(), MainLoop, userReq("hi")); err != nil {
		t.Fatal(err)
	}
	// No roles in the degraded snapshot → the default vendor's balanced tier.
	if p.requested[0] != "terra" {
		t.Fatalf("degraded selection requested %q, want terra", p.requested[0])
	}
}

// An uncataloged served label (catalog skew) must tag backend "unknown" — the
// old empty backend fell into the ledger's stale "anthropic" default AND
// skipped the footer's served-state recording.
func TestUncatalogedServeTagsUnknown(t *testing.T) {
	p := &scriptedProv{}
	info := prodInfo(nil)
	info.Models = append(info.Models, provider.ModelFact{Label: "mystery-9b", Vendor: "", Window: 100000, Pinnable: true})
	r := hostedRunner(p, info)
	r.SetPin("mystery-9b")
	resp, err := r.Complete(context.Background(), MainLoop, userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Backend != "unknown" {
		t.Fatalf("Backend = %q, want unknown", resp.Backend)
	}
	if st := r.Ledger().ByBackend(); st["anthropic"].Calls != 0 || st["unknown"].Calls != 1 {
		t.Fatalf("ledger attribution wrong: %+v", st)
	}
}

// Fork inherits the vendor flavor — sub-agents must honor /model <vendor>.
func TestForkInheritsVendor(t *testing.T) {
	p := &scriptedProv{}
	r := hostedRunner(p, prodInfo(nil))
	r.SetVendor("anthropic")
	f := r.Fork()
	req := userReq("hi")
	req.RoutingHint = &wire.RoutingHint{Reason: "self_heal"}
	if _, err := f.Complete(context.Background(), MainLoop, req); err != nil {
		t.Fatal(err)
	}
	// self_heal → frontier tier of the SESSION vendor: opus, not sol.
	if p.requested[0] != "opus" {
		t.Fatalf("forked self_heal requested %q, want opus (inherited anthropic vendor)", p.requested[0])
	}
}
