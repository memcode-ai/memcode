// Package identity carries the authenticated caller identity on the request
// context. It lives outside package server so the provider layer can read it
// without an import cycle — per-principal key routing resolves from exactly
// this, never from anything client-supplied. The fields are GENERIC: who the
// caller is, which vendors they hold keys for, and whether serving is limited
// to those keyed vendors. What POPULATES them (a local token, or a hosted
// verification service) is the composition's business — this package encodes
// no commercial concepts.
package identity

import "context"

// Info is the verified identity of one request, stamped by the auth
// middleware from the composition's Authenticator + Authorizer.
type Info struct {
	// OrgID / UserID scope the principal (a tenant + a user within it; both
	// "selfhost" in the default composition). They are the key-lookup scope
	// and the rate-limit / usage dimension.
	OrgID  string
	UserID string

	// KeyedVendors are the vendors this principal holds serving keys for via
	// the composition's KeySource. The presence gate: keys are resolved only
	// for vendors listed here.
	KeyedVendors []string

	// LimitToKeyed means serving must use a keyed vendor — a non-keyed lane
	// refuses rather than serving on the gateway's own keys. Always false in
	// the default (self-host) composition; a hosted composition sets it (e.g.
	// a hosted composition's usage limit). The refusal is the wire's insufficient_credits code.
	LimitToKeyed bool
}

// HasKey reports whether the principal has a serving key for the vendor.
func (i Info) HasKey(vendor string) bool {
	for _, v := range i.KeyedVendors {
		if v == vendor {
			return true
		}
	}
	return false
}

type ctxKey struct{}

// With stamps the identity on the context.
func With(ctx context.Context, info Info) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

// From reads the identity stamped by the auth middleware. The zero Info means
// "not authenticated" — callers on authed paths can rely on OrgID being set.
func From(ctx context.Context) Info {
	info, _ := ctx.Value(ctxKey{}).(Info)
	return info
}
