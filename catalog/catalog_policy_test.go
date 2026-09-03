package catalog

import "testing"

// The catalog is the CLI's routing control plane now (all-policy-client-side):
// selection reads vendor identity, tier triples, and fallback chains straight
// from models.json. These tests keep that data internally consistent — a
// catalog edit that dangles a label fails here, not in a live session.

func TestCatalogVendorsDeclared(t *testing.T) {
	for _, m := range CatalogModels() {
		if m.Vendor == "" {
			t.Errorf("model %q (%s) has no vendor — selection policy needs authoritative vendor identity", m.Label, m.ID)
		}
	}
}

// TestCatalogTiersResolve is replaced: the `tiers` block it validated is gone.
// What must hold now is that the two SINGLE models the catalog names — the
// first-run seed and the internal-plumbing model — actually exist and are
// servable.
func TestDefaultAndUtilityModelsResolve(t *testing.T) {
	for _, tc := range []struct{ what, label string }{
		{"default_model", DefaultModel()},
		{"utility_model", UtilityModel()},
	} {
		if tc.label == "" {
			t.Errorf("%s is unset — the catalog must name one", tc.what)
			continue
		}
		m, ok := LookupModel(tc.label)
		if !ok {
			t.Errorf("%s = %q, which is not in the catalog", tc.what, tc.label)
			continue
		}
		if m.Window <= 0 {
			t.Errorf("%s (%s) declares no context window", tc.what, tc.label)
		}
		if p := ModelPricing(tc.label); p.Input <= 0 {
			t.Errorf("%s (%s) has no input price — it would meter at $0", tc.what, tc.label)
		}
	}
	// The seed is what a brand-new user lands on, so it must be one they could
	// also have picked themselves.
	if m, ok := LookupModel(DefaultModel()); ok && !m.Pinnable {
		t.Errorf("default_model %q is not pinnable — a user could never choose it back", DefaultModel())
	}
}

func TestCatalogFallbackChainsResolve(t *testing.T) {
	for _, m := range CatalogModels() {
		seen := map[string]bool{m.Label: true}
		for _, fb := range m.Fallback {
			if _, ok := LookupModel(fb); !ok {
				t.Errorf("model %q fallback %q names no catalog model", m.Label, fb)
			}
			if seen[fb] {
				t.Errorf("model %q fallback chain revisits %q", m.Label, fb)
			}
			seen[fb] = true
		}
	}
}

// TestTierAltitude is DELETED with TierAltitude: it named which rung of its
// vendor's frontier/balanced/cheap triple a model occupied, which only mattered
// for remapping an Automatic pick to the same "altitude" on another vendor.

// A fallback must fail DIFFERENTLY from the thing it covers.
//
// A same-vendor first hop shares the provider, the adapter and the request
// semantics, so it survives only a single-model outage and multiplies every
// other kind of failure. That is not theoretical: gemini-flash used to fall
// back to gemini-pro, and when a bug in the shared Gemini adapter rejected
// every tool-using turn, the chain dutifully reproduced the same failure on the
// second model.
//
// Later hops may return to the same vendor — by then the independent one has
// already been tried.
func TestFallbackFirstHopLeavesTheVendor(t *testing.T) {
	for _, m := range CatalogModels() {
		fb := FallbackChain(m.Label)
		if len(fb) == 0 {
			continue
		}
		first, ok := LookupModel(fb[0])
		if !ok {
			t.Errorf("%s falls back to %q, which is not in the catalog — a chain naming a "+
				"model that does not exist silently shortens the safety net", m.Label, fb[0])
			continue
		}
		if first.Vendor == m.Vendor {
			t.Errorf("%s (%s) falls back first to %s, the SAME vendor. A fallback that shares "+
				"the provider and adapter cannot cover an outage or a bug in either — point the "+
				"first hop at a different vendor.", m.Label, m.Vendor, fb[0])
		}
	}
}
