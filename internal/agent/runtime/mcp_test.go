package runtime

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

// The MCP gate has NO risk classifier by design: an opaque remote tool's semantics can't be
// inferred from its name or args, so nothing pretends to (the old verb-list/SQL classifier
// was deleted). The user decides at the invocation prompt; grants persist per project.
// argsCarrySecret is the one deterministic rail that inspects args — known secret values
// force a fresh prompt every time.
func TestArgsCarrySecret(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, &captureProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.redactor.Add("sk-live-hunter2")

	if s.argsCarrySecret(map[string]any{"q": "harmless"}) {
		t.Errorf("plain args must not trip the secret rail")
	}
	if !s.argsCarrySecret(map[string]any{"token": "sk-live-hunter2"}) {
		t.Errorf("a registered secret value in args must trip the rail")
	}
	if !s.argsCarrySecret(map[string]any{"nested": map[string]any{"auth": "Bearer sk-live-hunter2"}}) {
		t.Errorf("secrets embedded in nested args must trip the rail")
	}
	if s.argsCarrySecret(nil) {
		t.Errorf("no args, no secret")
	}
}
