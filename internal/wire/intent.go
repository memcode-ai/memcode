package wire

// Intent is the abstract selection contract: the session layer expresses WHAT a
// turn is, and the CLI's selection policy (cli/internal/llm: laneFor + the
// resolver) maps intent → concrete catalog label. It is a client-internal type
// now — it never rides a wire. The abstraction survives the policy's move from
// the gateway because it is what keeps call sites from naming models: purposes
// and modes are stable vocabulary; labels and tiers are catalog data.
type Intent struct {
	// Purpose is what this turn is FOR (ledger attribution + lane selection):
	// "main_loop" | "explore" | "classify" | "compact" | …
	Purpose string `json:"purpose,omitempty"`

	// Mode names the doctrine the turn runs under ("chat" | "exec" | "plan" |
	// …) — plan mode has its own lane branch.
	Mode string `json:"mode,omitempty"`

	// Reasoning is the abstract reasoning depth the turn warrants (maps to Effort).
	Reasoning Effort `json:"reasoning,omitempty"`

	// Difficulty is the TIER demand the turn was judged to have — a separate axis
	// from Reasoning (thinking depth): a tricky single-file bug can be
	// {standard, high-thinking}. Judged by the turn_intent classifier on the
	// classify lane: "lookup" (short read-only retrieval → cheap tier) |
	// "standard" (ordinary work) | "deep" (repo-scale/architectural/root-cause →
	// heavy tier). Empty = unjudged (judge unavailable) — the ladder falls back
	// to Reasoning-based escalation.
	Difficulty string `json:"difficulty,omitempty"`

	// Risk is the session layer's escalation signal — what only it observes:
	// "self_heal" | "user_friction_high" | … (folded from RoutingHint.Reason).
	Risk string `json:"risk,omitempty"`

	// Interactive is true when a live user is present (vs a batch/background
	// job) — available to the policy for latency-vs-cost trade-offs.
	Interactive bool `json:"interactive,omitempty"`

	// Vendor is the per-session strong-tier override: "" = the configured
	// default, or one of "openai" | "anthropic" | "gemini" | "grok". It names a
	// VENDOR, not a model — the ladder still resolves the tier (frontier/
	// balanced/cheap) within that vendor from the catalog's tier triples. Set by
	// the CLI's /model selector; BYOK steering may prefer a keyed vendor when
	// none is configured.
	Vendor string `json:"vendor,omitempty"`

	// Pin is the user's explicit model choice from /model: a catalog LABEL
	// ("sol", "sonnet", "glm-5p2", …), never a raw provider id — raw ids stay at
	// the serving edge. "" = Automatic (the routing doctrine decides). When set
	// and valid, every real request serves this model; invisible plumbing
	// (classify/compact/shrinkwrap) stays on the utility lanes, and an
	// unknown/un-pinnable label falls through to Automatic so a stale pin never
	// breaks a session. Pins are never coerced — capability gaps fail typed, not
	// silently rerouted.
	Pin string `json:"pin,omitempty"`
}
