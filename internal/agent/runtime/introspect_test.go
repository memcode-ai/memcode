package runtime

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

func newIntrospectSession(t *testing.T) *Session {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.AddClaim(ctx, store.Claim{
		ID: "c1", Type: "command", Text: "Use pnpm as the package manager, never npm",
		Scope: ".", Status: "current", Confidence: "high", ExtractedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return newSess(st, &captureProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
}

func TestMemcodeToolMemories(t *testing.T) {
	s := newIntrospectSession(t)
	r := s.memcodeTool(context.Background(), []byte(`{"command":"memories"}`))
	out, isErr := r.text(), r.isError
	if isErr {
		t.Fatalf("memories errored: %s", out)
	}
	if !strings.Contains(out, "pnpm") {
		t.Fatalf("memories output missing the claim: %q", out)
	}
}

func TestMemcodeToolRecall(t *testing.T) {
	s := newIntrospectSession(t)
	r := s.memcodeTool(context.Background(), []byte(`{"command":"recall","query":"which package manager"}`))
	out, isErr := r.text(), r.isError
	if isErr {
		t.Fatalf("recall errored: %s", out)
	}
	if !strings.Contains(out, "pnpm") {
		t.Fatalf("recall output missing the claim: %q", out)
	}
}

func TestMemcodeToolUnknownCommand(t *testing.T) {
	s := newIntrospectSession(t)
	if r := s.memcodeTool(context.Background(), []byte(`{"command":"frobnicate"}`)); !r.isError {
		t.Fatal("expected an error for an unknown command")
	}
}

func TestMemcodeToolRecallNeedsQuery(t *testing.T) {
	s := newIntrospectSession(t)
	if r := s.memcodeTool(context.Background(), []byte(`{"command":"recall"}`)); !r.isError {
		t.Fatal("recall with no query should error")
	}
}
