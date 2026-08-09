package provider

import (
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// The gateway must NEVER expose the inference vendor (the provider path) to the client. This
// guards every wire surface: SanitizeModelID, SanitizeResponse, and ConfiguredModels.
func TestSanitizeNeverLeaksProvider(t *testing.T) {
	if got := SanitizeModelID("accounts/fireworks/models/glm-5p1"); got != "glm-5p1" {
		t.Errorf("SanitizeModelID = %q, want glm-5p1", got)
	}

	resp := &wire.Response{
		Model:          "accounts/fireworks/models/glm-5p1",
		RequestedModel: "accounts/fireworks/models/kimi-k2p6",
		Backend:        "fireworks", // internal tag — must not ride the wire
	}
	SanitizeResponse(resp)
	for _, v := range []string{resp.Model, resp.RequestedModel, resp.Backend} {
		if strings.Contains(v, "/") || strings.Contains(strings.ToLower(v), "fireworks") {
			t.Errorf("SanitizeResponse left provider identity: %q", v)
		}
	}
	if resp.Backend != "cheap" {
		t.Errorf("Backend = %q, want the vendor-neutral wire value \"cheap\"", resp.Backend)
	}

	// Even when the configured roles ARE Fireworks ids, /v1/models must return clean names —
	// and the catalog labels mean known models report their short name (luna), not the raw id.
	SetModels("accounts/fireworks/models/glm-5p1", "gpt-5.6-luna", "accounts/fireworks/models/glm-5p1")
	defer SetModels("", "", "")
	for _, rm := range ConfiguredModels() {
		if strings.Contains(rm.ID, "/") || strings.Contains(strings.ToLower(rm.Label), "fireworks") {
			t.Errorf("ConfiguredModels leaked a provider path: %+v", rm)
		}
		if rm.Role == "reviewer" && rm.Label != "luna" {
			t.Errorf("reviewer should report its catalog label 'luna', got %q", rm.Label)
		}
	}
}

// Every catalog model must declare a label, so its client-facing name comes from the catalog
// (never a fallback that could expose a raw vendor id).
func TestEveryModelHasLabel(t *testing.T) {
	for id, spec := range reg.byID {
		if spec.Label == "" {
			t.Errorf("model %q has no label in models.json — add one so its short name is explicit", id)
		}
	}
}
