package provider

import "testing"

// TestModelCatalog validates the embedded registry: it parses (the package var would have
// panicked otherwise), every configured role points at a real cataloged model, and the
// capability lookups (vision/window) are correct — including the SAFE default for an unknown id.
func TestModelCatalog(t *testing.T) {
	// Every role in config.json must reference a model present in models.json. A typo here would
	// silently give the model a zero spec (vision off → images escalate; window 0 → no guard).
	for role, id := range reg.roles {
		if _, ok := reg.byID[id]; !ok {
			t.Errorf("config.json role %q -> %q is not in the model catalog (models.json)", role, id)
		}
	}
	for _, r := range []string{"planner", "reviewer", "standard"} {
		if reg.role(r) == "" {
			t.Errorf("role %q is unset in config.json", r)
		}
	}

	// Capability lookups by id.
	if s := reg.spec("accounts/fireworks/models/minimax-m3"); !s.Vision || s.Window == 0 {
		t.Errorf("minimax-m3 should be vision-capable with a window, got %+v", s)
	}
	if s := reg.spec("accounts/fireworks/models/deepseek-v4-pro"); s.Vision {
		t.Errorf("deepseek-v4-pro should be vision=false, got %+v", s)
	}
	// An unknown id must return the zero spec — fails SAFE (no vision, no overflow guard bypass).
	if s := reg.spec("accounts/fireworks/models/does-not-exist"); s.Vision || s.Window != 0 {
		t.Errorf("unknown id must be a zero spec (vision off, window 0), got %+v", s)
	}
}

// TestPinnableCatalog pins the /model picker contract: every pinnable row must be fully
// specified (label = the pin namespace, group = the picker family, window = the budget
// meter), and the label index must round-trip back to the same entry. The un-pinnable
// utility/retired rows must stay out of the picker.
func TestPinnableCatalog(t *testing.T) {
	pinnableCount := 0
	for id, s := range reg.byID {
		if !s.Pinnable {
			continue
		}
		pinnableCount++
		if s.Label == "" || s.Group == "" || s.Window == 0 {
			t.Errorf("pinnable model %q needs label+group+window, got %+v", id, s)
		}
		got, ok := reg.specByLabel(s.Label)
		if !ok || got.ID != id {
			t.Errorf("specByLabel(%q) = (%+v, %v), want id %q", s.Label, got, ok, id)
		}
	}
	if pinnableCount < 12 {
		t.Errorf("expected the full pinnable lineup (>=12 models), got %d", pinnableCount)
	}
	for _, label := range []string{"gpt-oss-120b", "glm-5p1", "deepseek-v4-pro", "minimax-m3"} {
		if s, ok := reg.specByLabel(label); ok && s.Pinnable {
			t.Errorf("%s must not be pinnable (utility/retired/not-yet-live)", label)
		}
	}
	// An unknown label falls through to Automatic — the lookup must miss, not zero-value match.
	if _, ok := reg.specByLabel("no-such-label"); ok {
		t.Error("unknown label must not resolve")
	}
}
