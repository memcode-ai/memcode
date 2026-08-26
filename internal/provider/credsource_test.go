package provider

import "testing"

// An exported provider key, with no memcode login and no explicit endpoint, is
// a CONNECTED own-key lane on the native vendor adapter — the account-free
// first turn. It must never outrank a real login or a configured endpoint.
func TestOwnKeyBackendSelection(t *testing.T) {
	t.Run("exported anthropic key attaches a native anthropic lane", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-own")
		l := NewFromEnvLazy()
		if !l.Connected() {
			t.Fatal("an exported provider key must be a connected state")
		}
		if ep, ok := l.Endpoint(); ok {
			t.Fatalf("own-key lane must not put the TUI in endpoint mode: %+v", ep)
		}
		lanes := l.laneSet()
		if len(lanes) != 1 || lanes[0].vendor != "anthropic" || lanes[0].kind != laneOwnKey {
			t.Fatalf("own-key lanes = %+v, want one anthropic own-key lane", l.Lanes())
		}
		if lanes[0].ep.BaseURL != "https://api.anthropic.com" || lanes[0].ep.Key != "sk-ant-own" {
			t.Fatalf("own-key lane endpoint resolved wrong: %+v", lanes[0].ep)
		}
		if _, isNative := lanes[0].c.turn.(*nativeShim); !isNative {
			t.Errorf("own-key lane on a vendor host must use the native adapter, got %T", lanes[0].c.turn)
		}
		if lanes[0].c.side != nil {
			t.Error("own-key mode has NO memcode side channel")
		}
	})

	t.Run("a memcode token becomes the base beside an exported key lane", func(t *testing.T) {
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
		if lanes := l.Lanes(); len(lanes) != 1 || lanes[0].Kind != "ownkey" || lanes[0].Vendor != "anthropic" {
			t.Fatalf("hosted session should keep the exported key as a family lane, got %+v", lanes)
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

	// A source must carry a default model so the first turn works with zero
	// configuration; MEMCODE_ENDPOINT_MODEL overrides it.
	t.Run("own key gets a default model, overridable", func(t *testing.T) {
		clearBackendEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-own")
		ep, ok := discoverCredentialEndpoint()
		if !ok || ep.Model == "" {
			t.Fatalf("own-key endpoint must default a model: %+v", ep)
		}
		t.Setenv(EnvEndpointModel, "claude-opus-5")
		ep, _ = discoverCredentialEndpoint()
		if ep.Model != "claude-opus-5" {
			t.Errorf("MEMCODE_ENDPOINT_MODEL must override the default, got %q", ep.Model)
		}
	})
}
