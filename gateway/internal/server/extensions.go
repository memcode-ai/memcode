package server

// extensions.go — step 1 of the core/cloud split: the server accepts a
// gateway.Extensions set. The self-host Authenticator lives here (local
// shared token → one local identity). Cloud implementations (verify,
// entitlement, usage, keys) are NOT in this repo — they compose from the
// private gateway-cloud module. Until that module exists, New() keeps the
// current in-tree cloud path so behavior and every test are unchanged; the
// injected Authenticator is consumed first when present.

import (
	"crypto/subtle"
	"net/http"

	"github.com/memcode-ai/memcode/gateway"
)

// LocalTokenAuthenticator authenticates the single shared self-host bearer as
// the one local identity. Exported through gateway/serve for compositions.
func LocalTokenAuthenticator(token string) gateway.Authenticator {
	return selfHostAuthenticator{token}
}

type selfHostAuthenticator struct{ token string }

func (a selfHostAuthenticator) Authenticate(r *http.Request) (*gateway.Identity, error) {
	got := bearerOf(r)
	if a.token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1 {
		return &gateway.Identity{ID: "selfhost", User: "selfhost"}, nil
	}
	return nil, errInvalidToken
}
