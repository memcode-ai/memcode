package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/overview"
	memsync "github.com/memcode-ai/memcode/internal/sync"
)

// SyncDetect scans the project root for known AI-editor context files (CLAUDE.md,
// AGENTS.md, .github/copilot-instructions.md, …) and reports which exist on disk and
// which are already managed by memcode. The /sync picker calls this to show the grid
// of toggleable targets before the user picks.
func (s *Session) SyncDetect() []memsync.DetectedTarget {
	return memsync.Detect(s.root)
}

// Sync regenerates the project overview and writes it to the selected AI-editor
// context files. It mirrors the `memcode sync` CLI pipeline (cmd/sync.go) but runs
// against the live session's store/runner/root — so the interactive /sync command
// needs no separate process. targets is the user's pick from the picker (empty +
// Everything=false means "nothing configured"); the overview is synthesized
// deterministically from repo facts, so this works without a model call.
func (s *Session) Sync(ctx context.Context, targets []config.SyncTarget) (string, error) {
	cfg, err := config.Load(s.root)
	if err != nil {
		// No .memcode/config.json yet — but the picker can still have toggled targets
		// on; fall back to a config shaped by those targets so a first-time /sync works.
		cfg = &config.Config{Root: s.root, Sync: config.SyncConfig{Targets: targets}}
	} else {
		cfg.Sync.Targets = targets
		cfg.Sync.Everything = false
	}

	active := memsync.ActiveTargets(s.root, cfg.Sync)
	if len(active) == 0 {
		return "nothing to sync yet — no AI-editor files found", nil
	}

	o, err := overview.Get(ctx, s.store, s.runner, s.root, s.model)
	if err != nil {
		return "", fmt.Errorf("synthesizing overview: %w", err)
	}
	if err := memsync.Write(s.root, o.Text, active); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, t := range active {
		fmt.Fprintf(&b, "synced → %s\n", t.Path)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
