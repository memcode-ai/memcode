package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	compat "github.com/memcode-ai/memcode/internal/providers/compat"
	"github.com/memcode-ai/memcode/internal/wire"
)

// reqPing is a minimal request for signed-out failure checks.
func reqPing() wire.Request {
	return wire.Request{Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("ping")}}}}
}

// clearBackendEnv strips every backend-selection input so each case states its
// own world exactly.
func clearBackendEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvAPIToken, EnvAPIURL, EnvEndpointURL, EnvEndpointKey, EnvEndpointModel} {
		t.Setenv(k, "")
	}
	// Ambient own-key sources select a backend too — clear them so each case
	// states its world exactly.
	for _, v := range ownKeyVendors {
		t.Setenv(v.env, "")
	}
}

// TestBackendSelectionMatrix pins the Phase C selection order in both env
// constructors: memcode token → hosted; else configured endpoint (caller's
// resolved one first, env second) → the compat transport with no side client;
// else signed out (Lazy) / error (NewFromEnv).
func TestBackendSelectionMatrix(t *testing.T) {
	t.Run("token wins over endpoint", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv(EnvAPIToken, "memcode_tok")
		t.Setenv(EnvEndpointURL, "http://localhost:11434/v1")
		l := NewFromEnvLazy()
		if !l.Connected() {
			t.Fatal("token must connect")
		}
		if _, ok := l.Endpoint(); ok {
			t.Error("a memcode login must select the hosted backend, not the endpoint")
		}
		if l.c.Load().side == nil {
			t.Error("hosted mode must keep the side-channel client")
		}
	})

	t.Run("endpoint from env", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv(EnvEndpointURL, "http://localhost:11434/v1")
		t.Setenv(EnvEndpointKey, "sk-local")
		t.Setenv(EnvEndpointModel, "mistral:latest")
		l := NewFromEnvLazy()
		if !l.Connected() {
			t.Fatal("a configured endpoint is a CONNECTED state (Phase C widening)")
		}
		ep, ok := l.Endpoint()
		if !ok {
			t.Fatal("endpoint mode must report its endpoint")
		}
		if ep.BaseURL != "http://localhost:11434/v1" || ep.Key != "sk-local" || ep.Model != "mistral:latest" || ep.Name != "localhost:11434" {
			t.Errorf("env endpoint resolved wrong: %+v", ep)
		}
		c := l.c.Load()
		if _, isCompat := c.turn.(*compat.Transport); !isCompat {
			t.Errorf("endpoint mode must serve turns on the compat transport, got %T", c.turn)
		}
		if c.side != nil {
			t.Error("endpoint mode must have NO side-channel client (no memcode surface)")
		}
	})

	t.Run("resolved endpoint beats env endpoint", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv(EnvEndpointURL, "http://env-endpoint/v1")
		l := NewFromEnvLazy(Endpoint{Name: "cfg", BaseURL: "http://cfg-endpoint/v1"})
		ep, ok := l.Endpoint()
		if !ok || ep.BaseURL != "http://cfg-endpoint/v1" {
			t.Errorf("the caller's RESOLVED endpoint must win (it already merged env), got %+v ok=%v", ep, ok)
		}
	})

	t.Run("neither is signed out", func(t *testing.T) {
		clearBackendEnv(t)
		l := NewFromEnvLazy()
		if l.Connected() {
			t.Fatal("no token, no endpoint must boot signed out")
		}
		if _, ok := l.Endpoint(); ok {
			t.Error("signed out must not report an endpoint")
		}
		if _, err := l.Complete(context.Background(), reqPing()); !errors.Is(err, ErrNotLoggedIn) {
			t.Errorf("signed-out calls must fail ErrNotLoggedIn, got %v", err)
		}
	})

	t.Run("NewFromEnv mirrors the order", func(t *testing.T) {
		clearBackendEnv(t)
		if _, err := NewFromEnv(); err == nil {
			t.Fatal("no backend must error")
		}
		t.Setenv(EnvEndpointURL, "http://localhost:11434/v1")
		p, err := NewFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.(*conn).turn.(*compat.Transport); !ok || p.(*conn).side != nil {
			t.Errorf("NewFromEnv endpoint mode wrong: turn=%T side=%v", p.(*conn).turn, p.(*conn).side)
		}
		t.Setenv(EnvAPIToken, "memcode_tok")
		p, err = NewFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.(*conn).Endpoint(); ok {
			t.Error("a token must select hosted even with an endpoint configured")
		}
	})
}

// TestEndpointSideChannelsAbsent: the gateway-only capabilities fail cleanly
// (never panic on the missing side client) so callers degrade — webfetch falls
// back to the local fetch, the advisor prints its unavailable line.
func TestEndpointSideChannelsAbsent(t *testing.T) {
	clearBackendEnv(t)
	t.Setenv(EnvEndpointURL, "http://localhost:11434/v1")
	l := NewFromEnvLazy()
	ctx := context.Background()
	if _, err := l.WebSearch(ctx, "q"); !errors.Is(err, ErrGatewayOnly) {
		t.Errorf("WebSearch must fail ErrGatewayOnly, got %v", err)
	}
	if _, err := l.WebFetch(ctx, "https://example.com"); !errors.Is(err, ErrGatewayOnly) {
		t.Errorf("WebFetch must fail ErrGatewayOnly, got %v", err)
	}
	if _, err := l.Advise(ctx, "q", "high"); !errors.Is(err, ErrGatewayOnly) {
		t.Errorf("Advise must fail ErrGatewayOnly, got %v", err)
	}
	// The retry-notify seam must tolerate the missing side client too.
	l.SetRetryNotify(func(int, error, time.Duration) {})
}

// TestLoginLogoutSwapsBackends: /login swaps endpoint mode → hosted;
// /logout (ClearCredentials) falls BACK to the configured endpoint instead of
// dead air — and to signed-out when none is configured.
func TestLoginLogoutSwapsBackends(t *testing.T) {
	clearBackendEnv(t)
	t.Setenv(EnvEndpointURL, "http://localhost:11434/v1")
	l := NewFromEnvLazy()
	if _, ok := l.Endpoint(); !ok {
		t.Fatal("boot must select the endpoint")
	}
	l.SetCredentials("https://code.memcode.ai", "memcode_fresh")
	if _, ok := l.Endpoint(); ok {
		t.Fatal("login must swap onto the hosted gateway")
	}
	l.ClearCredentials()
	ep, ok := l.Endpoint()
	if !ok || ep.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("logout must fall back to the configured endpoint, got %+v ok=%v", ep, ok)
	}
	if !l.Connected() {
		t.Fatal("endpoint fallback is a connected state")
	}

	clearBackendEnv(t)
	l2 := NewFromEnvLazy()
	l2.SetCredentials("https://code.memcode.ai", "memcode_fresh")
	l2.ClearCredentials()
	if l2.Connected() {
		t.Fatal("logout without a configured endpoint must be signed out")
	}
}

// TestEndpointModels: GET {base}/models with the configured key, and the
// /v1/models retry for bases configured without the prefix. Errors are nil
// (the picker falls back to free text), never failures.
func TestEndpointModels(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"mistral:latest"},{"id":"openhermes:latest"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	// Base WITH the prefix: straight hit.
	ids := EndpointModels(ctx, Endpoint{BaseURL: srv.URL + "/v1", Key: "sk-x"})
	if !reflect.DeepEqual(ids, []string{"mistral:latest", "openhermes:latest"}) {
		t.Errorf("models = %v", ids)
	}
	if gotAuth != "Bearer sk-x" {
		t.Errorf("models fetch must carry the endpoint key, got %q", gotAuth)
	}
	// Base WITHOUT the prefix: {base}/models 404s, {base}/v1/models retried.
	if ids := EndpointModels(ctx, Endpoint{BaseURL: srv.URL}); !reflect.DeepEqual(ids, []string{"mistral:latest", "openhermes:latest"}) {
		t.Errorf("prefixless base must retry /v1/models, got %v", ids)
	}
	// Keyless fetch sends no Authorization header.
	gotAuth = "sentinel"
	_ = EndpointModels(ctx, Endpoint{BaseURL: srv.URL + "/v1"})
	if gotAuth != "" {
		t.Errorf("keyless models fetch must send no Authorization header, got %q", gotAuth)
	}
	// A dead endpoint returns nil, not an error surface.
	if ids := EndpointModels(ctx, Endpoint{BaseURL: "http://127.0.0.1:1/v1"}); ids != nil {
		t.Errorf("unreachable endpoint must yield nil, got %v", ids)
	}
}

// TestCatalogWindowAndKnows: known catalog ids get real windows; local ids
// (ollama tags) get 0/false — the blank-column and tokens-only-cost contract.
func TestCatalogWindowAndKnows(t *testing.T) {
	if w := CatalogWindow("sonnet"); w <= 0 {
		t.Errorf("cataloged label must have a window, got %d", w)
	}
	if w := CatalogWindow("openhermes:latest"); w != 0 {
		t.Errorf("unknown local model must report window 0 (blank column), got %d", w)
	}
	if !CatalogKnows("sonnet") || CatalogKnows("openhermes:latest") {
		t.Error("CatalogKnows must be exact-catalog membership")
	}
}

// Conventional provider keys: pointing the CLI at a well-known provider cloud
// picks up the ecosystem-standard env var when no explicit key is configured;
// explicit keys always win; local/unknown hosts stay keyless.
func TestConventionalKeyFallback(t *testing.T) {
	clearBackendEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-conventional")

	t.Setenv(EnvEndpointURL, "https://api.openai.com/v1")
	ep, ok := EndpointFromEnv()
	if !ok || ep.Key != "sk-conventional" {
		t.Fatalf("openai endpoint must adopt OPENAI_API_KEY: %+v", ep)
	}

	// Explicit key wins.
	t.Setenv(EnvEndpointKey, "sk-explicit")
	ep, _ = EndpointFromEnv()
	if ep.Key != "sk-explicit" {
		t.Fatalf("explicit key must win: %+v", ep)
	}

	// Local endpoints stay keyless.
	t.Setenv(EnvEndpointKey, "")
	t.Setenv(EnvEndpointURL, "http://localhost:11434/v1")
	ep, _ = EndpointFromEnv()
	if ep.Key != "" {
		t.Fatalf("local endpoint must stay keyless: %+v", ep)
	}
}

// Vendor-aware /models listing auth: Anthropic wants x-api-key +
// anthropic-version; the Gemini API wants ?key=; OpenAI-compat hosts keep the
// Bearer; keyless stays bare.
func TestShapeListAuth(t *testing.T) {
	hdr := http.Header{}
	u := shapeListAuth("https://api.anthropic.com/v1/models", "sk-a", hdr)
	if u != "https://api.anthropic.com/v1/models" ||
		hdr.Get("x-api-key") != "sk-a" || hdr.Get("anthropic-version") == "" || hdr.Get("Authorization") != "" {
		t.Fatalf("anthropic shaping wrong: %q %v", u, hdr)
	}

	hdr = http.Header{}
	u = shapeListAuth("https://generativelanguage.googleapis.com/v1beta/models", "gk", hdr)
	if u != "https://generativelanguage.googleapis.com/v1beta/models?key=gk" || len(hdr) != 0 {
		t.Fatalf("gemini shaping wrong: %q %v", u, hdr)
	}

	hdr = http.Header{}
	u = shapeListAuth("http://localhost:11434/v1/models", "k", hdr)
	if hdr.Get("Authorization") != "Bearer k" {
		t.Fatalf("default shaping wrong: %v", hdr)
	}
	if shapeListAuth("http://localhost:11434/v1/models", "", http.Header{}) != "http://localhost:11434/v1/models" {
		t.Fatal("keyless must stay bare")
	}
	_ = u
}

func TestFetchEndpointModelsSendsShapedAuth(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	t.Cleanup(srv.Close)
	ids, err := fetchEndpointModels(context.Background(), srv.URL+"/v1/models", "sk-x")
	if err != nil || len(ids) != 1 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	if got.Get("Authorization") != "Bearer sk-x" {
		t.Fatalf("default auth = %q", got.Get("Authorization"))
	}
}
