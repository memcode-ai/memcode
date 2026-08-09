// Package sources discovers the instruction/memory/doc artifacts left by AI
// coding tools and humans (CLAUDE.md, .claude/, .cursor/rules, AGENTS.md,
// copilot/windsurf/aider configs, README, docs, ADRs) and records them as
// doctrine *candidates*.
//
// Principle: these are EVIDENCE, not truth. Each source is scoped to the
// directory it governs and carries recency, so memcode can tell when a source is
// likely stale relative to the code it describes. Turning candidates into
// current doctrine — and reconciling conflicts — is the reducer's job (`learn`).
package sources

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/repofiles"
	"github.com/memcode-ai/memcode/internal/store"
)

// staleThresholdDays: a source is only "stale" if the code it governs changed
// this many days AFTER it — avoids flagging docs that were updated alongside
// recent work.
const staleThresholdDays = 21

// Source is one discovered evidence document.
type Source struct {
	Kind        string    `json:"kind"`  // claude | cursor | codex/agents | copilot | windsurf | aider | readme | docs
	Path        string    `json:"path"`  // relative to repo root
	Scope       string    `json:"scope"` // directory it governs ("." = whole repo)
	Bytes       int64     `json:"bytes"`
	ModTime     time.Time `json:"mod_time"`
	GitDate     string    `json:"git_date"` // last commit date touching it (YYYY-MM-DD)
	Stale       bool      `json:"stale"`    // code in scope changed after this last did
	StaleReason string    `json:"stale_reason,omitempty"`
}

// Discover returns every recognized source document among the project's
// non-ignored files (so vendored/cached/gitignored files never count). Tracked
// AI-config files (CLAUDE.md, .claude/*, .cursor/rules) are included; purely
// local-ignored ones are not.
func Discover(ctx context.Context, root string) ([]Source, error) {
	var out []Source
	for _, rel := range repofiles.List(ctx, root) {
		kind, ok := classify(rel)
		if !ok {
			continue
		}
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		s := Source{
			Kind:    kind,
			Path:    rel,
			Scope:   scopeOf(rel),
			Bytes:   info.Size(),
			ModTime: info.ModTime(),
			GitDate: gitLastDate(ctx, root, rel),
		}
		// Stale only if the code it governs changed materially AFTER it.
		if scopeDate := gitLastDate(ctx, root, s.Scope); scopeDate != "" && s.GitDate != "" {
			if days := dayGap(s.GitDate, scopeDate); days > staleThresholdDays {
				s.Stale = true
				s.StaleReason = "code in " + scopeLabel(s.Scope) + " changed " + strconv.Itoa(days) + " days after this was last updated (" + s.GitDate + ")"
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Persist stores discovered sources as doctrine candidates (doctrine entities)
// so context/why and the agent can surface the ones applicable to an area.
func Persist(ctx context.Context, st store.Store, srcs []Source) error {
	for _, s := range srcs {
		attrs, _ := json.Marshal(s)
		if err := st.UpsertEntity(ctx, store.Entity{Kind: "doctrine", Key: s.Path, Attrs: attrs}); err != nil {
			return err
		}
	}
	return nil
}

// Load reads previously discovered sources back from the store.
func Load(ctx context.Context, st store.Store) ([]Source, error) {
	ents, err := st.ListEntities(ctx, "doctrine")
	if err != nil {
		return nil, err
	}
	var out []Source
	for _, e := range ents {
		var s Source
		if len(e.Attrs) > 0 && json.Unmarshal(e.Attrs, &s) == nil && s.Path != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// AppliesTo reports whether a source governs the given path.
func (s Source) AppliesTo(rel string) bool {
	if s.Scope == "." {
		return true
	}
	rel = filepath.ToSlash(rel)
	return rel == s.Scope || strings.HasPrefix(rel, s.Scope+"/")
}

// classify maps a relative path to its source kind.
func classify(rel string) (string, bool) {
	base := filepath.Base(rel)
	switch {
	case base == "CLAUDE.md":
		return "claude", true
	case base == "AGENTS.md":
		return "codex/agents", true
	case base == ".cursorrules":
		return "cursor", true
	case base == ".windsurfrules":
		return "windsurf", true
	case base == "CONVENTIONS.md":
		return "aider", true
	case base == "copilot-instructions.md" && strings.Contains(rel, ".github/"):
		return "copilot", true
	case base == "README.md" || base == "ARCHITECTURE.md" || base == "CONTRIBUTING.md":
		return "readme", true
	case strings.Contains(rel, ".cursor/rules/"):
		return "cursor", true
	case strings.Contains(rel, ".claude/"):
		return "claude", true
	case strings.HasPrefix(rel, "docs/") && strings.HasSuffix(rel, ".md"):
		if strings.Contains(rel, "/adr") || strings.Contains(rel, "/decisions") {
			return "adr", true
		}
		return "docs", true
	}
	return "", false
}

// scopeOf returns the directory an artifact governs.
func scopeOf(rel string) string {
	rel = filepath.ToSlash(rel)
	for _, marker := range []string{"/.claude/", "/.cursor/", "/.github/"} {
		if i := strings.Index(rel, marker); i >= 0 {
			return rel[:i]
		}
	}
	for _, marker := range []string{".claude/", ".cursor/", ".github/", "docs/"} {
		if strings.HasPrefix(rel, marker) {
			return "."
		}
	}
	d := filepath.ToSlash(filepath.Dir(rel))
	if d == "." || d == "" {
		return "."
	}
	return d
}

// dayGap returns how many days b is after a (both YYYY-MM-DD); 0 if unparseable
// or b is not after a.
func dayGap(a, b string) int {
	ta, e1 := time.Parse("2006-01-02", a)
	tb, e2 := time.Parse("2006-01-02", b)
	if e1 != nil || e2 != nil || !tb.After(ta) {
		return 0
	}
	return int(tb.Sub(ta).Hours() / 24)
}

func scopeLabel(scope string) string {
	if scope == "." {
		return "the repo"
	}
	return scope
}

func gitLastDate(ctx context.Context, root, path string) string {
	if path == "" {
		path = "."
	}
	out, err := exec.CommandContext(ctx, "git", "-C", root, "log", "-1", "--format=%ad", "--date=short", "--", path).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
