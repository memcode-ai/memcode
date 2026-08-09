package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/scripts"
)

// maxScriptMatches caps how many scripts a `find` (or a `run`-miss's near-match hint)
// surfaces — mirrors maxSkillMatches: a short list stays useful, a long one is noise.
const maxScriptMatches = 6

// useScript drives the script tool. LIST/FIND are read-only (never gated) — they only
// describe what's already saved. SAVE/DELETE/RUN each get exactly ONE coarse permission
// decision (s.gate — the same Yes/remember/cancel card edits use), never a deep look inside
// the script: the commands a script is MADE OF were already approved (per-command, by the
// real bash gate) the moment they first ran live — that's what earned the script its "this
// is repeatable" status. Re-classifying its contents on every replay would be redundant,
// over-cautious, and exactly the bespoke-permissions layer this tool must NOT add. So `run`
// asks "run script X?" once, then executes straight through — no inner classifier.
func (s *Session) useScript(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.ScriptInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}

	if in.List {
		return s.listScripts()
	}
	if q := strings.TrimSpace(in.Find); q != "" {
		return s.findScripts(q)
	}
	if slug := strings.TrimSpace(in.Run); slug != "" {
		return s.runScript(ctx, slug, in.Background)
	}
	if slug := strings.TrimSpace(in.Save); slug != "" {
		return s.saveScript(ctx, slug, in.Description, in.Command)
	}
	if slug := strings.TrimSpace(in.Delete); slug != "" {
		return s.deleteScript(ctx, slug)
	}
	return errResult("script needs one action: list:true, find:\"<topic>\", run:\"<slug>\", save:\"<slug>\" (+description+command), or delete:\"<slug>\".")
}

// listScripts formats every saved script — read-only, ungated.
func (s *Session) listScripts() toolResult {
	all, err := scripts.List(s.root)
	if err != nil {
		return errResult(err.Error())
	}
	if len(all) == 0 {
		return textResult("no saved scripts yet — save one after a proven multi-step run with script{save:\"<slug>\", description:\"...\", command:\"...\"}.")
	}
	var b strings.Builder
	b.WriteString("Saved scripts — run one with script{run:\"<slug>\"}:\n")
	for _, sc := range all {
		b.WriteString("- " + sc.Slug + ": " + clip(sc.Description, 200))
		if sc.RunCount > 0 {
			b.WriteString(" (run " + strconv.Itoa(sc.RunCount) + " time(s))")
		}
		b.WriteString("\n")
	}
	return textResult(strings.TrimRight(b.String(), "\n"))
}

// findScripts searches saved scripts by topic — read-only, ungated.
func (s *Session) findScripts(query string) toolResult {
	all, err := scripts.List(s.root)
	if err != nil {
		return errResult(err.Error())
	}
	matches := searchScripts(all, query, maxScriptMatches)
	if len(matches) == 0 {
		return textResult("no saved script matches \"" + query + "\" — list them all with script{list:true}, or save a new one.")
	}
	var b strings.Builder
	b.WriteString("Saved scripts matching \"" + query + "\" — run the best fit with script{run:\"<slug>\"}:\n")
	for _, sc := range matches {
		b.WriteString("- " + sc.Slug + ": " + clip(sc.Description, 200) + "\n")
	}
	return textResult(strings.TrimRight(b.String(), "\n"))
}

// runScript hands the saved body to execution under a SINGLE, script-level permission
// decision — not bash's per-command risk classifier. The classifier/gate already did its
// job on every command that went into this script WHILE it was being run live and approved
// (that's what made it worth saving); re-litigating that content command-by-command on
// every replay is exactly the redundant, over-cautious layer this tool must NOT add. So a
// saved script gets ONE gate — "run script <slug>?" — mirroring save/delete's card, honoring
// the same session "don't ask again" — and then runs straight through runGatedCommand.
func (s *Session) runScript(ctx context.Context, slug string, background bool) toolResult {
	sc, ok := scripts.Get(s.root, slug)
	if !ok {
		msg := "no saved script named \"" + slug + "\""
		if all, err := scripts.List(s.root); err == nil {
			if near := searchScripts(all, slug, 3); len(near) > 0 {
				names := make([]string, len(near))
				for i, m := range near {
					names[i] = m.Slug
				}
				msg += " — did you mean: " + strings.Join(names, ", ") + "?"
			} else if len(all) == 0 {
				msg += " — nothing has been saved yet (script{save:...} after a proven run)."
			}
		}
		return errResult(msg + " List them with script{list:true}.")
	}
	if ok, reason := s.gate(ctx, permissions.Medium, false, ApprovalRequest{
		Title: sc.Slug, Label: "Run script", Detail: clip(sc.Description, 200), Editable: true,
		Risk: permissions.Medium.String(),
	}); !ok {
		return errResult("script not run: " + reason)
	}
	tr := s.runGatedCommand(ctx, sc.Body, "", background)
	if !tr.isError {
		scripts.RecordRun(s.root, slug)
	}
	return tr
}

// saveScript persists a proven command sequence. Gated like any other file write — the
// standard Yes/remember/cancel card (Editable:true surfaces "don't ask again for edits").
func (s *Session) saveScript(ctx context.Context, slug, description, command string) toolResult {
	if !scripts.ValidSlug(strings.TrimSpace(slug)) {
		return errResult("script slug must be lowercase letters, digits, and hyphens (got " + clip(slug, 40) + ")")
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return errResult("script save needs description:\"...\"")
	}
	if strings.TrimSpace(command) == "" {
		return errResult("script save needs command:\"...\"")
	}
	if ok, reason := s.gate(ctx, permissions.Medium, false, ApprovalRequest{
		Title: slug, Label: "Save script", Detail: clip(description, 200), Editable: true,
		Risk: permissions.Medium.String(),
	}); !ok {
		return errResult("script not saved: " + reason)
	}
	sc, err := scripts.Save(s.root, slug, description, command)
	if err != nil {
		return errResult(err.Error())
	}
	s.refreshScripts()
	s.toolLine(true, "Script", "save "+sc.Slug, "", false)
	return textResult("saved script `" + sc.Slug + "` (.memcode/scripts/" + sc.Slug + ".sh) — replay it with script{run:\"" + sc.Slug + "\"}.")
}

// deleteScript soft-removes a saved script (moved to .trash, not hard-deleted — .memcode is
// gitignored so it isn't otherwise git-recoverable). Gated the same as save.
func (s *Session) deleteScript(ctx context.Context, slug string) toolResult {
	sc, ok := scripts.Get(s.root, slug)
	if !ok {
		return errResult("no saved script named \"" + slug + "\" — list them with script{list:true}.")
	}
	if ok, reason := s.gate(ctx, permissions.Medium, false, ApprovalRequest{
		Title: sc.Slug, Label: "Delete script", Detail: clip(sc.Description, 200), Editable: true,
		Risk: permissions.Medium.String(),
	}); !ok {
		return errResult("script not deleted: " + reason)
	}
	if err := scripts.Delete(s.root, sc.Slug); err != nil {
		return errResult(err.Error())
	}
	s.refreshScripts()
	s.toolLine(true, "Script", "delete "+sc.Slug, "", false)
	return textResult("deleted script `" + sc.Slug + "` (moved to .memcode/scripts/.trash — recoverable, not gone for good).")
}

// refreshScripts re-indexes the session's script catalog after a save/delete so subsequent
// list/find/nudge calls this session see the change immediately.
func (s *Session) refreshScripts() {
	if all, err := scripts.List(s.root); err == nil {
		s.scripts = all
	}
}

// searchScripts ranks saved scripts against a query (slug + description), mirroring
// skills.Search's whole-query/token scoring. Used by both `find` and a `run` miss's
// near-match suggestion.
func searchScripts(all []scripts.Script, query string, max int) []scripts.Script {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" || len(all) == 0 || max <= 0 {
		return nil
	}
	var qtoks []string
	for _, t := range strings.FieldsFunc(q, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') && r != '-'
	}) {
		if len(t) >= 3 {
			qtoks = append(qtoks, t)
		}
	}
	type scored struct {
		sc scripts.Script
		n  int
	}
	var hits []scored
	for _, sc := range all {
		hay := strings.ToLower(sc.Slug + " " + sc.Description)
		n := 0
		if strings.Contains(hay, q) {
			n += 5
		}
		for _, t := range qtoks {
			if strings.Contains(hay, t) {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, scored{sc, n})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].sc.Slug < hits[j].sc.Slug
	})
	if len(hits) > max {
		hits = hits[:max]
	}
	out := make([]scripts.Script, len(hits))
	for i, h := range hits {
		out[i] = h.sc
	}
	return out
}
