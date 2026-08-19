// Package sync writes project memory to AI-editor context files
// (CLAUDE.md, .github/copilot-instructions.md, .cursor/rules, .windsurfrules).
// It is driven by the /sync slash command and optionally by a post-commit hook.
package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/config"
)

const managedHeader = "<!-- synced by memcode · edit preferences with /sync -->\n\n"

// DetectTargets returns the subset of SyncTargetAll that already exist on disk,
// and whether each one is already managed by memcode.
type DetectedTarget struct {
	config.SyncTargetMeta
	Exists  bool // file is present in the repo
	Managed bool // file starts with the memcode managed header
}

// Detect scans root for known AI-editor context files.
func Detect(root string) []DetectedTarget {
	out := make([]DetectedTarget, len(config.SyncTargetAll))
	for i, t := range config.SyncTargetAll {
		full := filepath.Join(root, t.Path)
		dt := DetectedTarget{SyncTargetMeta: t}
		if b, err := os.ReadFile(full); err == nil {
			dt.Exists = true
			dt.Managed = strings.HasPrefix(string(b), managedHeader)
		}
		out[i] = dt
	}
	return out
}

// Write writes content to the given target paths under root.
// Each file gets the managed header prepended. Parent directories are created
// as needed.
func Write(root string, content string, targets []config.SyncTargetMeta) error {
	body := managedHeader + content
	var errs []string
	for _, t := range targets {
		full := filepath.Join(root, t.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t.Path, err))
			continue
		}
		if err := atomicfile.WriteFile(full, []byte(body), 0o644); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t.Path, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("sync: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ActiveTargets returns the SyncTargetMeta entries that should be written based
// on the stored SyncConfig and what's currently detected on disk.
//
// "Everything" means every AI-editor file that ALREADY EXISTS in the repo, plus
// any that appear later (each run re-detects, so a newly-created .cursor/rules is
// adopted automatically) — NOT every possible target. Writing all four blindly
// would litter repos with context files for editors the user doesn't use, on
// every commit. An explicit target list, by contrast, is honored verbatim: if you
// pick Cursor before the file exists, we create it.
func ActiveTargets(root string, cfg config.SyncConfig) []config.SyncTargetMeta {
	if cfg.Everything {
		var out []config.SyncTargetMeta
		for _, d := range Detect(root) {
			if d.Exists {
				out = append(out, d.SyncTargetMeta)
			}
		}
		return out
	}
	// Explicit list.
	byName := map[string]config.SyncTargetMeta{}
	for _, t := range config.SyncTargetAll {
		byName[strings.ToLower(t.Name)] = t
	}
	var out []config.SyncTargetMeta
	for _, name := range cfg.Targets {
		if t, ok := byName[string(name)]; ok {
			out = append(out, t)
		}
	}
	return out
}

// InstallGitHook writes a post-commit hook under root/.git/hooks/post-commit
// that calls `memcode sync --auto`. It is idempotent: a no-op if our hook is
// already present. If a hook already exists that isn't ours, it appends to it.
func InstallGitHook(root string) error {
	hookDir := filepath.Join(root, ".git", "hooks")
	if _, err := os.Stat(hookDir); err != nil {
		return nil // not a git repo (or no .git) — skip silently
	}
	hookPath := filepath.Join(hookDir, "post-commit")
	const hookLine = "memcode sync --auto\n"
	const hookMarker = "memcode sync --auto"

	existing, err := os.ReadFile(hookPath)
	if err == nil {
		if strings.Contains(string(existing), hookMarker) {
			return nil // already present
		}
		// Append to existing hook.
		f, err := os.OpenFile(hookPath, os.O_APPEND|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintf(f, "\n# memcode: refresh AI-editor context\n%s", hookLine)
		return err
	}
	// Create a new hook.
	content := "#!/bin/sh\n# memcode: refresh AI-editor context\n" + hookLine
	return os.WriteFile(hookPath, []byte(content), 0o755)
}
