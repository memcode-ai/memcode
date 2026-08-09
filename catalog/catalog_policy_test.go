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

func TestCatalogTiersResolve(t *testing.T) {
	vendors := TierVendors()
	if len(vendors) == 0 {
		t.Fatal("catalog declares no tier triples — the CLI's vendor-tier resolution would be empty")
	}
	for _, v := range vendors {
		for _, alt := range []string{"frontier", "balanced", "cheap"} {
			lbl := VendorTier(v, alt)
			if lbl == "" {
				t.Errorf("tiers[%s][%s] is unset", v, alt)
				continue
			}
			m, ok := LookupModel(lbl)
			if !ok {
				t.Errorf("tiers[%s][%s] = %q names no catalog model", v, alt, lbl)
				continue
			}
			if m.Vendor != v {
				t.Errorf("tiers[%s][%s] = %q belongs to vendor %q — a triple must stay within its vendor", v, alt, lbl, m.Vendor)
			}
		}
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

func TestTierAltitude(t *testing.T) {
	cases := map[string]string{
		"terra": "balanced", "sol": "frontier", "luna": "cheap",
		"sonnet": "balanced", "glm-5p2": "cheap", "kimi-k3": "frontier",
		"fable": "", // pin-only: outside its vendor's triple
	}
	for lbl, want := range cases {
		if got := TierAltitude(lbl); got != want {
			t.Errorf("TierAltitude(%q) = %q, want %q", lbl, got, want)
		}
	}
}
