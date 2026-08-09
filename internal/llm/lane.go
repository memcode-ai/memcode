package llm

import (
	"strings"

	"github.com/memcode-ai/memcode/internal/wire"
)

// lane.go — the SEMANTIC LADDER: the rules that map an abstract turn intent
// (purpose, mode, risk, difficulty, thinking depth) to a LANE — a chain of
// deployment roles with a vendor-tier fallback. This is the model-selection
// doctrine that lived in the gateway (api resolve.go) until the
// all-policy-client-side migration (2026-08-08): the CLI is the agent, so the
// agent decides. Ported against parity goldens generated from the gateway
// code before its deletion (testdata/ladder_goldens.json — the port must
// reproduce every row).
//
// Doctrine (unchanged): spend intelligence only where uncertainty/risk demands
// it, and default toward CAPABLE when unsure — misrouting DOWN is the
// expensive failure.

// lane is one ladder verdict: try the deployment roles IN ORDER (first
// configured wins — the *Or chains of the old resolve.go), then fall back to
// the session vendor's tier member.
type lane struct {
	roles []string // "planner" | "reviewer" | "standard" | "classify", in preference order
	tier  string   // "frontier" | "balanced" | "cheap" — the vendor-tier fallback
}

// planEscalations are the FAILURE reasons that send a PLAN to the frontier
// tier. Mood signals (user_friction_high, room_repair) do NOT escalate the
// cheap planner: a plan is read-only and user-approved before anything
// executes. high_risk_surface is different (checked separately): it marks the
// SUBJECT MATTER as high-blast-radius, and in plan mode the plan IS the
// binding contract the cheap lane will execute — those turns draft on the
// frontier tier from turn one.
var planEscalations = map[string]bool{
	"plan_review_escalate":  true,
	"plan_synth_incomplete": true, // cheap planner couldn't produce a usable plan even after a regen
	"self_heal":             true,
}

// utilityPurposes are the invisible-plumbing purposes a pin never captures:
// the structured classifiers and the compaction/shrinkwrap machinery. They
// stay on the utility lanes so a pinned session stays fast and never burns the
// user's credits on background jobs.
var utilityPurposes = map[string]bool{"classify": true, "compact": true, "shrinkwrap": true}

// laneFor maps an Intent to its lane — a straight port of the gateway's
// ResolveModel branch structure (the roleOr(strongFallback(vendor, tier))
// chains become {roles, tier} pairs; the caller resolves them over the
// control-plane role config + the catalog tier triples).
func laneFor(it wire.Intent) lane {
	purpose := strings.TrimSpace(strings.ToLower(it.Purpose))
	if purpose == "review" {
		return lane{roles: []string{"reviewer"}, tier: "frontier"} // cross-model plan critic — its own mode, not "plan"
	}
	if it.Mode == "plan" {
		switch purpose {
		case "classify":
			return lane{roles: []string{"classify", "standard"}, tier: "balanced"} // plan-intent classify → the classifier lane
		case "explore":
			return lane{roles: []string{"standard"}, tier: "balanced"} // plan scouts → the cheap lane
		default: // executive: main_loop / synth / reflect
			if planEscalations[it.Risk] {
				return lane{tier: "frontier"} // the reviewer judged the approach wrong → re-plan on the hardest tier
			}
			if it.Risk == "high_risk_surface" {
				return lane{tier: "frontier"} // high-blast-radius surface: the plan is the binding contract → frontier from turn one
			}
			return lane{roles: []string{"planner"}, tier: "frontier"} // the planner drafts (Effort=high here is depth, not escalation)
		}
	}
	switch purpose {
	case "classify":
		return lane{roles: []string{"classify", "standard"}, tier: "cheap"} // the dedicated cheap classifier lane — separate from the cheap CODING lane
	case "explore", "route":
		// "route" has no live caller (its Purpose const is gone) but the case
		// stays: the parity goldens replay it, pinning the ported branch shape.
		return lane{roles: []string{"standard"}, tier: "cheap"} // read-only scouts → the cheap lane
	case "reflect":
		// Plan-mode reflection is a structured triage the PLANNER does on its
		// own research — never a frontier spend by default.
		return lane{roles: []string{"planner"}, tier: "frontier"}
	case "synth":
		return lane{roles: []string{"planner"}, tier: "frontier"} // non-plan synth is rare → the planner
	case "predict", "overview", "learn", "compact", "shrinkwrap":
		// "overview" has no live caller (its Purpose const is gone) but the
		// case stays for the parity goldens, like "route" above.
		return lane{roles: []string{"standard"}, tier: "balanced"} // capable, deterministic-ish background work
	}
	// main_loop (and unlabelled): the dynamic decision. The frontier tier is
	// ERROR-ONLY — the planner handles hard reasoning; the frontier returns
	// here only as the failure valves below.
	if it.Risk == "self_heal" {
		return lane{tier: "frontier"} // the agent broke its OWN edit and is stuck → strongest model to converge
	}
	// Spawned-agent tiers: a dispatched sub-agent is force-routed to the
	// strong vendor (that's the point of asking for it); a LONG-RUNNING
	// (background) agent gets the frontier tier. Checked BEFORE the generic
	// risk branch, which would otherwise drop them onto the cheap lane.
	if it.Risk == "agent_frontier" {
		return lane{tier: "frontier"}
	}
	if it.Risk == "agent_strong" || it.Risk == "agent_strong_tier" {
		return lane{tier: "balanced"} // strong vendor's everyday tier, never a role
	}
	// Legacy/override tier escalation — ONLY when no Difficulty was judged (an
	// old session, or the /effort override where the judge is skipped). A
	// judged turn separates the axes: {difficulty: standard, thinking: high}
	// stays on the standard lane.
	if it.Difficulty == "" && it.Reasoning == wire.EffortHigh {
		return lane{roles: []string{"planner"}, tier: "frontier"}
	}
	switch it.Difficulty {
	case "deep":
		// Repo-scale / architectural / root-cause work → the heavy tier from
		// turn ONE (the 44M-token audit lesson; never again).
		return lane{roles: []string{"planner"}, tier: "frontier"}
	case "lookup":
		if it.Risk == "" { // friction/risk never rides the cheap tier
			return lane{roles: []string{"standard"}, tier: "cheap"}
		}
	}
	return lane{roles: []string{"standard"}, tier: "balanced"} // ordinary work (risk included) → capable cheap
}
