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
//
// Raised 31KB -> 32KB (2026-09-03) for the `policy` tool. Its description was
// trimmed twice first; what is left is the guardrail sentence ("call only when
// they ask — never on your own judgement"), which is the part that stops the
// tool being used to re-invent per-task model routing and is therefore the last
// thing that should be cut for bytes. The target list came out instead: `show`
// names every target, and a wrong one returns the valid set.
func TestToolDefsWireBudget(t *testing.T) {
	core, err := json.Marshal(Defs())
	if err != nil {
		t.Fatal(err)
	}
	if len(core) > 32_000 {
		t.Errorf("core tool defs = %dB (~%d tokens) on the wire — over the 32KB budget; trim descriptions/schemas or raise deliberately", len(core), len(core)/4)
	}
	browser, err := json.Marshal(BrowserDefs())
	if err != nil {
		t.Fatal(err)
	}
	// Raised 8KB→9KB when the suite grew 17→20 tools (wait/upload/resize);
	// the per-tool average must stay lean.
	if len(browser) > 9_000 {
		t.Errorf("browser tool defs = %dB (~%d tokens) — over the 9KB budget", len(browser), len(browser)/4)
	}
}
