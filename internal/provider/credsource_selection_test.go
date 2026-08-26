package provider

import "testing"

// Subscription sources are user-facing labels for family lanes.
func TestServingLabels(t *testing.T) {
	cases := map[string]string{
		"claude-sub": "claude",
		"codex":      "codex",
		"copilot":    "github",
		"grok-sub":   "grok",
		"ollama":     "ollama", // custom endpoints keep their own name
	}
	for in, want := range cases {
		if got := ServingLabel(in); got != want {
			t.Errorf("ServingLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSubscriptionEndpointName(t *testing.T) {
	for _, name := range []string{"claude-sub", "codex", "copilot", "grok-sub"} {
		if !SubscriptionEndpointName(name) {
			t.Errorf("SubscriptionEndpointName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "anthropic", "ollama", "memcode"} {
		if SubscriptionEndpointName(name) {
			t.Errorf("SubscriptionEndpointName(%q) = true, want false", name)
		}
	}
}

func TestExplicitCredentialSource(t *testing.T) {
	t.Setenv(EnvCredentialSource, "")
	if ExplicitCredentialSource() {
		t.Fatal("empty source reported as explicit")
	}
	t.Setenv(EnvCredentialSource, "claude")
	if !ExplicitCredentialSource() {
		t.Fatal("selected source not reported as explicit")
	}
}

func TestSelectedSourceUnresolvedWhenSignedOut(t *testing.T) {
	// A selected source with no live login must be reported, not swallowed —
	// this is the boot warning's trigger. (No real Claude/Codex login exists
	// in the test environment, so resolution fails by construction.)
	t.Setenv(EnvCredentialSource, "claude")
	t.Setenv("HOME", t.TempDir()) // no ~/.claude login
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	src, bad := SelectedSourceUnresolved()
	if !bad || src != "claude" {
		t.Fatalf("SelectedSourceUnresolved() = (%q, %v), want (claude, true)", src, bad)
	}
	t.Setenv(EnvCredentialSource, "")
	if _, bad := SelectedSourceUnresolved(); bad {
		t.Fatal("no source selected but reported unresolved")
	}
}

func TestLazySelectedSubscriptionIsLaneNotEndpoint(t *testing.T) {
	restoreResolve := resolveSourceFn
	restoreDial := dialLane
	defer func() {
		resolveSourceFn = restoreResolve
		dialLane = restoreDial
	}()
	resolveSourceFn = func(source string) (Endpoint, bool) {
		if source != "claude" {
			return Endpoint{}, false
		}
		return Endpoint{Name: "claude-sub", BaseURL: "https://api.anthropic.com", Key: "oauth", Model: "claude-sonnet-5"}, true
	}
	dialLane = func(ep Endpoint) *conn { return &conn{ep: &ep} }

	t.Setenv(EnvCredentials, "claude")
	t.Setenv(EnvAPIToken, "memcode_x")
	withGateway := NewFromEnvLazy()
	if ep, ok := withGateway.Endpoint(); ok {
		t.Fatalf("Endpoint() = %+v, true; subscription must be a lane beside the gateway", ep)
	}
	if lanes := withGateway.Lanes(); len(lanes) != 1 || lanes[0].Name != "claude-sub" {
		t.Fatalf("Lanes() = %+v, want claude-sub lane", lanes)
	}
	if !withGateway.GatewayPresent() {
		t.Fatal("gateway token should remain the base while subscription is attached as a lane")
	}

	t.Setenv(EnvAPIToken, "")
	laneOnly := NewFromEnvLazy()
	if ep, ok := laneOnly.Endpoint(); ok {
		t.Fatalf("lane-only Endpoint() = %+v, true; /model would open the endpoint picker again", ep)
	}
	if lanes := laneOnly.Lanes(); len(lanes) != 1 || lanes[0].Name != "claude-sub" {
		t.Fatalf("lane-only Lanes() = %+v, want claude-sub lane", lanes)
	}
	if !laneOnly.Connected() {
		t.Fatal("lane-only session must be connected")
	}
	if laneOnly.GatewayPresent() {
		t.Fatal("lane-only session must not report a gateway base")
	}
}

func TestLazyMemcodeAccountKeepsAllSubscriptionLanes(t *testing.T) {
	restoreResolve := resolveSourceFn
	restoreDial := dialLane
	defer func() {
		resolveSourceFn = restoreResolve
		dialLane = restoreDial
	}()
	resolveSourceFn = func(source string) (Endpoint, bool) {
		switch source {
		case "claude":
			return Endpoint{Name: "claude-sub", BaseURL: "https://api.anthropic.com", Key: "claude-oauth", Model: "claude-sonnet-5"}, true
		case "codex":
			return Endpoint{Name: "codex", BaseURL: "https://chatgpt.com/backend-api/codex", Key: "codex-oauth", Model: "gpt-5.6-terra"}, true
		default:
			return Endpoint{}, false
		}
	}
	dialLane = func(ep Endpoint) *conn { return &conn{ep: &ep} }

	t.Setenv(EnvCredentials, "claude,codex")
	t.Setenv(EnvAPIToken, "memcode_x")
	l := NewFromEnvLazy()
	if ep, ok := l.Endpoint(); ok {
		t.Fatalf("Endpoint() = %+v, true; subscriptions must not replace the memcode account backend", ep)
	}
	if !l.GatewayPresent() {
		t.Fatal("memcode account must remain the base backend")
	}
	lanes := l.Lanes()
	if len(lanes) != 2 {
		t.Fatalf("Lanes() = %+v, want claude and codex lanes", lanes)
	}
	want := map[string]string{"claude-sub": "anthropic", "codex": "openai"}
	for _, ln := range lanes {
		if want[ln.Name] != ln.Vendor {
			t.Fatalf("unexpected lane %+v in %+v", ln, lanes)
		}
		delete(want, ln.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing lanes: %+v from %+v", want, lanes)
	}
}
