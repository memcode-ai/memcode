package provider

// Fireworks is the cheap lane — the shared generic compat engine
// (providers/compat) constructed in LANE mode: the lane error contract
// (LaneRequestError), the model-conditional reasoning_effort vocabulary, and
// the tool-call salvage net + MiniMax leak strip. The wire implementation
// lives in the SDK; this file is construction only (gateway concern: which
// endpoint, whose key).
//
// Stamps Backend: "fireworks" on responses via the router — honest INTERNAL
// naming for metering; SanitizeResponse maps it to the vendor-neutral "cheap"
// before anything leaves the server.

import (
	"context"

	"github.com/memcode-ai/memcode/internal/providers/compat"
	"github.com/memcode-ai/memcode/internal/wire"
)

// LaneRequestError is the shared lane error contract (compatwire) — aliased so
// errors.As matches identically across the gateway and the shared engine.
type LaneRequestError = compat.LaneRequestError

// Fireworks wraps the shared engine with the lane's default model.
type Fireworks struct {
	t     *compat.Transport
	model string
}

// NewFireworks returns the cheap-lane client: the shared compat engine against
// the Fireworks OpenAI-compatible API, lane mode + salvage on.
func NewFireworks(baseURL, apiKey, model string) *Fireworks {
	return &Fireworks{
		t: compat.New(compat.Config{
			BaseURL: baseURL,
			Token:   apiKey,
			Model:   model,
			Lane:    true,
			Salvage: true,
		}),
		model: model,
	}
}

// Model returns the lane's default served model id.
func (f *Fireworks) Model() string { return f.model }

// Complete serves one lane call on the shared engine. Backend is stamped
// "fireworks" — honest internal naming for metering (the engine's generic
// "endpoint" tag is for backends it can't identify).
func (f *Fireworks) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	resp, err := f.t.Complete(ctx, r)
	f.stamp(&resp, r)
	return resp, err
}

// Stream serves one lane call, forwarding deltas (the router buffers cheap
// serves today; the capability exists for parity).
func (f *Fireworks) Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	resp, err := f.t.Stream(ctx, r, h)
	f.stamp(&resp, r)
	return resp, err
}

// stamp restores the lane's response identity contract: Backend "fireworks"
// (metering) and the served model backfilled from the request/default when the
// endpoint omitted it.
func (f *Fireworks) stamp(resp *wire.Response, r wire.Request) {
	resp.Backend = "fireworks"
	if resp.Model == "" {
		resp.Model = r.Model
	}
	if resp.Model == "" {
		resp.Model = f.model
	}
}
