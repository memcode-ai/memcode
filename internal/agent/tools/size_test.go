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
// Raised 32KB -> 33KB (2026-09-12) for the `task` tool, which turns recurring
// work into a durable autonomous task. Its description was trimmed twice and
// two inferable fields (operation/target) dropped from the schema first. What
// remains is the trigger phrasing ("keep dependencies current", "watch for
// advisories") and the clarifying_questions instruction — the first is what
// makes the tool fire on the requests users actually type, and the second is
// what stops it guessing at scope and side effects on their behalf.
//
// Raised 31KB -> 32KB (2026-09-03) for the `policy` tool. Its description was
// trimmed twice first; what is left is the guardrail sentence ("call only when
// they ask — never on your own judgement"), which is the part that stops the
// tool being used to re-invent per-task model routing and is therefore the last
// thing that should be cut for bytes. The target list came out instead: `show`
// names every target, and a wrong one returns the valid set.
//
// Raised 33KB -> 34KB (2026-09-12) for the automation contract on `task`:
// does / success_criteria / side_effects / just_completed. These are not
// decoration. They are what the user signs off on before a task is allowed to
// run unattended, phrased as behaviour rather than as the YAML underneath, and
// there is no way to obtain them other than asking the model for them at
// creation time. `just_completed` pays for itself immediately: without it,
// boxing up work that was just done successfully runs the whole job a second
// time to prove something already proven.
//
// Raised 34KB -> 35KB (2026-09-12) for scope: projects / coordination /
// project_discovery / verify_across. An automation belongs to the scope of the
// RESPONSIBILITY, not to the repository the conversation happened in, and there
// is no cheaper place to establish that. Without these fields a cross-cutting
// concern (memcode's model catalog spans two repos) becomes a task attached to
// whichever checkout was open, which then keeps half the product current and
// reports success.
//
// Raised 35KB -> 36KB (2026-09-12) for `revise`. It is what makes Create,
// Customize, Edit and resolving a pause one operation instead of four flows
// that would drift apart — one validating, another not; one showing the
// contract, another a YAML diff. Its length is carrying the rule that an
// automation which paused resumes on evidence, not on having been answered.
func TestToolDefsWireBudget(t *testing.T) {
	core, err := json.Marshal(Defs())
	if err != nil {
		t.Fatal(err)
	}
	if len(core) > 36_000 {
		t.Errorf("core tool defs = %dB (~%d tokens) on the wire — over the 36KB budget; trim descriptions/schemas or raise deliberately", len(core), len(core)/4)
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
