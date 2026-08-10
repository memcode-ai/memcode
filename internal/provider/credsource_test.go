package provider

import "testing"

// An exported provider key, with no memcode login and no explicit endpoint, is
// a CONNECTED own-key backend on the native vendor adapter — the account-free
// first turn. It must never outrank a real login or a configured endpoint.
func TestOwnKeyBackendSelection(t *testing.T) {
	t.Run("exported anthropic key selects the native anthropic backend", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-own")
		l := NewFromEnvLazy()
		if !l.Connected() {
			t.Fatal("an exported provider key must be a connected state")
		}
		ep, ok := l.Endpoint()
		if !ok || ep.BaseURL != "https://api.anthropic.com" || ep.Key != "sk-ant-own" {
			t.Fatalf("own-key endpoint resolved wrong: %+v (ok=%v)", ep, ok)
		}
		c := l.c.Load()
		if _, isNative := c.turn.(*nativeShim); !isNative {
			t.Errorf("own-key mode on a vendor host must use the native adapter, got %T", c.turn)
		}
		if c.side != nil {
			t.Error("own-key mode has NO memcode side channel")
		}
	})

	t.Run("a memcode token outranks an exported key", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-own")
		t.Setenv(EnvAPIToken, "memcode_tok")
		l := NewFromEnvLazy()
		if _, ok := l.Endpoint(); ok {
			t.Error("a memcode login must win over an ambient key")
		}
		if l.c.Load().side == nil {
			t.Error("the winning hosted backend must keep its side channel")
		}
	})

	t.Run("an explicit endpoint outranks an exported key", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-own")
		t.Setenv(EnvEndpointURL, "http://localhost:11434/v1")
		ep, ok := resolveEndpoint(nil)
		if !ok || ep.BaseURL != "http://localhost:11434/v1" {
			t.Fatalf("an explicit MEMCODE_ENDPOINT_URL must win: %+v (ok=%v)", ep, ok)
		}
	})

	t.Run("no key, no backend", func(t *testing.T) {
		clearBackendEnv(t)
		if _, ok := discoverCredentialEndpoint(); ok {
			t.Error("discovery must find nothing in an empty environment")
		}
	})
}
