package introspect

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/edit"
	"github.com/memcode-ai/memcode/internal/observe"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"
)

type passthroughRedactor struct{}

func (passthroughRedactor) Redact(s string) string { return s }

// workHistory's OPEN THREADS section must surface a thread an external
// source touched, tagged with its provenance — and must stay silent on that
// front entirely when nothing has ever been ingested (the common case),
// proving observe.HasAny's cheap-skip actually gates the merge.
func TestWorkHistoryMergesExternalObservations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	eng := New(Deps{Root: root, Store: s, SessionID: "sess_live", Redactor: passthroughRedactor{}})

	// Before any ingestion: no observe directory at all, no crash, no mention
	// of external provenance.
	out := eng.workHistory(ctx, 10)
	if strings.Contains(out, "claude-code") {
		t.Fatalf("no external source was ever ingested; output must not mention one:\n%s", out)
	}

	// An external source observed a still-open ask.
	w, err := observe.Open(root, "claude-code", "cc_1")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(observe.Record{Kind: sessionlog.KindUserMessage, Text: "migrate the billing webhook to stripe"}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	out = eng.workHistory(ctx, 10)
	if !strings.Contains(out, "migrate the billing webhook") {
		t.Fatalf("external thread missing from OPEN THREADS:\n%s", out)
	}
	if !strings.Contains(out, "claude-code") {
		t.Fatalf("external provenance must be surfaced:\n%s", out)
	}

	// Confirm nothing leaked into memcode's own session log.
	if _, err := os.Stat(filepath.Join(root, ".memcode", "sessions", "cc_1")); !os.IsNotExist(err) {
		t.Error("the external session must never appear under .memcode/sessions")
	}
}

// workHistory must surface an external FileClaim alongside its OWN,
// independently computed corroboration — two separate facts, never merged
// into one number.
func TestWorkHistoryRendersFileClaimsWithCorroboration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	root := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write("banner.go", "package banner\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	claimedAt := time.Now()

	// The external tool claims it wrote banner.go to this content — and
	// that content is exactly what's on disk (committed intact).
	write("banner.go", "package banner // updated\n")
	claimedHash, _ := edit.Hash(root, "banner.go")
	run("add", ".")
	run("commit", "-q", "-m", "keep banner change")

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	eng := New(Deps{Root: root, Store: s, SessionID: "sess_live", Redactor: passthroughRedactor{}})

	w, err := observe.Open(root, "claude-code", "cc_2")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(observe.Record{
		Kind: sessionlog.KindToolCall, Tool: "Write",
		FileClaim: &observe.FileClaim{Path: "banner.go", ClaimedHash: claimedHash, ClaimedAt: claimedAt},
	}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	out := eng.workHistory(ctx, 10)
	if !strings.Contains(out, "claude-code claims it edited `banner.go`") {
		t.Fatalf("file claim missing:\n%s", out)
	}
	if !strings.Contains(out, "accepted") || !strings.Contains(out, "committed intact") {
		t.Fatalf("corroboration missing/wrong:\n%s", out)
	}
}
