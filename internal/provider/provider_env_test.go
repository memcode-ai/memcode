package provider

import (
	"strings"
	"testing"
)

// The CLI must work with ONLY a token: the gateway endpoint defaults to
// production and MEMCODE_API_URL is a dev override, never a requirement.
func TestAPIURLDefaultsToProduction(t *testing.T) {
	t.Setenv(EnvAPIURL, "")
	if got := APIURL(); got != DefaultAPIURL {
		t.Fatalf("APIURL() = %q, want %q", got, DefaultAPIURL)
	}
	t.Setenv(EnvAPIURL, "http://127.0.0.1:8080")
	if got := APIURL(); got != "http://127.0.0.1:8080" {
		t.Fatalf("APIURL() ignored the override: %q", got)
	}
}

func TestNewFromEnvRequiresOnlyToken(t *testing.T) {
	t.Setenv(EnvAPIURL, "")
	t.Setenv(EnvAPIToken, "")
	t.Setenv(EnvEndpointURL, "") // a dev-exported endpoint must not turn the no-backend case into a success
	if _, err := NewFromEnv(); err == nil || !strings.Contains(err.Error(), EnvAPIToken) {
		t.Fatalf("want a missing-token error, got %v", err)
	}
	t.Setenv(EnvAPIToken, "memcode_test")
	if _, err := NewFromEnv(); err != nil {
		t.Fatalf("token set but NewFromEnv failed: %v", err)
	}
}
