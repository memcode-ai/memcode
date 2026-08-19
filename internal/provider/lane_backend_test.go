package provider

import "testing"

// LaneBackendVendor must resolve EVERY subscription endpoint name to its real
// vendor — "copilot" used to come back ("", "sub", true) because the lookup
// went through ServingLabel ("github"), which is not an alias-map key.
func TestLaneBackendVendor(t *testing.T) {
	cases := []struct {
		backend string
		vendor  string
		kind    string
		ok      bool
	}{
		{"claude-sub", "anthropic", "sub", true},
		{"codex", "openai", "sub", true},
		{"copilot", "openai", "sub", true},
		{"grok-sub", "grok", "sub", true},
		{"ownkey:anthropic", "anthropic", "ownkey", true},
		{"ownkey:openai", "openai", "ownkey", true},
		{"memcode", "", "", false},
		{"cheap", "", "", false},
	}
	for _, c := range cases {
		vendor, kind, ok := LaneBackendVendor(c.backend)
		if vendor != c.vendor || kind != c.kind || ok != c.ok {
			t.Errorf("LaneBackendVendor(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.backend, vendor, kind, ok, c.vendor, c.kind, c.ok)
		}
	}
}
