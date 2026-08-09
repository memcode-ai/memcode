package provider

// hybrid_test.go — the SERVING contract (all-policy-client-side): the router
// serves exactly the requested model or returns a TYPED error. Nothing is
// absorbed, retargeted, or steered — those behaviors moved to the CLI's
// recovery policy, which keys on the typed errors locked here.

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/wire"
)

// fakeStrong is a minimal strong-tier provider for router tests.
type fakeStrong struct{}

func (fakeStrong) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{wire.TextBlock("ok")},
		Model: r.Model, Backend: "openai"}, nil
}
func (f fakeStrong) Stream(ctx context.Context, r wire.Request, _ wire.StreamHandler) (wire.Response, error) {
	return f.Complete(ctx, r)
}
func (fakeStrong) WebSearch(context.Context, string) (string, wire.Response, error) {
	return "", wire.Response{}, nil
}
func (fakeStrong) WebFetch(context.Context, string) (string, wire.Response, error) {
	return "", wire.Response{}, nil
}
func (fakeStrong) Model() string { return catalog.ModelTerra }

func servingHybrid(srv string) *Hybrid {
	return &Hybrid{
		strong:     StrongTiers{"openai": {Vendor: "openai", Provider: fakeStrong{}}},
		cheap:      NewFireworks(srv+"/v1", "k", "accounts/fireworks/models/glm-5p2"),
		cheapModel: "accounts/fireworks/models/glm-5p2",
	}
}

func userTurn(text string) []wire.Message {
	return []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock(text)}}}
}

// TestCheapLaneStampsInputBudget: every serve must carry the lane's usable
// input capacity (window − reserve, floored at window/2) so the CLI's adaptive
// compaction budget learns the REAL lane instead of a static default.
func TestCheapLaneStampsInputBudget(t *testing.T) {
	srv, _ := captureServer(t, http.StatusOK, func(w http.ResponseWriter, _ oaRequest) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	newH := func(model string) *Hybrid {
		return &Hybrid{
			strong:     StrongTiers{"openai": {Vendor: "openai", Provider: fakeStrong{}}},
			cheap:      NewFireworks(srv.URL+"/v1", "k", model),
			cheapModel: model,
		}
	}

	// 1M-window lane (glm-5p2) → window − reserve.
	glm := "accounts/fireworks/models/glm-5p2"
	resp, err := newH(glm).Complete(context.Background(), wire.Request{
		Model:    glm,
		Messages: userTurn("hi"),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := 1_000_000 - inputBudgetReserve; resp.InputBudget != want {
		t.Fatalf("InputBudget = %d, want %d (window − reserve)", resp.InputBudget, want)
	}

	// Small-window lane (gpt-oss-120b, 131072) → floored at window/2, never collapsed.
	oss := "accounts/fireworks/models/gpt-oss-120b"
	resp, err = newH(oss).Complete(context.Background(), wire.Request{
		Model:    oss,
		Messages: userTurn("hi"),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := 131072 - inputBudgetReserve; resp.InputBudget != want || resp.InputBudget < 131072/2 {
		t.Fatalf("InputBudget = %d, want %d (≥ window/2 floor)", resp.InputBudget, want)
	}

	// Strong-tier serve → the strong model's budget is stamped too.
	resp, err = newH(glm).Complete(context.Background(), wire.Request{
		Model:    catalog.ModelTerra,
		Messages: userTurn("hi"),
	})
	if err != nil {
		t.Fatalf("Complete (strong): %v", err)
	}
	if want := 1_050_000 - inputBudgetReserve; resp.InputBudget != want {
		t.Fatalf("strong-tier serve InputBudget = %d, want %d", resp.InputBudget, want)
	}
}

// A strong-vendor model rides through to its owner untouched — the requested
// id IS what serves.
func TestStrongModelServesUntouched(t *testing.T) {
	h := servingHybrid("http://unused.invalid")
	resp, err := h.Complete(context.Background(), wire.Request{
		Model: catalog.ModelSol, Messages: userTurn("hi"),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != catalog.ModelSol || resp.FallbackReason != "" {
		t.Fatalf("serve = model %q fallback %q, want sol / no fallback", resp.Model, resp.FallbackReason)
	}
	if resp.InputBudget == 0 {
		t.Fatal("strong serve must stamp InputBudget")
	}
}

// A Fireworks model (glm/kimi) serves on the Fireworks lane with the exact
// requested id.
func TestFireworksModelServesOnLane(t *testing.T) {
	var served string
	srv, _ := captureServer(t, http.StatusOK, func(w http.ResponseWriter, r oaRequest) {
		served = r.Model
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	h := servingHybrid(srv.URL)
	kimi := "accounts/fireworks/models/kimi-k2p6"
	resp, err := h.Complete(context.Background(), wire.Request{
		Model: kimi, Messages: userTurn("hi"),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if served != kimi {
		t.Fatalf("lane served %q, want the requested id %q", served, kimi)
	}
	if resp.Backend != "fireworks" || resp.FallbackReason != "" {
		t.Fatalf("backend %q fallback %q, want fireworks / no fallback", resp.Backend, resp.FallbackReason)
	}
}

// An image turn on a no-vision model is a TYPED capability error — never an
// absorb, never a silent retarget. The CLI pre-checks this from the shared
// catalog; the gateway is the enforcement backstop.
func TestVisionGapIsTypedError(t *testing.T) {
	h := servingHybrid("http://unused.invalid")
	img := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "image", Content: "…"}}}}
	_, err := h.Complete(context.Background(), wire.Request{
		Model: "accounts/fireworks/models/glm-5p2", Messages: img,
	})
	ce := AsCapabilityError(err)
	if ce == nil || ce.Capability != "vision" {
		t.Fatalf("err = %v, want CapabilityError{vision}", err)
	}
	if ce.Model != "glm-5p2" {
		t.Fatalf("capability error names %q, want the client-facing label glm-5p2", ce.Model)
	}
}

// A document turn on a model without native PDF input is the document
// capability error.
func TestDocumentGapIsTypedError(t *testing.T) {
	h := servingHybrid("http://unused.invalid")
	doc := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "document", Content: "…"}}}}
	_, err := h.Complete(context.Background(), wire.Request{
		Model: "grok-4.5", Messages: doc, // grok: vision yes, pdf no
	})
	// grok isn't in the test tier map, but the capability gate fires BEFORE
	// owner lookup — the typed error must win over the config error.
	ce := AsCapabilityError(err)
	if ce == nil || ce.Capability != "document" {
		t.Fatalf("err = %v, want CapabilityError{document}", err)
	}
}

// A prompt past the requested model's window is a typed overflow — the CLI
// compacts and retries; the gateway never reroutes to a bigger model.
func TestOverflowIsTypedError(t *testing.T) {
	h := servingHybrid("http://unused.invalid")
	big := make([]byte, 300_000*4*2) // ~600K estimated tokens, past haiku's 200K
	for i := range big {
		big[i] = 'a'
	}
	_, err := h.Complete(context.Background(), wire.Request{
		Model: catalog.ModelHaiku, Messages: userTurn(string(big)),
	})
	if !IsContextOverflow(err) {
		t.Fatalf("err = %v, want a context overflow", err)
	}
}

// A cheap-lane failure surfaces as an error — no absorb onto the strong tier.
// (The CLI's fallback chain decides what happens next, visibly.)
func TestLaneErrorSurfacesTyped(t *testing.T) {
	srv, _ := captureServer(t, http.StatusInternalServerError, func(w http.ResponseWriter, _ oaRequest) {
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	})
	h := servingHybrid(srv.URL)
	_, err := h.Complete(context.Background(), wire.Request{
		Model: "accounts/fireworks/models/glm-5p2", Messages: userTurn("hi"),
	})
	if err == nil {
		t.Fatal("want the lane error to surface, got a served response")
	}
	if AsCapabilityError(err) != nil || IsContextOverflow(err) {
		t.Fatalf("a lane failure must stay a plain serving error, got %v", err)
	}
}

// A model whose vendor has no configured credentials errors cleanly (the
// /v1/models gate prevents this normally — this is the config-race backstop).
func TestUnconfiguredVendorErrors(t *testing.T) {
	h := servingHybrid("http://unused.invalid")
	_, err := h.Complete(context.Background(), wire.Request{
		Model: catalog.ModelSonnet, Messages: userTurn("hi"), // anthropic not in the test tier map
	})
	if err == nil {
		t.Fatal("want an error for an unconfigured vendor")
	}
}
