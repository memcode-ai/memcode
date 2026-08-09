package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/sources"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
)

// indexHead* name the State marker recording the HEAD the structural model was last
// scanned at — the signal for auto-refresh (memcode maintains its own model; the
// user never runs `memcode index`).
const indexHeadScope, indexHeadLayer = "repo", "index_head"

// storePath returns the SQLite path for a project rooted at root.
func storePath(root string) string {
	return filepath.Join(root, config.DirName, "state.db")
}

// openProject locates the project root, loads its config and opens the state
// store. If the project hasn't been initialized yet, it self-heals by running
// the deterministic first-run setup (no model call) so the user always gets
// something to work with — first run should give you a working tool, not
// homework. Callers must Close the store.
func openProject(ctx context.Context) (store.Store, *config.Config, error) {
	cfg, err := config.Load(".")
	if errors.Is(err, config.ErrNotInitialized) {
		if cfg, err = ensureInitialized(ctx); err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(ctx, storePath(cfg.Root))
	if err != nil {
		return nil, nil, err
	}
	// Self-ignore .memcode on EVERY launch — config.Init only runs on first-time
	// init, but an already-initialized project still needs the .gitignore (and the
	// migration of the old `*`+`!.gitignore` template that didn't hide the dir).
	config.EnsureGitignore(cfg.Root)
	freshenIndex(ctx, st, cfg.Root) // re-scan if HEAD moved since last time — no manual `index`
	return st, cfg, nil
}

// ensureInitialized runs the deterministic init/index path (topology + source
// discovery — never the model-backed `learn`) and returns the loaded config.
// Works inside or outside a git repo: outside one, state anchors to the CWD.
func ensureInitialized(ctx context.Context) (*config.Config, error) {
	root, _, err := config.Resolve(".")
	if err != nil {
		return nil, err
	}

	if _, err := config.Init(root, false); err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, storePath(root))
	if err != nil {
		return nil, err
	}
	defer st.Close()

	// Notices go to stderr so they never pollute machine-readable (--json) stdout.
	fmt.Fprintf(os.Stderr, "● first run here — setting up memcode for %s …\n", root)
	res, err := structure.Scan(ctx, st, root)
	if err != nil {
		return nil, err
	}
	srcs, _ := sources.Discover(ctx, root)
	if err := sources.Persist(ctx, st, srcs); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ couldn't persist sources: %v\n", err)
	}
	_, _ = events.Append(ctx, st, events.KindNote, "init", map[string]any{
		"subsystems": len(res.Subsystems), "sources": len(srcs), "auto": true})
	recordIndexHead(ctx, st, currentHead(ctx, root)) // baseline so we don't immediately re-scan
	fmt.Fprintf(os.Stderr, "  ✓ %d subsystem(s), %d doc source(s) · state at %s\n",
		len(res.Subsystems), len(srcs), storePath(root))
	fmt.Fprintln(os.Stderr, "  (run `memcode learn` later to adjudicate doctrine claims)")

	return config.Load(".")
}

// currentHead returns the repo's HEAD sha, or "" if it can't be determined (no git,
// no commits) — in which case auto-refresh is skipped (nothing reliable to compare).
func currentHead(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// recordIndexHead stamps the HEAD the structural model was last scanned at.
func recordIndexHead(ctx context.Context, st store.Store, head string) {
	if head == "" {
		return
	}
	body, _ := json.Marshal(head)
	if err := st.PutState(ctx, store.State{Scope: indexHeadScope, Layer: indexHeadLayer, Body: body, RefreshedAt: time.Now().UTC()}); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ couldn't record index head: %v\n", err)
	}
}

// freshenIndex re-scans the deterministic model when HEAD has moved since the last
// scan — so the user never runs `memcode index` by hand; keeping the model fresh is
// memcode's housekeeping, not theirs. Cheap when nothing changed (one rev-parse + one
// state lookup, no scan); only an actual HEAD change pays for a rescan, with a notice.
// Best-effort: a scan error never blocks the launch (a slightly stale model is fine).
func freshenIndex(ctx context.Context, st store.Store, root string) {
	head := currentHead(ctx, root)
	if head == "" {
		return // non-git / no commits — manual `memcode index` still works
	}
	if cur, ok, _ := st.GetState(ctx, indexHeadScope, indexHeadLayer); ok {
		var last string
		_ = json.Unmarshal(cur.Body, &last)
		if last == head {
			return // already current
		}
	}
	// Success is silent housekeeping — the user shouldn't watch memcode re-index. A
	// FAILURE is actionable, so surface it (the only case worth a word).
	res, err := structure.Scan(ctx, st, root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "⚠ couldn't refresh memcode's codebase model — run `memcode doctor`")
		return
	}
	srcs, _ := sources.Discover(ctx, root)
	if err := sources.Persist(ctx, st, srcs); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ couldn't persist sources: %v\n", err)
	}
	recordIndexHead(ctx, st, head)
	_, _ = events.Append(ctx, st, events.KindNote, "index", map[string]any{
		"subsystems": len(res.Subsystems), "sources": len(srcs), "head": head, "auto": true})
}
