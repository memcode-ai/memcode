package tools

import (
	"encoding/json"
	"testing"
)

// The tool registry rides EVERY model call — its wire size (names + descriptions
// + schemas, as JSON) is baseline context spent before the user says a word.
// Measured 2026-07-13: core ~26.7KB (~6.7k tokens) / browser ~7.0KB. The
// ceilings sit ~15% above that: hitting one means new tools or fatter
// descriptions grew the baseline — trim, or consciously raise the ceiling in
// the same commit that justifies it.
func TestToolDefsWireBudget(t *testing.T) {
	core, err := json.Marshal(Defs())
	if err != nil {
		t.Fatal(err)
	}
	if len(core) > 31_000 {
		t.Errorf("core tool defs = %dB (~%d tokens) on the wire — over the 31KB budget; trim descriptions/schemas or raise deliberately", len(core), len(core)/4)
	}
	browser, err := json.Marshal(BrowserDefs())
	if err != nil {
		t.Fatal(err)
	}
	if len(browser) > 8_000 {
		t.Errorf("browser tool defs = %dB (~%d tokens) — over the 8KB budget", len(browser), len(browser)/4)
	}
}
