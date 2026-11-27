package catalog

import "testing"

// ResolveEffort maps memcode's universal scale onto each model's native levels:
// exact when supported, otherwise the nearest level, ties going up.
func TestResolveEffort(t *testing.T) {
	for _, tc := range []struct{ model, level, want string }{
		{"opus", "max", "max"},              // Claude 5.x supports all five
		{"opus", "xhigh", "xhigh"},          //
		{"grok-4.7", "max", "xhigh"},        // Grok tops out at xhigh
		{"gemini-pro", "max", "high"},       // Carbon tops out at high
		{"gemini-pro", "xhigh", "high"},     //
		{"gemini-flash", "max", "xhigh"},    // Hydrogen adds xhigh
		{"glm-5p3", "low", "high"},          // GLM runs low/medium as high
		{"glm-5p3", "medium", "high"},       //
		{"glm-5p3", "xhigh", "max"},         // tie between high and max goes up
		{"kimi-k3p1-pro", "medium", "high"}, // tie between low and high goes up
		{"qwen3p8-max", "low", "high"},      // one level: everything maps to it
		{"opus", "", ""},                    // off passes through
		{"opus", "default", "default"},      // provider default passes through
		{"no-such-model", "max", "max"},     // no declared levels: pass through
		{"opus", "bogus", "bogus"},          // unknown level: pass through
	} {
		if got := ResolveEffort(tc.model, tc.level); got != tc.want {
			t.Errorf("ResolveEffort(%q, %q) = %q, want %q", tc.model, tc.level, got, tc.want)
		}
	}
}

// Every pinnable reasoning model declares its native levels, each on the scale,
// lowest first. Without a list the picker can't hide redundant levels and the
// effective level shown to the user would be a guess.
func TestPinnableModelsDeclareEfforts(t *testing.T) {
	for _, m := range CatalogModels() {
		if !m.Pinnable || !m.Reasoning {
			continue
		}
		if len(m.Efforts) == 0 {
			t.Errorf("pinnable reasoning model %q declares no efforts", m.Label)
			continue
		}
		prev := -1
		for _, l := range m.Efforts {
			i := effortIndex(l)
			if i < 0 {
				t.Errorf("%q: effort %q is not on the scale %v", m.Label, l, EffortScale)
			}
			if i <= prev {
				t.Errorf("%q: efforts %v must be unique and lowest first", m.Label, m.Efforts)
			}
			prev = i
		}
	}
}
