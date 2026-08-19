package provider

import "testing"

// The wizard's explicit subscription choice must never be silently shadowed:
// not by a stored memcode login, not by a stale config endpoint. These pin the
// selection rules that make "✓ using your claude subscription" true.
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
	t.Setenv("HOME", t.TempDir())          // no ~/.claude login
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
