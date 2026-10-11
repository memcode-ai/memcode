// Package skills — this file is the embedded-fallback source: a skill that
// ships INSIDE the memcode binary (go:embed), never written to disk, so
// discovery works even in a session with no $HOME and no repo skill dirs.
// See DiscoverIn: it ranks below every filesystem root, so a repo-local or
// user-global skill of the same name overrides it for free via the existing
// first-occurrence-per-name dedup — no special-casing anywhere else.
package skills

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed builtin
var builtinFS embed.FS

// builtinPriority must stay numerically ABOVE every filesystem root's
// priority (see discoveryRoots — currently maxing at 5, ~/.claude/plugins)
// so a built-in skill always loses to a real filesystem one of the same
// name. If discoveryRoots ever adds a lower-precedence (higher-numbered)
// root, bump this to stay below it.
const builtinPriority = 6

// builtinCandidates parses every builtin/<name>/SKILL.md embedded in the
// binary into a candidate at builtinPriority. Parse errors or malformed
// skills are skipped rather than panicking — a shipped skill failing to
// parse should degrade silently, not break discovery for everything else.
func builtinCandidates() []candidate {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil
	}
	var out []candidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		skillPath := path.Join("builtin", name, "SKILL.md")
		raw, err := builtinFS.ReadFile(skillPath)
		if err != nil {
			continue
		}
		sk, ok := parseFrontmatterBody(string(raw))
		if !ok {
			continue
		}
		sk.body = stripFrontmatter(string(raw))
		sk.Path = "built-in/" + name + "/SKILL.md"
		sk.Dir = "built-in/" + name
		out = append(out, candidate{
			skill:    sk,
			priority: builtinPriority,
			version:  sk.Version,
			mtimeNs:  0,
		})
	}
	return out
}
