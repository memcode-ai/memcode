package provider

// Credential-resolution tests: gcpSAKey stays GATEWAY-side (the adapter takes
// resolved bytes; the env policy lives here).

import (
	"encoding/base64"
	"testing"
)

// gcpSAKey honors "base64 or raw JSON" and rejects the deploy "placeholder" seed
// (so the gemini vendor isn't advertised when no real key is set).
func TestGCPSAKeyDecoding(t *testing.T) {
	rawJSON := `{"type":"service_account","project_id":"p"}`
	t.Run("raw json", func(t *testing.T) {
		t.Setenv(EnvGCPSAKey, rawJSON)
		if string(gcpSAKey()) != rawJSON {
			t.Errorf("raw JSON not passed through: %q", gcpSAKey())
		}
	})
	t.Run("base64", func(t *testing.T) {
		t.Setenv(EnvGCPSAKey, base64.StdEncoding.EncodeToString([]byte(rawJSON)))
		if string(gcpSAKey()) != rawJSON {
			t.Errorf("base64 not decoded to JSON: %q", gcpSAKey())
		}
	})
	t.Run("placeholder rejected", func(t *testing.T) {
		t.Setenv(EnvGCPSAKey, "placeholder")
		if gcpSAKey() != nil {
			t.Errorf("placeholder must read as unset, got %q", gcpSAKey())
		}
	})
}
