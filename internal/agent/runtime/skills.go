package runtime

import "strings"

// This file owns how installed skills are SURFACED to the model — both the passive,
// always-on pointer and the active, request-triggered nudge. Discovery/loading itself
// lives in the skills package; here we only decide what to say and when.

// skillsPointer is the one-liner that tells the model installed skills EXIST and how to use
// them — a pointer, not a payload: nothing rides the prompt until the model pulls a skill in.
// It leads with the `skill` tool (find → load) and names the dirs as a fallback the model
// can grep/read directly. Empty when no skill dirs exist.
func skillsPointer(roots []string) string {
	if len(roots) == 0 {
		return ""
	}
	return "INSTALLED SKILLS — expert guidance for specific tools/libraries/services. Use the `skill` tool: `find` " +
		"to search by topic, `load` to pull one in (gated — the user approves, then it's remembered). The MOMENT you " +
		"start working with a third-party CLI, package, or service you lack strong guidance for (a new import, a " +
		"`vercel`/`prisma`/`supabase` command, an Anthropic-API task), `find` it before improvising. The skills also " +
		"live as SKILL.md files under: " + strings.Join(roots, ", ") + " if you want to read one directly. Skip for " +
		"routine repo work; one at a time; reconcile against the repo's ACTUAL dependency versions."
}

// skillNudge turns the passive skills pointer into an active reminder when the request
// itself NAMES an installed skill — the case the model keeps fumbling (it improvised raw
// REST/curl against Supabase instead of loading the installed `supabase` skill, which
// prescribes the CLI/SQL path). A name match is a high-precision signal, so unlike the
// always-on pointer this costs nothing until a trigger actually appears. Fires at most once
// per trigger per session (s.nudgedSkills) so it nudges, not nags. Empty when nothing matches.
func (s *Session) skillNudge(text string) string {
	if len(s.skills) == 0 {
		return ""
	}
	words := skillWordSet(text)
	var triggers []string
	for _, sk := range s.skills {
		trig := skillTrigger(sk.Name) // "supabase"; "vercel:ai-sdk" → "vercel"
		if len(trig) < 4 || !words[trig] || s.nudgedSkills[trig] {
			continue
		}
		s.nudgedSkills[trig] = true
		triggers = append(triggers, trig)
		if len(triggers) == 2 { // name the two most salient; more would be noise
			break
		}
	}
	if len(triggers) == 0 {
		return ""
	}
	return "INSTALLED SKILL MATCH — your request involves " + strings.Join(triggers, ", ") +
		", which an installed skill covers. Run the `skill` tool (find → load) on it BEFORE improvising: " +
		"it has the canonical CLI/SQL/API path, so you don't hand-roll REST calls or guess flags."
}

// skillTrigger reduces a skill name to the single word a user would type to invoke it: the
// plugin namespace for namespaced skills (vercel:ai-sdk → vercel), the bare name otherwise.
func skillTrigger(name string) string {
	n := strings.ToLower(name)
	if i := strings.IndexByte(n, ':'); i >= 0 {
		n = n[:i]
	}
	return n
}

// skillWordSet is the lowercased set of alphanumeric words in text — for exact word-boundary
// matching against skill triggers (so "supabase" matches but a substring like "sub" never does).
func skillWordSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		out[w] = true
	}
	return out
}
