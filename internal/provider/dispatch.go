package provider

import (
	"context"
	"strings"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/providers/provcore"
	"github.com/memcode-ai/memcode/internal/wire"
)

// route picks the serving conn for a turn: the request's model family → that
// vendor's lane, else the base (gateway or exclusive endpoint). Only turn
// traffic dispatches; side channels always ride base. The returned request is
// a COPY when a lane rewrites the wire model (catalog label → raw vendor id).
func (l *Lazy) route(r wire.Request) (wire.Request, *conn, *lane, error) {
	base := l.c.Load()
	lanes := l.laneSet()
	// A consented exhaustion choice bypasses lane dispatch for this turn.
	if r.LaneBypass == "gateway" {
		if base == nil {
			return r, nil, nil, ErrNotLoggedIn
		}
		return r, base, nil, nil
	}
	if len(lanes) == 0 {
		if base == nil {
			return r, nil, nil, ErrNotLoggedIn
		}
		return r, base, nil, nil
	}
	// Exclusive endpoint mode never has lanes (buildLanes is skipped), so
	// reaching here means base is the gateway or nil.
	if r.Pin == "" {
		if base != nil {
			return r, base, nil, nil
		}
		ln := &lanes[0] // user's first listed lane; its ep.Model default applies
		return r, ln.c, ln, nil
	}
	vendor := catalog.ModelVendor(r.Pin)
	for i := range lanes {
		if lanes[i].vendor == vendor {
			ln := &lanes[i]
			rr := r
			rr.Pin = laneWireModel(r.Pin)
			return rr, ln.c, ln, nil
		}
	}
	if base != nil {
		return r, base, nil, nil
	}
	return r, nil, nil, &ErrNoLane{Model: r.Pin, Vendor: vendor, Attached: l.Lanes()}
}

// classifyLaneErr wraps a lane's terminal quota/rate failure as
// ErrLaneExhausted. 400/404 (bad request / unknown model — e.g. an id the
// codex backend doesn't serve) are ordinary errors and pass through: a
// model-404 must never read as "subscription exhausted".
func (l *Lazy) classifyLaneErr(ln *lane, err error) error {
	if err == nil {
		return nil
	}
	code, hdr, isAPI := provcore.APIErrorInfo(err)
	if !isAPI {
		// Compat lanes (copilot) surface plain "… http NNN: …" errors rather
		// than extractor-typed SDK errors — sniff the status from the text.
		code = sniffHTTPStatus(err.Error())
	}
	if code != 429 && code != 402 {
		return err
	}
	exh := &ErrLaneExhausted{
		Lane:        ln.info(),
		Status:      code,
		CanFallback: l.c.Load() != nil,
		Err:         err,
	}
	if hdr != nil {
		if d := provcore.RetryAfter(hdr); d > 0 {
			exh.ResetAt = timeNow().Add(d)
		}
	}
	return exh
}

// sniffHTTPStatus extracts an "http NNN" status from a textual transport
// error, 0 when absent.
func sniffHTTPStatus(msg string) int {
	i := strings.Index(msg, "http ")
	if i < 0 || i+8 > len(msg) {
		return 0
	}
	n := 0
	for _, ch := range msg[i+5 : i+8] {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

// restamp overwrites the response's backend with the lane's provenance name
// so the via-label, ledger split, and $0 costing are per-turn accurate.
func restamp(resp *wire.Response, ln *lane) {
	if ln != nil {
		resp.Backend = ln.backendName()
	}
}

// CompleteOnGateway forces a turn onto the gateway base, bypassing lane
// dispatch — the consented-fallback path after an ErrLaneExhausted card.
func (l *Lazy) CompleteOnGateway(ctx context.Context, r wire.Request) (wire.Response, error) {
	c := l.c.Load()
	if c == nil {
		return wire.Response{}, ErrNotLoggedIn
	}
	return c.Complete(ctx, r)
}

// StreamOnGateway is CompleteOnGateway's streaming twin.
func (l *Lazy) StreamOnGateway(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	c := l.c.Load()
	if c == nil {
		return wire.Response{}, ErrNotLoggedIn
	}
	return c.Stream(ctx, r, h)
}
