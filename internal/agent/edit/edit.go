// Package edit implements the safe edit transaction used by the agent: read →
// verify the anchor is unique → patch → re-read → show the diff. Anchored
// search/replace, no second model. Bash-driven edits are caught separately by
// the runtime via `git diff`.
package edit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// formatters maps a file extension to its canonical formatter (run IN PLACE, if-installed).
// Polyglot, NOT gofmt-only; the config-driven ones (prettier/ruff) read the project's own
// config when present. Add a row to support a language — no other change needed.
var formatters = map[string][]string{
	".go": {"gofmt", "-w"},
	".rs": {"rustfmt"},
	".py": {"ruff", "format"},
	".js": {"prettier", "--write"}, ".jsx": {"prettier", "--write"},
	".ts": {"prettier", "--write"}, ".tsx": {"prettier", "--write"},
	".json": {"prettier", "--write"}, ".css": {"prettier", "--write"}, ".scss": {"prettier", "--write"},
	".md": {"prettier", "--write"}, ".yaml": {"prettier", "--write"}, ".yml": {"prettier", "--write"},
}

// Format runs the canonical formatter for path's language, by extension, IN PLACE — if
// that tool is on PATH. Best-effort: a missing tool or a formatter error (e.g. the edit
// left a syntax error) is silently ignored; formatting is a courtesy, never a gate.
// Returns the tool name when it ran, "" otherwise (for display).
func Format(ctx context.Context, root, path string) string {
	cmd, ok := formatters[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return ""
	}
	if _, err := exec.LookPath(cmd[0]); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := append(append([]string{}, cmd[1:]...), filepath.Join(root, path))
	if exec.CommandContext(ctx, cmd[0], args...).Run() != nil {
		return ""
	}
	return cmd[0]
}

// Result reports the outcome of an edit.
type Result struct {
	Path        string `json:"path"`
	Applied     bool   `json:"applied"`
	Occurrences int    `json:"occurrences"`
	Created     bool   `json:"created"`
	Diff        string `json:"diff"`
}

// Apply performs an anchored search/replace on path (relative to root).
//
//   - oldString == "" creates a new file with newString (errors if it exists).
//   - otherwise oldString must occur exactly once, unless replaceAll is set.
//
// The file is re-read after writing to confirm the change landed, and a git diff
// is captured for the agent to inspect.
func Apply(ctx context.Context, root, path, oldString, newString string, replaceAll bool) (Result, error) {
	abs := filepath.Join(root, path)

	// Create-new-file path.
	if oldString == "" {
		if _, err := os.Stat(abs); err == nil {
			return Result{}, fmt.Errorf("%s already exists; provide old_string to edit it", path)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(abs, []byte(newString), 0o644); err != nil {
			return Result{}, err
		}
		return Result{Path: path, Applied: true, Created: true, Diff: gitDiff(ctx, root, path)}, nil
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", path, err)
	}
	before := string(data)
	beforeHash := sha256.Sum256(data)

	n := strings.Count(before, oldString)
	switch {
	case n == 0:
		return Result{}, fmt.Errorf("old_string not found in %s", path)
	case n > 1 && !replaceAll:
		return Result{}, fmt.Errorf("old_string appears %d times in %s; add surrounding context or set replace_all", n, path)
	}

	var after string
	if replaceAll {
		after = strings.ReplaceAll(before, oldString, newString)
	} else {
		after = strings.Replace(before, oldString, newString, 1)
	}
	if after == before {
		return Result{}, fmt.Errorf("edit produced no change in %s", path)
	}

	mode := os.FileMode(0o644)
	if info, err := os.Stat(abs); err == nil {
		mode = info.Mode()
	}
	if err := os.WriteFile(abs, []byte(after), mode); err != nil {
		return Result{}, err
	}

	// Re-read to confirm the write landed and the content actually changed.
	reread, err := os.ReadFile(abs)
	if err != nil {
		return Result{}, err
	}
	if sha256.Sum256(reread) == beforeHash {
		return Result{}, fmt.Errorf("re-read of %s shows no change", path)
	}

	// Prefer git's diff (real context for a tracked file); fall back to a diff synthesized from
	// the edited SPAN when git has nothing to show — an UNTRACKED or gitignored file, a path
	// outside any repo, or no git on PATH. Without this fallback the TUI rendered no preview at
	// all for those edits (the "hit or miss" preview), and the model's tool result said only
	// "(no git diff available)" instead of showing what changed.
	diff := gitDiff(ctx, root, path)
	if strings.TrimSpace(diff) == "" {
		diff = spanDiff(before, oldString, newString)
	}
	return Result{
		Path:        path,
		Applied:     true,
		Occurrences: n,
		Diff:        diff,
	}, nil
}

// spanDiff synthesizes a unified-diff-shaped preview of the replaced span (old lines as removals,
// new lines as additions) WITHOUT git — so an edit to an untracked/gitignored/out-of-repo file
// still shows a diff. The hunk is located at the first occurrence's line; for replace_all it shows
// the change once (a preview, not a full patch). Format matches what renderDiff parses.
func spanDiff(before, oldString, newString string) string {
	start := 1
	if i := strings.Index(before, oldString); i > 0 {
		start += strings.Count(before[:i], "\n")
	}
	oldLines, newLines := diffSplit(oldString), diffSplit(newString)
	var b strings.Builder
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", start, len(oldLines), start, len(newLines))
	for _, l := range oldLines {
		b.WriteString("-" + l + "\n")
	}
	for _, l := range newLines {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

// diffSplit splits a span into lines, dropping the single trailing empty element a terminating
// newline would otherwise add (so a 3-line span counts as 3, not 4).
func diffSplit(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Hash returns the current content hash of a file (for staleness checks).
func Hash(root, path string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return "", false
	}
	return HashBytes(data), true
}

// HashBytes hashes already-read content the SAME way as Hash, so a hash recorded at
// read time compares directly against Hash() at write time (the stale-read guard).
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func gitDiff(ctx context.Context, root, path string) string {
	if _, err := exec.LookPath("git"); err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, "git", "-C", root, "diff", "--", path).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
