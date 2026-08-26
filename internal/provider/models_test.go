package provider

import (
	"context"
	"testing"
)

func TestAvailablePinsCatalogFallbackIncludesQwen38Max(t *testing.T) {
	t.Setenv(EnvAPIToken, "")
	pins := AvailablePins(context.Background())
	for _, p := range pins {
		if p.Label != "qwen3p8-max" {
			continue
		}
		if p.Name != "Qwen3.8 Max" || p.Group != "Qwen" || p.Window != 262_144 {
			t.Fatalf("qwen3p8-max pin metadata = %+v", p)
		}
		return
	}
	t.Fatalf("qwen3p8-max missing from catalog-backed /model pins: %+v", pins)
}
