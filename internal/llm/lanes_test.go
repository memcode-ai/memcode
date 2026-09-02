package llm

import (
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/provider"
)

// prodInfo is a production-shaped control-plane snapshot: every windowed
// catalog model servable, with an optional BYOK-keyed vendor set. It moved here
// when parity_test.go (the Automatic ladder's 6,608 golden rows) was deleted —
// the credential-lane tests below still need a realistic servable set.
func prodInfo(byok []string) provider.ModelsInfo {
	keyed := map[string]bool{}
	for _, v := range byok {
		keyed[v] = true
	}
	info := provider.ModelsInfo{
		Backend: "hybrid",
		Vendors: []string{"openai", "anthropic", "gemini", "grok"},
	}
	for _, m := range catalog.CatalogModels() {
		if m.Window <= 0 {
			continue
		}
		info.Models = append(info.Models, provider.ModelFact{
			Label: m.Label, Vendor: m.Vendor, Window: m.Window,
			Vision: m.Vision, PDF: m.PDF, Pinnable: m.Pinnable,
			Byok: keyed[m.Vendor],
		})
	}
	return info
}

func subLane(vendor string) provider.LaneInfo {
	return provider.LaneInfo{Vendor: vendor, Name: vendor + "-lane", Kind: "sub"}
}
func keyLane(vendor string) provider.LaneInfo {
	return provider.LaneInfo{Vendor: vendor, Name: "ownkey:" + vendor, Kind: "ownkey"}
}

// Signed-out lanes-only: the servable set clamps to attached vendors and the
// first lane's vendor becomes the default; roles are dropped.
func TestSignedOutLaneClamp(t *testing.T) {
	info := applyLaneFacts(prodInfo(nil), []provider.LaneInfo{subLane("anthropic")}, false)
	for _, f := range info.Models {
		if f.Vendor != "anthropic" {
			t.Fatalf("clamped set contains %s (%s)", f.Label, f.Vendor)
		}
	}
	if info.DefaultVendor() != "anthropic" {
		t.Fatalf("DefaultVendor = %q, want anthropic", info.DefaultVendor())
	}
}

// Own-key lanes merge into the BYOK keyed set (sub > key handled at
// transport dispatch; policy just sees the vendor as funded/keyed).
func TestOwnKeyLaneMergesIntoKeyedSet(t *testing.T) {
	info := applyLaneFacts(prodInfo(nil), []provider.LaneInfo{keyLane("anthropic")}, true)
	if !info.ByokVendorSet()["anthropic"] {
		t.Fatal("ownkey lane vendor missing from keyed set")
	}
	if len(info.SubVendors) != 0 {
		t.Fatal("ownkey lane must not count as a sub vendor")
	}
}

// $0 fundability: with credits exhausted and a sub attached, an AUTOMATIC
// off-family selection remaps to the sub's balanced member with the
// credits_sub reason (subs outrank keyed remaps).
// TestZeroCreditsPrefersSubLane is DELETED. It asserted that at $0 an
// AUTOMATIC selection preferred a subscription vendor's tier member. Selection
// no longer picks vendors — the pin does — so there is nothing to prefer. Which
// CREDENTIAL pays for the pinned model's vendor is a separate question, still
// answered by the credential lanes exercised in the tests above.
