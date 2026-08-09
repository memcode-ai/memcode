package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/store"
)

// TestSyncDetectReportsDiskTruth: SyncDetect runs memsync.Detect against the session root and
// reports which targets exist on disk. An empty temp dir has none; a CLAUDE.md makes one.
func TestSyncDetectReportsDiskTruth(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	sess := newSess(st, captureProviderNil{}, root, "sonnet", permissions.ModeAsk, io.Discard)

	if d := sess.SyncDetect(); len(d) != len(config.SyncTargetAll) {
		t.Fatalf("SyncDetect should return one entry per known target, got %d", len(d))
	}
	// Nothing exists yet → every target should report Exists=false.
	for _, target := range sess.SyncDetect() {
		if target.Exists {
			t.Errorf("target %q should not exist in an empty temp dir", target.Name)
		}
	}

	// Drop a CLAUDE.md — that target should now report Exists=true.
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# project"), 0o644); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, target := range sess.SyncDetect() {
		if strings.EqualFold(target.Name, "claude") && target.Exists {
			found = true
		}
	}
	if !found {
		t.Fatalf("SyncDetect didn't report CLAUDE.md as existing after we wrote it")
	}
}

// TestSyncNoTargetsReturnsNothingMessage: with no targets selected and no files on disk,
// Sync returns the "nothing to sync yet" message rather than erroring — the picker and the
// CLI both rely on this graceful path.
func TestSyncNoTargetsReturnsNothingMessage(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	sess := newSess(st, captureProviderNil{}, root, "sonnet", permissions.ModeAsk, io.Discard)

	out, err := sess.Sync(ctx, nil)
	if err != nil {
		t.Fatalf("Sync with no targets should not error, got %v", err)
	}
	if !strings.Contains(out, "nothing to sync") {
		t.Fatalf("Sync with no targets should say 'nothing to sync', got %q", out)
	}
}

// TestSyncWritesSelectedTargets: when a target file exists on disk, Sync writes the overview
// to it and reports "synced → <path>". The overview is synthesized from repo facts (no model
// call), so it works with the nil provider.
func TestSyncWritesSelectedTargets(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	// Pre-create the target file so ActiveTargets picks it up (memsync only writes to files
	// that exist, to avoid littering a project with context files the user didn't ask for).
	targetPath := filepath.Join(root, "CLAUDE.md")
	if err := os.WriteFile(targetPath, []byte("# old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess := newSess(st, captureProviderNil{}, root, "sonnet", permissions.ModeAsk, io.Discard)

	// Select the claude target.
	var targets []config.SyncTarget
	for _, t := range config.SyncTargetAll {
		if strings.EqualFold(t.Name, "claude") {
			targets = append(targets, config.SyncTarget(strings.ToLower(t.Name)))
		}
	}
	out, err := sess.Sync(ctx, targets)
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if !strings.Contains(out, "synced →") {
		t.Fatalf("Sync should report 'synced → <path>', got %q", out)
	}
	// The file on disk should have been overwritten with the overview (not the old content).
	written, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "old content") {
		t.Fatalf("Sync didn't overwrite the target file — still has old content")
	}
}
