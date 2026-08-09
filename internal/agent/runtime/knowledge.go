package runtime

import (
	"encoding/json"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/knowledge"
)

// This file owns how memcode's built-in KNOWLEDGE PACKS are surfaced to the model. The split is
// deliberate: DETERMINISM for the FACT (repo detection — "this repo uses vercel" is true in the
// files), MODEL JUDGMENT for the rest (when to consult the ungated `knowledge` tool). We do NOT
// keyword-match the user's prose to force content into context — that's CLI-side intent
// classification, which memcode's doctrine keeps out of the CLI (the model/gateway judges intent,
// not a regex). The pointer states the detected fact + the consult obligation; the model acts on it.

// useKnowledge drives the knowledge tool. FIND lists packs matching a topic; TOPIC returns one
// pack's full Facts + Idioms by name. UNGATED throughout — it's reference, not an action, so
// (unlike skill{load}) there is no approval prompt.
func (s *Session) useKnowledge(input json.RawMessage) toolResult {
	var in tools.KnowledgeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}

	// FIND: list packs matching a topic.
	if q := strings.TrimSpace(in.Find); q != "" {
		matches := knowledge.Find(q)
		if len(matches) == 0 {
			return textResult("no knowledge pack matches \"" + q + "\" — available: " +
				strings.Join(knowledge.Names(), ", ") + ". Proceed without one.")
		}
		var b strings.Builder
		b.WriteString("Knowledge packs matching \"" + q + "\" — read one with knowledge{topic:\"<name>\"}:\n")
		for _, p := range matches {
			b.WriteString("- " + p.Name + "\n")
		}
		return textResult(strings.TrimRight(b.String(), "\n"))
	}

	// TOPIC: return a pack's full body by exact name.
	name := strings.TrimSpace(in.Topic)
	if name == "" {
		return errResult("knowledge needs find:\"<topic>\" to search, or topic:\"<exact pack name>\" to read one. Available: " +
			strings.Join(knowledge.Names(), ", "))
	}
	p, ok := knowledge.Get(name)
	if !ok {
		return errResult("no knowledge pack named " + name + " — available: " + strings.Join(knowledge.Names(), ", "))
	}
	s.toolLine(true, "Knowledge", p.Name, "", false)
	return textResult(p.Full())
}

// knowledgePointer tells the model the knowledge packs EXIST, names the ones this repo actually
// uses (deterministic — Detect reads package.json/manifests/marker files), and states the consult
// obligation. This is the ONLY surfacing: a fact + an instruction, not pushed content. The model
// judges when to act on it. Leading with the detected stacks is what makes a relevant pack salient
// at the moment it matters — fixing the original failure (a pack was available but never reached for)
// without CLI-side prose classification. The catalog is small/curated, so naming packs is cheap.
func knowledgePointer(root string) string {
	all := knowledge.Names()
	if len(all) == 0 {
		return ""
	}
	if detected := knowledge.Detect(root); len(detected) > 0 {
		var names []string
		for _, p := range detected {
			names = append(names, p.Name)
		}
		return "KNOWLEDGE PACKS — this repo uses: " + strings.Join(names, ", ") + ". memcode carries curated " +
			"baseline facts + idioms for these. The MOMENT you write or change code touching their platform " +
			"behavior — env vars, build/deploy config, server/client boundaries, auth/RLS — and you're not " +
			"certain of the specifics, CONSULT the pack FIRST with knowledge{topic:\"<name>\"} (ungated, free). " +
			"It's cheaper than guessing wrong. Other packs available: " + strings.Join(all, ", ") + "."
	}
	return "KNOWLEDGE PACKS — memcode carries baseline facts/idioms for common stacks (" + strings.Join(all, ", ") +
		"). Consult with knowledge{topic:\"<name>\"} (ungated) the moment you touch one and aren't sure of its specifics."
}
