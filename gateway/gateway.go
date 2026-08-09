// Package gateway is the embeddable serving core: the OpenAI-compat /v1
// surface (chat completions, the models control plane, advisor and web
// tools) over the shared provider adapters, with a handful of small,
// generic extension points.
//
// The doctrine this package encodes: the core carries the PRODUCT — serving,
// translation, the wire's typed error vocabulary — and nothing that exists
// only because someone operates a paid service around it. Anything
// service-shaped (who may call, what they may spend, where usage reports go,
// where per-user provider keys live) enters through the interfaces below.
// The in-tree implementations are the self-host ones: a local shared token,
// allow-everything authorization, usage as stdout JSON lines, provider keys
// from the environment. A hosted operator imports this package and supplies
// its own implementations; the composition is a Go dependency, never a fork.
//
// The extension API is v0 and may change between minor releases.
package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/memcode-ai/memcode/gateway/internal/identity"
)

// Identity is IDENTITY ONLY — who a request runs as. No capability,
// billing, or policy state rides on it (see Access).
type Identity struct {
	// ID is the stable principal id (an org, a deploy key, or "selfhost").
	ID string
	// User sub-scopes ID when the principal distinguishes users ("" = none).
	User string
}

// Access is the authorization/routing state the serving mechanism and the
// /v1/models control plane consume — separate from identity, produced by
// the Authorizer. The core maps it onto the wire's field names
// (credits_exhausted, per-model byok flags) as pure protocol translation;
// what makes the values true is the composition's business.
type Access struct {
	// KeyedVendors are vendors this identity holds serving keys for via the
	// KeySource ("anthropic", "openai", …). Drives the per-model byok flags
	// on /v1/models and the keyed-first serving preference.
	KeyedVendors []string
	// LimitToKeyed means serving should require keyed vendors (the wire's
	// credits_exhausted fact). Always false in the default composition.
	LimitToKeyed bool
}

// Keyed reports whether vendor is in KeyedVendors.
func (a Access) Keyed(vendor string) bool {
	for _, v := range a.KeyedVendors {
		if v == vendor {
			return true
		}
	}
	return false
}

// Authenticator turns a request into an identity. A nil *Identity or a
// non-nil error refuses the request (401). Implementations own their own
// caching.
type Authenticator interface {
	Authenticate(r *http.Request) (*Identity, error)
}

// Authorizer approves work for an authenticated identity. Errors returned
// from Authorize/GateVendor surface as the wire's typed responses (the
// error VOCABULARY — 402 insufficient_credits and friends — is protocol and
// lives in the core; the policy deciding to raise them lives in the
// implementation).
type Authorizer interface {
	// Authorize is the door: called once per request after authentication.
	Authorize(r *http.Request, id *Identity) error
	// GateVendor is called on the serving path before a vendor serves a
	// non-keyed call.
	GateVendor(ctx context.Context, id *Identity, vendor string) error
	// Access reports the identity's routing/capability state.
	Access(id *Identity) Access
}

// UsageEvent is the OPERATIONALLY GENERIC record of one served call:
// counts, models, latency. It deliberately carries no pricing or
// commercial accounting — a composition's sink derives that on its own side.
type UsageEvent struct {
	RequestID      string
	Purpose        string
	RequestedModel string
	ServedModel    string
	Backend        string
	InputTokens    int
	OutputTokens   int
	CacheRead      int
	CacheWrite     int
	SearchCount    int
	LatencyMS      int64
	// ViaKeySource marks a call served on a key the KeySource supplied for
	// this identity (rather than the gateway's own environment keys).
	ViaKeySource bool
	KeyVendor    string
}

// UsageSink receives each served call's usage, after the response
// (asynchronous, best-effort). The core always also emits its stdout JSON
// usage line; a sink is for operators who need the event elsewhere.
type UsageSink interface {
	Record(id *Identity, u UsageEvent)
}

// KeySource supplies per-identity provider keys — the mechanism behind the
// wire's byok lanes. Nil means the environment keys are the only keys.
type KeySource interface {
	Key(ctx context.Context, org, user, vendor string) (key, version string, err error)
	MarkInvalid(org, user, vendor string)
}

// StatusError lets an Authorizer map a refusal onto a specific wire status +
// code (e.g. a hosted composition's 402 insufficient_credits). A plain error
// from Authorize is a 403.
type StatusError struct {
	Status int
	Code   string
	Msg    string
}

func (e *StatusError) Error() string { return e.Msg }

// ErrKeyInvalid is returned by KeySource.Key when the key was recently proven
// bad (a vendor auth rejection) — the serving path fast-fails with the same
// message instead of re-burning a vendor call.
var ErrKeyInvalid = errors.New("key recently rejected by the vendor")

// Route mounts an operator-specific endpoint on the server (composed via
// gateway/serve.New). Authed routes run behind the Authenticator/Authorizer
// like every core route.
type Route struct {
	Pattern string // e.g. "POST /v1/byok"
	Handler http.Handler
	Authed  bool
}

// Option configures New.
type Option func(*Extensions)

// Extensions is the resolved option set (exported for the internal server's
// consumption; construct via Options).
type Extensions struct {
	Authenticator Authenticator
	Authorizer    Authorizer
	UsageSink     UsageSink
	KeySource     KeySource
	Routes        []Route
}

func WithAuthenticator(a Authenticator) Option { return func(e *Extensions) { e.Authenticator = a } }
func WithAuthorizer(a Authorizer) Option       { return func(e *Extensions) { e.Authorizer = a } }
func WithUsageSink(s UsageSink) Option         { return func(e *Extensions) { e.UsageSink = s } }
func WithKeySource(k KeySource) Option         { return func(e *Extensions) { e.KeySource = k } }
func WithRoutes(rs ...Route) Option {
	return func(e *Extensions) { e.Routes = append(e.Routes, rs...) }
}

// Resolve applies opts over the self-host defaults.
func Resolve(opts ...Option) Extensions {
	e := Extensions{Authorizer: AllowAll{}}
	for _, o := range opts {
		o(&e)
	}
	if e.Authorizer == nil {
		e.Authorizer = AllowAll{}
	}
	return e
}

// AllowAll is the self-host Authorizer: everything an authenticated
// identity asks for is permitted, and no routing limits apply.
type AllowAll struct{}

func (AllowAll) Authorize(*http.Request, *Identity) error            { return nil }
func (AllowAll) GateVendor(context.Context, *Identity, string) error { return nil }
func (AllowAll) Access(*Identity) Access                             { return Access{} }

// ── public accessors for composition-supplied routes ────────────────────────
// A hosted composition mounting its own routes (e.g. managed key management)
// needs the request's identity and the byok vendor roster without importing
// the core's internal packages. These thin accessors expose exactly that.

// IdentityOf returns the principal + user + keyed vendors stamped on the
// request by the auth middleware (empty strings before auth).
func IdentityOf(r *http.Request) (org, user string, keyedVendors []string) {
	info := identity.From(r.Context())
	return info.OrgID, info.UserID, info.KeyedVendors
}
