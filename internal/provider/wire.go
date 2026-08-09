package provider

// wire.go — the connection story: ONE turn transport (the OpenAI-compat
// chat/completions wire, sdk providers/compat) against whichever base URL
// the backend selection picked — the memcode gateway at {api}/v1, or any
// arbitrary compat endpoint. The old memcode-shaped turn wire and its
// MEMCODE_WIRE selection flag are gone (one-wire final pass): there is exactly
// one way requests leave this machine. The side channels (websearch / webfetch
// / advisor) and BYOK key management are gateway-only services with no compat
// equivalent, so they ride the SDK client next to the turn transport — conn
// couples the two so capability composition stays invisible to callers.

import (
	"context"
	"fmt"
	"time"

	"github.com/memcode-ai/memcode/internal/doctrine"
	"github.com/memcode-ai/memcode/internal/gateway/client"
	compat "github.com/memcode-ai/memcode/internal/providers/compat"
	memcodeprov "github.com/memcode-ai/memcode/internal/providers/memcode"
	"github.com/memcode-ai/memcode/internal/wire"
)

// composeDoctrine renders a legacy-shaped side call (Mode stamped, doctrine
// not yet composed) through the CLI's doctrine — the Compose hook every turn
// transport gets. The CLI owns doctrine; the shared engines only run the hook.
func composeDoctrine(r wire.Request) (wire.Request, error) {
	stable, volatile, err := doctrine.Compose(r.Mode, r.Facts, r.System, "", false)
	if err != nil {
		return r, fmt.Errorf("composing mode %q locally: %w", r.Mode, err)
	}
	r.System, r.SystemVolatile, r.Facts = stable, volatile, nil
	return r, nil
}

// turnTransport is what the turn wire must provide: the model calls plus the
// retry-notify seam.
type turnTransport interface {
	ModelProvider
	Streamer
	SetRetryNotify(fn func(attempt int, err error, delay time.Duration))
}

// conn couples the turn transport with the SDK client that serves the
// gateway-only side channels. In endpoint mode there IS no side client — the
// side channels are memcode services with no compat equivalent — so side is
// nil and the capability methods fail cleanly with ErrGatewayOnly (callers
// already degrade on error).
type conn struct {
	turn turnTransport
	side *client.Client
	ep   *Endpoint // non-nil = arbitrary-endpoint mode (no memcode backend)
}

// dial builds the gateway connection for one (url, token) pair: the memcode
// provider (the compat dialect + the memcode extensions, at {url}/v1) + the
// side-channel client.
func dial(url, token string) *conn {
	return &conn{
		turn: memcodeprov.New(memcodeprov.Config{
			BaseURL: url,
			Token:   token,
			Compose: composeDoctrine,
		}),
		side: client.New(url, token),
	}
}

// dialEndpoint builds the connection for an arbitrary endpoint. A provider's
// OWN API (api.openai.com, api.x.ai) gets its full-fidelity shared native
// adapter (native.go); everything else speaks the generic compat wire.
// Memcode:false either way — no memcode extension or opaque attach ever
// leaves the machine, and no side client at all.
func dialEndpoint(ep Endpoint) *conn {
	if t := nativeTurnTransport(ep); t != nil {
		return &conn{turn: t, ep: &ep}
	}
	return &conn{
		turn: compat.New(compat.Config{
			BaseURL: ep.BaseURL, // the FULL base as configured, incl. any path prefix
			Token:   ep.Key,     // "" = no Authorization header
			Model:   ep.Model,   // the session model when a request doesn't pin one
			Compose: composeDoctrine,
		}),
		ep: &ep,
	}
}

func (c *conn) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return c.turn.Complete(ctx, r)
}

func (c *conn) Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	return c.turn.Stream(ctx, r, h)
}

func (c *conn) WebSearch(ctx context.Context, query string) (string, error) {
	if c.side == nil {
		return "", ErrGatewayOnly
	}
	return c.side.WebSearch(ctx, query)
}

func (c *conn) WebFetch(ctx context.Context, url string) (string, error) {
	if c.side == nil {
		return "", ErrGatewayOnly
	}
	return c.side.WebFetch(ctx, url)
}

func (c *conn) Advise(ctx context.Context, question, effort string) (string, error) {
	if c.side == nil {
		return "", ErrGatewayOnly
	}
	return c.side.Advise(ctx, question, effort)
}

// Endpoint reports the arbitrary endpoint this conn dials, ok=false for the
// hosted gateway.
func (c *conn) Endpoint() (Endpoint, bool) {
	if c.ep == nil {
		return Endpoint{}, false
	}
	return *c.ep, true
}

// SetRetryNotify wires the callback into both halves (no side half in
// endpoint mode).
func (c *conn) SetRetryNotify(fn func(attempt int, err error, delay time.Duration)) {
	c.turn.SetRetryNotify(fn)
	if c.side != nil {
		c.side.SetRetryNotify(fn)
	}
}

// Compile-time capability guarantees: the conn must satisfy everything the
// runtime type-asserts on a provider.
var (
	_ ModelProvider = (*conn)(nil)
	_ Streamer      = (*conn)(nil)
	_ WebSearcher   = (*conn)(nil)
	_ WebFetcher    = (*conn)(nil)
	_ Advisor       = (*conn)(nil)
	_ retryNotifier = (*conn)(nil)
	_ Endpointer    = (*conn)(nil)

	_ turnTransport = (*compat.Transport)(nil)
)
