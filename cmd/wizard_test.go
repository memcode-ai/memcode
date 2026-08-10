package cmd

import (
	"os"
	"testing"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/provider"
)

func TestParseChoice(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want int
	}{
		{"", 5, -1}, {"1", 5, 0}, {"5", 5, 4}, {"6", 5, -1},
		{"0", 5, -1}, {"x", 5, -1}, {" 2 ", 5, 1},
	}
	for _, c := range cases {
		if got := parseChoice(c.in, c.n); got != c.want {
			t.Errorf("parseChoice(%q,%d) = %d, want %d", c.in, c.n, got, c.want)
		}
	}
}

// The wizard must not run when a backend is already configured — any of these
// signals means the user is set up and shouldn't be prompted.
func TestHasBackend(t *testing.T) {
	clear := func() {
		for _, k := range []string{provider.EnvAPIToken, provider.EnvCredentialSource, provider.EnvEndpointURL, "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
			t.Setenv(k, "")
		}
	}
	cfg := &config.Config{}

	clear()
	if hasBackend(cfg) {
		t.Error("no signals set → hasBackend should be false")
	}
	for _, k := range []string{provider.EnvAPIToken, provider.EnvCredentialSource, provider.EnvEndpointURL, "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		clear()
		t.Setenv(k, "value")
		if !hasBackend(cfg) {
			t.Errorf("%s set → hasBackend should be true", k)
		}
	}
}

// The onboarding marker gates the one-time wizard.
func TestOnboardedMarker(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if onboarded() {
		t.Fatal("fresh config dir must not be onboarded")
	}
	markOnboarded()
	if !onboarded() {
		t.Fatal("after markOnboarded, onboarded must be true")
	}
	if _, err := os.Stat(onboardedMarkerPath()); err != nil {
		t.Errorf("marker file missing: %v", err)
	}
}
