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
// hostedRunner builds a Runner over a hosted scripted provider with a fixed
// control-plane snapshot (no network) and a PIN. Production's pin resolver
// always yields one — session override, workspace, user, or the default_model
// seed — so an unpinned Runner is not a state the product can reach, and
// selection deliberately refuses rather than inventing a model.
func hostedRunner(p *scriptedProv, info provider.ModelsInfo) *Runner {
	return pinnedRunner(p, info, "sonnet")
}

func pinnedRunner(p *scriptedProv, info provider.ModelsInfo, pin string) *Runner {
	r := NewRunner(p)
	r.sel.fetch = func(context.Context) (provider.ModelsInfo, error) { return info, nil }
	r.SetPin(pin)
	return r
}

func userReq(text string) wire.Request {
	return wire.Request{Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock(text)}}}}
}

func TestPinServesEveryRealPurposeAndUtilityRidesTheUtilityModel(t *testing.T) {
	p := &scriptedProv{}
	r := pinnedRunner(p, prodInfo(nil), "opus")

	// Real work rides the pin regardless of purpose — there is no ladder left
	// to send a "hard" turn somewhere else or a "cheap" turn down a tier.
	for _, purpose := range []Purpose{MainLoop, Synth, Reflect, Explore, Agent, Learn, Predict} {
		if _, err := r.Complete(context.Background(), purpose, userReq("hi")); err != nil {
			t.Fatalf("%s: %v", purpose, err)
		}
	}
	// Internal plumbing never rides the pin.
	for _, purpose := range []Purpose{Classify, Compact, Shrinkwrap} {
		if _, err := r.Complete(context.Background(), purpose, userReq("hi")); err != nil {
			t.Fatalf("%s: %v", purpose, err)
		}
	}

	for i := 0; i < 7; i++ {
		if p.requested[i] != "opus" {
			t.Errorf("real-work call %d requested %q, want the pin (opus); all: %v", i, p.requested[i], p.requested)
		}
	}
	for i := 7; i < 10; i++ {
		if p.requested[i] != "gpt-oss-120b" {
			t.Errorf("utility call %d requested %q, want the utility model; all: %v", i, p.requested[i], p.requested)
		}
	}
}

// An unpinned Runner refuses rather than falling back to default_model.
// default_model seeds the PIN, once, in the resolver — it is not a runtime
// default, or it would quietly become the new Automatic.
func TestUnpinnedSelectionRefusesAndNeverDefaults(t *testing.T) {
	p := &scriptedProv{}
	r := NewRunner(p)
	r.sel.fetch = func(context.Context) (provider.ModelsInfo, error) { return prodInfo(nil), nil }

	_, err := r.Complete(context.Background(), MainLoop, userReq("hi"))
	if err == nil || !strings.Contains(err.Error(), "no model selected") {
		t.Fatalf("want the no-model refusal, got %v", err)
	}
	if len(p.requested) != 0 {
		t.Fatalf("nothing may be requested without a pin, got %v", p.requested)
	}
}

// Capability gaps FAIL the turn. They used to absorb onto a capable model,
// which was Automatic routing wearing a different hat: a pasted screenshot
// silently moved the turn onto a model the user never chose.
func TestCapabilityGapsRefuseInsteadOfSwitchingModels(t *testing.T) {
	image := func() wire.Request {
		req := userReq("see")
		req.Messages[0].Blocks = append(req.Messages[0].Blocks, wire.ImageBlock("image/png", []byte{1}))
		return req
	}
	doc := func() wire.Request {
		req := userReq("read")
		req.Messages[0].Blocks = append(req.Messages[0].Blocks, wire.DocumentBlock("application/pdf", []byte{1}))
		return req
	}

	cases := []struct {
		name string
		pin  string
		req  wire.Request
		want string
	}{
		{"image on a vision-less pin", "glm-5p2", image(), "images"},
		{"pdf on a pin without native PDF", "glm-5p2", doc(), "PDFs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &scriptedProv{}
			r := pinnedRunner(p, prodInfo(nil), tc.pin)
			_, err := r.Complete(context.Background(), MainLoop, tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal mentioning %q, got %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "/model") {
				t.Errorf("the refusal must name the fix (/model): %v", err)
			}
			if len(p.requested) != 0 {
				t.Fatalf("a refused turn must reach no provider, got %v", p.requested)
			}
		})
	}

	// A capable pin serves the same turns untouched.
	p := &scriptedProv{}
	r := pinnedRunner(p, prodInfo(nil), "opus")
	if _, err := r.Complete(context.Background(), MainLoop, image()); err != nil {
		t.Fatalf("a vision-capable pin must serve an image turn: %v", err)
	}
	if p.requested[0] != "opus" {
		t.Fatalf("requested %q, want the pin untouched", p.requested[0])
	}
}

// A turn past the pin's window refuses too — /compact or switch, never a
// silent hop to a bigger model.
func TestContextOverflowRefuses(t *testing.T) {
	info := prodInfo(nil)
	for i := range info.Models {
		if info.Models[i].Label == "sonnet" {
			info.Models[i].Window = 100
		}
	}
	p := &scriptedProv{}
	r := pinnedRunner(p, info, "sonnet")
	_, err := r.Complete(context.Background(), MainLoop, userReq(strings.Repeat("word ", 2000)))
	if err == nil || !strings.Contains(err.Error(), "window") {
		t.Fatalf("want an overflow refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "/compact") {
		t.Errorf("the refusal must name /compact: %v", err)
	}
	if len(p.requested) != 0 {
		t.Fatalf("a refused turn must reach no provider, got %v", p.requested)
	}
}

// TestZeroCreditsSteersAutomaticOffUnkeyedLanes is DELETED. It asserted that an
// AUTOMATIC selection on an unfunded vendor was remapped to a keyed one. There
// is no automatic selection left to remap, and a pin was already exempt from
// that coercion by design — an explicit choice gets the gateway's clean 402
// naming the vendor, never a silent switch. That is now the only behavior.

func TestFallbackWalkOnModelError(t *testing.T) {
	p := &scriptedProv{failures: map[string]error{"glm-5p2": errors.New("lane http 500: boom")}}
	r := pinnedRunner(p, prodInfo(nil), "glm-5p2")

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
		r := pinnedRunner(p, prodInfo(nil), "glm-5p2")
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
	r := pinnedRunner(p, prodInfo(nil), "glm-5p2")
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

// TestDelegateDoctrineAppendsOnCheapLane is DELETED along with
// delegateDoctrine itself: the prompt told a cheap AUTOMATIC coding lane to
// delegate non-code work to a stronger sub-agent. With one pinned model there
// is no cheap lane to compensate for and no stronger tier to delegate to.

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
	r.SetPin("opus")
	if _, err := r.Complete(context.Background(), MainLoop, userReq("hi")); err != nil {
		t.Fatal(err)
	}
	// A gateway outage cannot change the model: the pin is local state and the
	// degraded catalog snapshot still carries its capabilities.
	if p.requested[0] != "opus" {
		t.Fatalf("degraded selection requested %q, want the pin (opus)", p.requested[0])
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
// A fork inherits the PIN. It used to inherit a session VENDOR, which only
// mattered because the ladder resolved a tier within that vendor; the vendor is
// implied by the model now.
func TestForkInheritsPin(t *testing.T) {
	p := &scriptedProv{}
	r := pinnedRunner(p, prodInfo(nil), "opus")
	f := r.Fork()
	if _, err := f.Complete(context.Background(), MainLoop, userReq("hi")); err != nil {
		t.Fatal(err)
	}
	if p.requested[0] != "opus" {
		t.Fatalf("forked turn requested %q, want the inherited pin (opus)", p.requested[0])
	}
}
