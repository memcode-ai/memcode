package theme

import "testing"

func TestPinkPantherRegistered(t *testing.T) {
	names := Names()
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	if !has("pinkpanther") {
		t.Error("pinkpanther theme not registered")
	}
	if got := Get("pinkpanther").Palette.Brand; got != "#F472B6" {
		t.Errorf("pinkpanther brand = %q, want #F472B6", got)
	}
}

// TestRandomResolvesToConcrete: Set("random") must pick a real theme (never "random" itself,
// never the light "dawn") so a persisted "random" choice yields a fresh dark theme each launch.
func TestRandomResolvesToConcrete(t *testing.T) {
	for i := 0; i < 30; i++ {
		if !Set("random") {
			t.Fatal("Set(\"random\") returned false")
		}
		if got := Active().Name; got == "random" || got == "dawn" {
			t.Fatalf("random resolved to %q (must be a concrete dark theme)", got)
		}
	}
}
