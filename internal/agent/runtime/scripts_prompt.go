package runtime

import (
	"strings"

	"github.com/memcode-ai/memcode/internal/scripts"
)

// This file owns how saved SCRIPTS (reusable multi-step command sequences) are surfaced to
// the model — a passive, always-on pointer plus an active, request-triggered nudge. Mirrors
// skills.go's shape exactly; storage/lookup itself lives in the scripts package.

// scriptsPointer is the one-liner that tells the model saved scripts EXIST and how to use
// them — a pointer, not a payload: only when at least one is saved (nothing to point at
// otherwise). Leads with the `script` tool (find/list → run) and names save as the natural
// follow-up to a sequence that just worked.
func scriptsPointer(saved []scripts.Script) string {
	if len(saved) == 0 {
		return ""
	}
	return "SAVED SCRIPTS — reusable multi-step command sequences from past work (e.g. \"rebuild the cli\", " +
		"\"commit, push, deploy\"). Use the `script` tool: `find`/`list` to see what's saved, `run` to replay one by " +
		"slug — it asks ONCE (\"run script X?\"), then executes; it does NOT re-classify the commands inside (they " +
		"were already vetted, live, the first time the sequence ran). Before RE-DERIVING a sequence you've likely " +
		"run before, check `script{find:\"...\"}` first. After a multi-step sequence you just ran WORKS, consider " +
		"`script{save:...}` it for next time — your judgment, not a required ceremony."
}

// scriptNudge turns the passive pointer into an active reminder when the request itself
// names a saved script's slug — the high-precision case where re-deriving from scratch would
// be wasted effort. Fires at most once per slug per session (s.nudgedScripts). Empty when
// nothing matches.
func (s *Session) scriptNudge(text string) string {
	if len(s.scripts) == 0 {
		return ""
	}
	words := skillWordSet(text) // same word-boundary tokenizer; not skill-specific despite the name
	var triggers []string
	for _, sc := range s.scripts {
		trig := strings.ToLower(sc.Slug)
		if !scriptTriggerMatches(trig, words) || s.nudgedScripts[trig] {
			continue
		}
		s.nudgedScripts[trig] = true
		triggers = append(triggers, sc.Slug)
		if len(triggers) == 2 { // name the two most salient; more would be noise
			break
		}
	}
	if len(triggers) == 0 {
		return ""
	}
	return "SAVED SCRIPT MATCH — a saved script looks like it covers this: " + strings.Join(triggers, ", ") +
		". Consider script{run:\"" + triggers[0] + "\"} instead of re-deriving the steps from scratch " +
		"(script{find:\"...\"} first if you want to confirm it's still the right one)."
}

// scriptTriggerMatches reports whether every hyphen-joined word of a script's slug appears
// in the request's word set — e.g. slug "rebuild-cli" matches "please rebuild the cli please",
// but a partial "just rebuild it" (missing "cli") does not, keeping the nudge high-precision.
func scriptTriggerMatches(slug string, words map[string]bool) bool {
	parts := strings.Split(slug, "-")
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if len(p) < 3 || !words[p] {
			return false
		}
	}
	return true
}
