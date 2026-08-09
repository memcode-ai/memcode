package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// memcodeMdName is memcode's project/user instruction file — the equivalent of CLAUDE.md.
const memcodeMdName = "MEMCODE.md"

// loadInstructions reads the user's custom instructions — user-wide (global) followed by
// project (more specific), each labeled so the model knows their scope. At EACH scope
// memcode's own MEMCODE.md wins, but if it's absent we FALL THROUGH to CLAUDE.md so a repo
// already set up for Claude Code works with no extra file. These are STANDING instructions
// (honor every turn), loaded inline, not pointed at like skills — a rule the model never
// reads is a rule it silently breaks. Returns "" when nothing exists. Pure for testing.
func loadInstructions(root, home string) string {
	var b strings.Builder
	addFirst := func(candidates ...[2]string) { // each: {path, label}; first existing & non-empty wins
		for _, c := range candidates {
			data, err := os.ReadFile(c[0])
			if err != nil {
				continue
			}
			if txt := strings.TrimSpace(string(data)); txt != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString("## " + c[1] + "\n" + txt)
				return
			}
		}
	}
	if home != "" {
		addFirst(
			[2]string{filepath.Join(home, ".memcode", memcodeMdName), "User instructions (~/.memcode/" + memcodeMdName + ")"},
			[2]string{filepath.Join(home, ".claude", claudeMdName), "User instructions (~/.claude/" + claudeMdName + ")"},
		)
	}
	addFirst(
		[2]string{filepath.Join(root, memcodeMdName), "Project instructions (./" + memcodeMdName + ")"},
		[2]string{filepath.Join(root, claudeMdName), "Project instructions (./" + claudeMdName + ")"},
	)
	if b.Len() == 0 {
		return ""
	}
	return "PROJECT INSTRUCTIONS — user-authored, treat as standing doctrine for this repo and honor them every " +
		"turn (more specific project instructions win over user-wide ones):\n\n" + b.String()
}

// claudeMdName is the Claude Code instruction file memcode falls through to when its own
// MEMCODE.md is absent — so an existing Claude Code repo works without a second file.
const claudeMdName = "CLAUDE.md"

// userInstructions loads the custom instructions and applies the size tiers: load verbatim
// when small, shrinkwrap (compress + cache) when large, refuse with a startup notice when
// it's so big it's a doc-dump rather than instructions. Called once per session.
func (s *Session) userInstructions(ctx context.Context) string {
	home, _ := os.UserHomeDir()
	out := loadInstructions(s.root, home)
	switch instructionTier(len(out)) {
	case tierRefuse:
		s.printf("  ⚠ %s is %d KB — too large to load as standing instructions; skipping it this session. "+
			"Trim it to a focused set of rules.\n", memcodeMdName, len(out)/1024)
		return ""
	case tierShrink:
		return s.shrinkwrap(ctx, out)
	default:
		return out
	}
}
