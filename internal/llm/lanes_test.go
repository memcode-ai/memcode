package llm

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

func subLane(vendor string) provider.LaneInfo {
	return provider.LaneInfo{Vendor: vendor, Name: vendor + "-lane", Kind: "sub"}
}
func keyLane(vendor string) provider.LaneInfo {
	return provider.LaneInfo{Vendor: vendor, Name: "ownkey:" + vendor, Kind: "ownkey"}
}

// The steering table: sub lanes are the preferred $0 path for AUTOMATIC
// main-loop turns; pins, explicit vendors, utility purposes, and fireworks
// owners are immune; with no lanes everything is byte-identical steerLabel.
func TestSubSteering(t *testing.T) {
	base := prodInfo(nil) // the parity fixture: full servable set, no byok

	cases := []struct {
		name    string
		lanes   []provider.LaneInfo
		gateway bool
		label   string // ladder's pick before steering
		vendor  string // session vendor ("" = Automatic)
		purpose string
		want    string
	}{
		{"no lanes is steerLabel", nil, true, "sol", "", "main_loop", "sol"},
		{"openai pick steers to claude sub", []provider.LaneInfo{subLane("anthropic")}, true, "sol", "", "main_loop", "opus"},
		{"same family stays", []provider.LaneInfo{subLane("anthropic")}, true, "opus", "", "main_loop", "opus"},
		{"fireworks passthrough", []provider.LaneInfo{subLane("anthropic")}, true, "glm-5p2", "", "main_loop", "glm-5p2"},
		{"explicit vendor immune", []provider.LaneInfo{subLane("anthropic")}, true, "sol", "openai-explicit", "main_loop", "sol"},
		{"utility immune", []provider.LaneInfo{subLane("anthropic")}, true, "sol", "", "classify", "sol"},
		{"balanced altitude maps", []provider.LaneInfo{subLane("anthropic")}, true, "terra", "", "main_loop", "sonnet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := applyLaneFacts(base, tc.lanes, tc.gateway)
			vendor := ""
			if tc.vendor == "openai-explicit" {
				// any non-default vendor counts as explicit intent
				vendor = "grok"
				tc.want = tc.label
			}
			got := steerLabelWithLanes(tc.label, vendor, tc.purpose, info)
			if got != tc.want {
				t.Fatalf("steer(%s) = %q, want %q", tc.label, got, tc.want)
			}
		})
	}
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
	if len(info.Roles) != 0 {
		t.Fatal("gateway roles must not survive the clamp")
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
func TestZeroCreditsPrefersSubLane(t *testing.T) {
	base := prodInfo(nil)
	base.CreditsExhausted = true
	info := applyLaneFacts(base, []provider.LaneInfo{subLane("anthropic")}, true)
	it := wire.Intent{Purpose: string(MainLoop), Mode: "exec"}
	rq := wire.Request{Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}}}
	res := resolveHosted(it, rq, info)
	if got := res.label; got != "sonnet" {
		t.Fatalf("label = %q, want sonnet (sub balanced member)", got)
	}
	if res.reason != "credits_sub" && res.reason != "" {
		t.Fatalf("reason = %q", res.reason)
	}
}
