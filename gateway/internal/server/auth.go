package server

// auth.go — the CORE auth layer: request → identity via the composition's
// Authenticator, then the Authorizer's door, then rate limiting. No hosted
// concepts live here (no verification service, plan/usage state, or
// usage reporting — those are the private cloud module's Authenticator/
// Authorizer/UsageSink implementations). The self-host Authenticator lives in
// extensions.go.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/memcode-ai/memcode/gateway"
	"github.com/memcode-ai/memcode/gateway/internal/identity"
)

// errInvalidToken is the generic authentication rejection.
var errInvalidToken = errors.New("invalid or revoked token")

// bearerOf extracts the Authorization bearer, "" when absent.
func bearerOf(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func asStatusError(err error) *gateway.StatusError {
	var se *gateway.StatusError
	if errors.As(err, &se) {
		return se
	}
	return nil
}

// withIdentity stamps the generic identity (from the composition's
// Authenticator + Authorizer.Access) on the request context.
func withIdentity(ctx context.Context, id *gateway.Identity, acc gateway.Access) context.Context {
	return identity.With(ctx, identity.Info{
		OrgID:        id.ID,
		UserID:       id.User,
		KeyedVendors: acc.KeyedVendors,
		LimitToKeyed: acc.LimitToKeyed,
	})
}

// orgIDFromContext reads the principal id stamped by auth(). "" if not set.
func orgIDFromContext(ctx context.Context) string {
	return identity.From(ctx).OrgID
}
