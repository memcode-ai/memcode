package server

import (
	"net/http"

	"github.com/memcode-ai/memcode/gateway"
)

// testAuth is the tests' composition Authenticator: any non-empty bearer is a
// fixed local identity; empty is refused (so auth-required tests still 401).
type testAuth struct{}

func (testAuth) Authenticate(r *http.Request) (*gateway.Identity, error) {
	if bearerOf(r) == "" {
		return nil, errInvalidToken
	}
	return &gateway.Identity{ID: "org-test", User: "user-test"}, nil
}

// newCoreHandler builds the handler with the test authenticator (the core
// serving path; cloud behavior is tested in the gateway-cloud module).
func newCoreHandler(cfg Config) http.Handler {
	return NewWith(cfg, gateway.Extensions{Authenticator: testAuth{}})
}
