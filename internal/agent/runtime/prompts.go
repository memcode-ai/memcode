package runtime

// Prompt SPECS, not prompt text. The doctrine — laws, mode prompts, room
// guidance, plan nudges — lives in internal/doctrine (client-owned). This
// package only gathers FACTS (repo root, platform, shell, overview/
// ContextPack) and names a MODE; request() composes locally via render()
// (internal/doctrine) into the (stable, volatile) two-system-message form
// the one-wire transport emits.

import (
	"math/rand"
	stdruntime "runtime"

	"github.com/memcode-ai/memcode/internal/doctrine"
	"github.com/memcode-ai/memcode/internal/wire"
)

// promptSpec selects a doctrine-composed system prompt: the mode, the locally
// gathered facts the mode's template needs, and a turn-scoped extra (skills
// catalog, prior-session orientation) appended after the doctrine. The
// transport's compose hook renders it (cli/internal/doctrine) just before
// encoding.
type promptSpec struct {
	mode  string
	facts map[string]string
	extra string
}

// withFact returns a copy of the spec with one fact added/replaced (the spec is
// shared across turns; never mutate in place).
func (p promptSpec) withFact(k, v string) promptSpec {
	facts := make(map[string]string, len(p.facts)+1)
	for fk, fv := range p.facts {
		facts[fk] = fv
	}
	facts[k] = v
	p.facts = facts
	return p
}

// withExtra returns a copy with turn-scoped text appended after the doctrine.
func (p promptSpec) withExtra(t string) promptSpec {
	if t == "" {
		return p
	}
	if p.extra != "" {
		p.extra += "\n\n"
	}
	p.extra += t
	return p
}

// withMode returns a copy of the spec re-tagged to a different doctrine mode,
// preserving its facts/extra. Used when a turn changes mode mid-flight (a mid-turn
// EnterPlan): the captured spec predates the switch, so its Mode would route + compose
// as the OLD mode (e.g. a high-effort plan turn sent as "exec" escalates to Opus).
func (p promptSpec) withMode(m string) promptSpec {
	p.mode = m
	return p
}

// request stamps the spec onto a provider request: composed LOCALLY via
// render() into the two-system form — stable doctrine prefix → System,
// per-turn suffix (incl. the extra) → SystemVolatile. Facts stay OFF the wire
// entirely; Mode remains stamped as a selection input (the Runner's ladder
// reads it — it never rides the wire itself).
func (p promptSpec) request(r wire.Request) wire.Request {
	if stable, volatile, err := p.render(); err == nil {
		r.Mode = p.mode
		r.Facts = nil
		r.System = stable
		r.SystemVolatile = volatile
		return r
	}
	// Unknown mode / missing fact: fall through to the legacy stamp — the
	// compat transport composes at send time and surfaces the error there,
	// on the turn that owns it.
	r.Mode = p.mode
	r.Facts = p.facts
	r.System = p.extra
	return r
}

// render composes the spec locally into the one-wire two-system-message form:
// the first system message is the STABLE, cacheable doctrine prefix (byte-
// identical across turns with identical facts — the cache key); the second is
// the VOLATILE per-turn suffix (date, room, personality, extra-mile, mcp,
// nudge, then this spec's turn-scoped extra). request() always rides this
// path (the one-wire transport carries the two halves as system messages).
//
// No model/pin inputs by design: composition happens BEFORE model selection
// (the one lane-dependent fragment, delegateDoctrine, is appended by the
// Runner's selection policy post-resolution — cli/internal/llm).
func (p promptSpec) render() (stable, volatile string, err error) {
	return doctrine.Compose(p.mode, p.facts, p.extra, "", false)
}

// baseFacts are the client-environment facts every mode needs.
func (s *Session) baseFacts() map[string]string {
	f := map[string]string{
		"root":     s.root,
		"platform": stdruntime.GOOS,
		"shell":    shellName(),
	}
	if p := s.personalityFact(); p != "" {
		// The doctrine composer gates this to conversational modes and wraps it in a
		// tone-only guard; sent here so it rides every turn without per-spec plumbing.
		f["personality"] = p
	}
	if s.extraMile {
		// "Extra mile" mode: the doctrine composer gates this to planner/executor modes and
		// injects an above-and-beyond rule. On every freshly-built spec here; the cached chat
		// spec also refreshes it per-turn (runTurn) so a mid-session /extramile toggle takes effect.
		f["extramile"] = "on"
	}
	if !s.readOnly {
		// The MCP index rides the volatile facts (progressive disclosure: server names +
		// tool counts only, ~25 tokens/server — the mcp tool discloses the rest on demand).
		// Never for read-only explorers, who don't get the mcp tool either.
		if line := s.mcpIndexFact(); line != "" {
			f["mcp"] = line
		}
	}
	return f
}

// personalityFact resolves the personality to send THIS request: the chosen voice, with
// "random" resolved to the session's roll (made in SetPersonality) so the composer only
// ever sees a real key and the ready line can name it. "" when none. Still read fresh per
// turn so a mid-session /personality change takes effect.
func (s *Session) personalityFact() string {
	if s.personality == personalityRandom {
		if s.personalityRoll == "" {
			s.personalityRoll = randomPersonality() // safety: random set without SetPersonality
		}
		return s.personalityRoll
	}
	return s.personality
}

// personalityRandom is the sentinel voice that rotates: each request picks a different
// concrete voice from personalityPool, resolved here so the composer stays oblivious.
const personalityRandom = "random"

// personalityPool is the set "random" draws from. It mirrors the doctrine's built-in
// voices (internal/doctrine/prompts.go: personalityProse) and the TUI picker
// (personalityCatalog) — keep the three in sync. Excludes "" (default) and "random"
// (no recursion).
var personalityPool = []string{"professional", "joker", "funny", "insulting", "emoji", "mirror", "zen", "dry"}

func randomPersonality() string {
	return personalityPool[rand.Intn(len(personalityPool))]
}

// chatSpec is the INTERACTIVE session prompt (TUI).
func (s *Session) chatSpec(overview string) promptSpec {
	if s.adminMode {
		return promptSpec{mode: "admin", facts: s.baseFacts()}
	}
	p := promptSpec{mode: "chat", facts: s.baseFacts()}
	return p.withFact("overview", overview)
}

// execSpec is the HEADLESS executive prompt (`memcode <task>`), oriented by a
// redacted ContextPack.
func (s *Session) execSpec(packJSON string) promptSpec {
	p := promptSpec{mode: "exec", facts: s.baseFacts()}
	return p.withFact("pack", packJSON)
}

// planSpec is PLAN MODE: research-first, propose, don't implement.
func (s *Session) planSpec(overview string) promptSpec {
	p := promptSpec{mode: "plan", facts: s.baseFacts()}
	return p.withFact("overview", overview)
}

// applySpec is APPLY MODE: execute an already-approved plan, with the plan pinned as the
// contract (a fact) so the server doctrine can forbid re-planning / broad re-research.
func (s *Session) applySpec(plan string) promptSpec {
	p := promptSpec{mode: "apply", facts: s.baseFacts()}
	return p.withFact("plan", plan)
}

// coldSpec is the A/B baseline (no ContextPack, no doctrine).
func (s *Session) coldSpec() promptSpec {
	return promptSpec{mode: "cold", facts: s.baseFacts()}
}

// roomSpec annotates a spec with the assessed interaction-room mode; the
// guidance prose for that mode is the server's.
func (s *Session) roomSpec(p promptSpec) promptSpec {
	if s.room.Mode == "" {
		return p
	}
	return p.withFact("room", string(s.room.Mode))
}
