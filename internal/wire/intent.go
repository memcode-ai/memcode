package wire

// Intent is the abstract selection contract: the session layer expresses WHAT a
// turn is, and the CLI's selection policy (internal/llm/resolve.go) maps it to a
// concrete catalog label. It is a client-internal type — it never rides a wire.
//
// It used to carry the Automatic ladder's inputs (Difficulty, Risk, Vendor), and
// resolving them into a model was a per-turn decision. That is gone: there is
// exactly one model per session, the user's pin, and Intent now only says what
// KIND of turn this is so that internal plumbing can be told apart from the
// user's own work.
type Intent struct {
	// Purpose is what this turn is FOR (ledger attribution + the utility split):
	// "main_loop" | "explore" | "classify" | "compact" | …
	Purpose string `json:"purpose,omitempty"`

	// Mode names the doctrine the turn runs under ("chat" | "exec" | "plan" | …).
	Mode string `json:"mode,omitempty"`

	// Reasoning is the abstract reasoning depth the turn warrants (maps to
	// Effort). Effort is a genuinely separate axis from model choice and
	// survives the ladder: a tricky single-file bug can want deep thinking on
	// the same model an easy one uses.
	Reasoning Effort `json:"reasoning,omitempty"`

	// Interactive is true when a live user is present (vs a batch/background
	// job) — available to the policy for latency-vs-cost trade-offs.
	Interactive bool `json:"interactive,omitempty"`

	// Pin is the session's model: a catalog LABEL ("sol", "sonnet", "glm-5p2",
	// …), never a raw provider id — raw ids stay at the serving edge. The pin
	// resolver settles it once at session start from the
	// session -> workspace -> user -> default_model chain, so it is always set
	// for a real session; selection refuses rather than inventing one if it
	// isn't.
	//
	// Every real request serves this model. Internal plumbing
	// (classify/compact/shrinkwrap) rides the catalog's utility_model instead,
	// and capability gaps fail the turn visibly rather than rerouting it.
	Pin string `json:"pin,omitempty"`
}
