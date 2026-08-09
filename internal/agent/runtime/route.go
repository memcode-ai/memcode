package runtime

import (
	"regexp"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/room"
	"github.com/memcode-ai/memcode/internal/wire"
)

// route.go carries the turn's deterministic ROUTING FACTS: the escalation
// signals only the session layer can observe (healing, room friction, high-risk
// surfaces) and the high-risk classifier. The hint's Reason becomes Intent.Risk,
// an input to the CLI's own selection ladder (llm/lane.go). How much a turn
// should SPEND — thinking depth and tier — is an LLM judgment now (the
// turn_intent judge, turnintent.go): "is this request hard?" was keyword lists
// here once, and a repo-wide audit routed like an ordinary turn. Determinism
// stays for facts; judgment goes to a model.

// frictionEscalates reports active friction/repair/correction — the live room
// signals that mean "stop cutting corners." Single-sourced so the MODEL axis
// (modelForTurn keeps the capable model) and the TIER axis (routingHintForTurn
// keeps the ladder off the cheap lane) escalate on exactly the same evidence. Friction
// is scored from THIS prompt and decays as an EMA over the next few turns, so it
// self-resolves; we don't wait for full anger before escalating.
func frictionEscalates(rm room.State) bool {
	return rm.Friction == "elevated" || rm.Friction == "high" ||
		rm.Mode == room.Repair || rm.Mode == room.Replan || rm.Intent == room.Correcting
}

// frictionReason names the SPECIFIC signal that tripped frictionEscalates, so the hint
// the user sees (· hint:…) and the usage log reflect reality — an "elevated" room
// (the agent should slow down / clarify) must not claim "high" (the user is angry). The
// ladder escalates on Risk being NON-EMPTY, not on this exact value (only planEscalations
// matches a reason literally, and none of these are in it), so the string is free to be
// honest. Strongest signal first. Only called when frictionEscalates is already true.
func frictionReason(rm room.State) string {
	switch {
	case rm.Friction == "high":
		return "user_friction_high"
	case rm.Friction == "elevated":
		return "user_friction_elevated"
	case rm.Mode == room.Repair:
		return "room_repair"
	case rm.Mode == room.Replan:
		return "room_replan"
	case rm.Intent == room.Correcting:
		return "user_correcting"
	default:
		return "user_friction" // unreachable (guarded by frictionEscalates); a safe non-empty fallback
	}
}

// routingHintForTurn derives the escalation signal only this layer can observe
// (ROUTING.md, the 80/20 gambit). The selection ladder (llm/lane.go) reads it as
// Intent.Risk and keeps such turns off the cheap lane. Returns nil when nothing
// argues for escalation (the common case → default routing). Triggers, in
// priority order:
//
//   - healing: the agent's own last edit broke a file and it's fixing forward — a
//     concrete FAILURE valve: a failed attempt must bounce to the strong model.
//     This is the keystone that makes routing the 80% to the cheap model safe —
//     outcomes find the 20%.
//   - friction: the user is frustrated / correcting course / the room went sideways —
//     high-stakes recovery, exactly when to spend the money.
//   - highRisk: the turn touches a high-blast-radius surface (auth/billing/secrets,
//     destructive/migration ops) — a wrong cheap-model edit there is a breach, a
//     charge, or data loss, not a re-prompt. Pre-computed at turn classification
//     (highRiskTurn) and passed in, so the loop carries no text.
//
// (The reasoning-heavy ~20% — review, debug, architecture, broad refactor — escalates
// WITHOUT a hint: those turns carry Effort, which the ladder routes up on.)
// turnRoutingHint is the per-turn escalation signal. A STRONG-tier agent (forceEscalate)
// keeps every request on the strong tier regardless of content — that's the whole point
// of running on the strong model. Otherwise it's the normal heal/friction/risk hint.
func (s *Session) turnRoutingHint() *wire.RoutingHint {
	// A LONG-RUNNING (background/detached) agent gets the frontier tier; a
	// foreground strong agent gets the strong vendor's balanced tier — the
	// ladder maps these reasons in lane.go.
	if s.forceFrontier {
		return escalate("agent_frontier")
	}
	if s.forceEscalate {
		return escalate("agent_strong")
	}
	return routingHintForTurn(s.room, s.turn.healRounds > 0, s.turnHighRisk)
}

func routingHintForTurn(rm room.State, healing, highRisk bool) *wire.RoutingHint {
	switch {
	case healing:
		return escalate("self_heal")
	case frictionEscalates(rm):
		return escalate(frictionReason(rm))
	case highRisk:
		return escalate("high_risk_surface")
	}
	return nil
}

func escalate(reason string) *wire.RoutingHint {
	return &wire.RoutingHint{Reason: reason}
}

// shellCommandsIn returns the parts of a message the user marked as actual COMMANDS — text
// inside `backtick` / ```fenced``` spans, and lines that open with a `$ ` or `> ` shell prompt.
// Only these get handed to the shell classifier; prose is never parsed as a command, so a plan
// that merely NAMES "rm -rf, force-push" (ops to GUARD against) is not mistaken for running them.
func shellCommandsIn(text string) []string {
	var cmds []string
	// Backtick spans: capture whatever sits between runs of backticks (handles inline `cmd`
	// and ```fenced``` blocks alike — a leading language tag like "bash" just classifies Safe).
	for i := 0; i < len(text); i++ {
		if text[i] != '`' {
			continue
		}
		j := i
		for j < len(text) && text[j] == '`' {
			j++
		}
		end := strings.IndexByte(text[j:], '`')
		if end < 0 {
			break
		}
		if span := strings.TrimSpace(text[j : j+end]); span != "" {
			cmds = append(cmds, span)
		}
		k := j + end
		for k < len(text) && text[k] == '`' {
			k++
		}
		i = k - 1
	}
	// Shell-prompt lines: `$ cmd` / `> cmd`.
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "$ ") || strings.HasPrefix(t, "> ") {
			if c := strings.TrimSpace(t[2:]); c != "" {
				cmds = append(cmds, c)
			}
		}
	}
	return cmds
}

// riskSurfaces touch security/auth/billing/secrets — high COST when wrong, but the
// words appear constantly in a codebase ABOUT those things ("what does the auth token
// do" is a lookup). So they escalate only ALONGSIDE an edit verb (write intent), never
// on a bare read. Whole-word match (via wordRe), paired with editishWords.
var riskSurfaces = wordSet(
	"auth", "oauth", "login", "password", "credential", "credentials", "secret", "secrets",
	"jwt", "rbac", "billing", "payment", "payments", "stripe", "invoice", "charge",
	"subscription", "refund", "payout", "migration", "migrations",
)

// highRiskTurn reports a turn whose blast radius warrants the strong model regardless
// of how simple it looks (ROUTING.md escalation categories). Deterministic and biased
// AGAINST false positives so it doesn't sink the 80/20 in a security-heavy codebase: a
// destructive phrase escalates alone; a risk surface escalates only with edit intent.
func highRiskTurn(text string) bool {
	// Destructive ops escalate ONLY when the message carries an ACTUAL command (a `$`/backtick
	// span) that the real shell classifier (permissions.ClassifyBash — mvdan.cc/sh AST) rates
	// Dangerous or worse — never because prose merely NAMES one. ClassifyBash is the single
	// source of truth for "what's dangerous", so there's no parallel substring list to drift
	// from it. Prose that happens to land in a span classifies Medium (unknown ≠ read-only), so
	// it doesn't trip; only genuine destructive commands (rm -rf, git push --force, …) do.
	for _, cmd := range shellCommandsIn(text) {
		if r, _ := permissions.ClassifyBash(cmd); r >= permissions.Dangerous {
			return true
		}
	}
	// Security/billing/secret SURFACES escalate WITH edit intent — word-based, so "change the
	// auth flow" counts but a bare lookup ("what does the auth token do") doesn't.
	low := strings.ToLower(text)
	editish, risky := false, false
	for _, w := range wordRe.FindAllString(low, -1) {
		if editishWords[w] {
			editish = true
		}
		if riskSurfaces[w] {
			risky = true
		}
	}
	return editish && risky
}

// --- the MODEL axis (deterministic, conservative) ---

// wordSet builds a lookup set from a word list (no repeated `true`).
func wordSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// editishWords mark intent to CHANGE something or diagnose — matched on WHOLE words
// (so "commit" doesn't catch "commits"). Any of these keeps the capable model:
// misrouting an edit/diagnosis DOWN to the cheap model is the expensive failure.
var editishWords = wordSet(
	"fix", "fixing", "add", "change", "implement", "refactor", "edit", "create", "write",
	"update", "remove", "delete", "rename", "install", "optimize", "improve", "debug",
	"broken", "why", "build", "make", "wire", "rework", "migrate", "rotate",
	// live-data intent: these need web_search tool calls (agentic work, and the small
	// self-hosted model is the one that shrugs the web doctrine off) — never downgrade.
	"latest", "today", "news", "currently", "now", "live",
)

var wordRe = regexp.MustCompile(`[a-z]+`)

// NOTE: the per-turn MODEL tier decision used to live here as modelForTurn. It
// lives in the CLI's selection policy now (llm/lane.go + llm/resolve.go): this
// layer emits an abstract Intent (purpose + effort + risk) and the ladder
// resolves the tier. Here stays only what the session uniquely observes — the
// THINKING axis (effortForTurn) and the room/risk signals (frictionEscalates,
// highRiskTurn) it folds into the hint.
