package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/skills"
)

// maxSkillMatches caps how many skills a `find` returns — a query touches a couple of
// candidates at most; a short list keeps the result lean.
const maxSkillMatches = 6

// useSkill drives the skill tool. FIND searches installed skills for a topic (read-only —
// no approval) so the agent can discover the right skill the moment it touches a 3rd-party
// tool. LOAD pulls a skill's body into context by name; that IS gated — the user approves,
// and a "don't ask again" choice is REMEMBERED across sessions (persisted under .memcode).
func (s *Session) useSkill(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.SkillInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}

	// DISCOVER: search the remote skills.sh catalog. Read-only — never gated (no install yet).
	if q := strings.TrimSpace(in.Discover); q != "" {
		dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		hits, err := skills.RemoteFind(dctx, q, "")
		if err != nil {
			return errResult(err.Error())
		}
		if len(hits) == 0 {
			return textResult("no skills.sh catalog match for \"" + q + "\" — proceed without one.")
		}
		var b strings.Builder
		b.WriteString("skills.sh catalog matches for \"" + q + "\" — install one with skill{install:\"<owner/repo@skill>\"}:\n")
		for i, h := range hits {
			if i == maxSkillMatches {
				break
			}
			line := "- " + h.Package
			if h.Installs != "" {
				line += " (" + h.Installs + " installs)"
			}
			b.WriteString(line + "\n")
		}
		return textResult(strings.TrimRight(b.String(), "\n"))
	}

	// INSTALL: add a catalog package into the repo's .agents/skills. GATED — it runs the
	// `skills` CLI (external code) and writes files, so it always asks (no don't-ask-again).
	if pkg := strings.TrimSpace(in.Install); pkg != "" {
		d := s.askApproval(ctx, ApprovalRequest{
			Title:  pkg,
			Label:  "Install skill from skills.sh",
			Detail: "Runs `skills add " + pkg + "` — downloads + writes files under .agents/skills, which then run with agent permissions.",
		})
		if d.Interrupt {
			return errResult("skill install interrupted — stopped at your request.")
		}
		if !d.Allow {
			return errResult("skill " + pkg + " not installed (you declined) — proceed without it.")
		}
		ictx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		out, err := skills.RemoteAdd(ictx, s.root, pkg)
		if err != nil {
			return errResult(err.Error())
		}
		s.skills = skills.Discover(s.root) // re-index so the freshly installed skill is loadable now
		s.toolLine(true, "Skill", "install "+pkg, "", false)
		return textResult("Installed " + pkg + " into .agents/skills — now discoverable; `load` it by name to use it.\n\n" + clip(out, 400))
	}

	// FIND: list installed skills matching a topic. Read-only discovery — never gated.
	if q := strings.TrimSpace(in.Find); q != "" {
		matches := skills.Search(s.skills, q, maxSkillMatches)
		if len(matches) == 0 {
			return textResult("no installed skill matches \"" + q + "\" — proceed without one.")
		}
		var b strings.Builder
		b.WriteString("Installed skills matching \"" + q + "\" — load the best fit with skill{load:\"<name>\"}:\n")
		for _, m := range matches {
			b.WriteString("- " + m.Name + ": " + clip(m.Description, 200) + "\n")
		}
		return textResult(strings.TrimRight(b.String(), "\n"))
	}

	// LOAD: pull a skill's body in by exact name.
	name := strings.TrimSpace(in.Load)
	if name == "" {
		return errResult("skill needs find:\"<topic>\" to search installed skills, or load:\"<exact name>\" to load one.")
	}
	sk, ok := s.findSkill(name)
	if !ok {
		return errResult("no installed skill named " + name + " — search with skill{find:\"<topic>\"} first, or proceed without one.")
	}

	// Gate the load behind an approval, unless the user already said don't-ask-again for
	// this skill (in THIS session or a PRIOR one — the choice persists). Editable=true
	// surfaces the "remember" option.
	if !s.approvedSkills[sk.Name] {
		d := s.askApproval(ctx, ApprovalRequest{
			Title:    sk.Name,
			Label:    "Use skill",
			Detail:   clip(sk.Description, 200),
			Editable: true,
		})
		if d.Interrupt {
			return errResult("skill load interrupted — stopped at your request.")
		}
		if !d.Allow {
			return errResult("skill " + sk.Name + " not loaded (you declined) — proceed without it or pick another.")
		}
		if d.Remember {
			s.rememberSkill(sk.Name) // persists across sessions
		}
	}

	body, err := sk.Load()
	if err != nil {
		return errResult("couldn't load skill " + sk.Name + ": " + err.Error())
	}
	s.toolLine(true, "Skill", sk.Name, "", false)
	// The body may reference bundled files/scripts; point the model at the skill dir
	// so it can read_file them if the instructions call for it.
	return textResult("Loaded skill \"" + sk.Name + "\" (bundled files, if any, live under " + sk.Dir + "):\n\n" + body)
}

// skillApprovalsPath is the editable file remembering which skills the user approved with
// "don't ask again" — kept in .memcode (like permissions), one skill name per line.
func skillApprovalsPath(root string) string {
	return filepath.Join(root, ".memcode", "skill-approvals")
}

// loadApprovedSkills reads the remembered skill approvals (absent file → empty, not an error).
func loadApprovedSkills(root string) map[string]bool {
	m := map[string]bool{}
	b, err := os.ReadFile(skillApprovalsPath(root))
	if err != nil {
		return m
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if n := strings.TrimSpace(ln); n != "" {
			m[n] = true
		}
	}
	return m
}

// rememberSkill records a "don't ask again" approval in memory AND on disk, so a future
// session honors it too. Best-effort persistence — an unwritable file just means re-asking.
func (s *Session) rememberSkill(name string) {
	if s.approvedSkills == nil {
		s.approvedSkills = map[string]bool{}
	}
	if s.approvedSkills[name] {
		return // already remembered — don't double-write
	}
	s.approvedSkills[name] = true
	path := skillApprovalsPath(s.root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(name + "\n")
}

// findSkill resolves a catalog name (case-insensitively) to a discovered skill.
func (s *Session) findSkill(name string) (skills.Skill, bool) {
	want := strings.TrimSpace(strings.ToLower(name))
	for _, sk := range s.skills {
		if strings.ToLower(sk.Name) == want {
			return sk, true
		}
	}
	return skills.Skill{}, false
}
