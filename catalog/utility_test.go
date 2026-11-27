package catalog

import "testing"

// Helper calls (classify/authorize, compact, shrinkwrap) are forced-tool calls
// that run on every turn. A helper label that no longer names a served catalog
// model fails every turn even while the pinned model works (gpt-oss-120b,
// 2026-11-27), so every helper label must resolve, belong to the vendor it is
// listed under, and accept a forced tool choice.
func TestHelperModelsResolveToTheirVendor(t *testing.T) {
	check := func(where, vendor, label string) {
		t.Helper()
		m, ok := LookupModel(label)
		if !ok {
			t.Errorf("%s: helper label %q is not a catalog model", where, label)
			return
		}
		if vendor != "" && m.Vendor != vendor {
			t.Errorf("%s: helper %q belongs to vendor %q, listed under %q", where, label, m.Vendor, vendor)
		}
		if m.NoForcedToolChoice {
			t.Errorf("%s: helper %q rejects forced tool_choice, which every classifier call sends", where, label)
		}
	}
	if UtilityModel() == "" {
		t.Fatal("utility_model is empty")
	}
	check("utility_model", "", UtilityModel())
	for vendor, label := range modelCatalog.file.VendorUtilityModels {
		check("vendor_utility_models", vendor, label)
	}
	for vendor, label := range modelCatalog.file.UtilityModels {
		check("utility_models", vendor, label)
	}
	// Every vendor with a pinnable model gets a helper of its own, so no pin
	// sends its helper calls to a vendor the user didn't pick.
	for _, m := range CatalogModels() {
		if m.Pinnable && VendorUtilityModel(m.Vendor) == "" {
			t.Errorf("pinnable %q has no vendor_utility_models entry for vendor %q", m.Label, m.Vendor)
		}
	}
}
