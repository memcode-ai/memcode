package runtime

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

// The `$` lane never prints permission provenance: the USER typed the command,
// so a "⎿ pre-approved" sub-line is noise — and a pending note must be discarded,
// not left to leak onto the next agent surface.
func TestShellLaneDiscardsProvenance(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var out bytes.Buffer
	s := newSess(st, &captureProvider{}, t.TempDir(), "m", permissions.ModeAuto, &out)

	s.allowPending = "pre-approved" // as gateCommand would set on a remembered-rule hit
	s.runShell(context.Background(), "echo hi")

	if strings.Contains(out.String(), "pre-approved") {
		t.Fatalf("$ lane printed a provenance note:\n%s", out.String())
	}
	if s.allowPending != "" {
		t.Fatal("pending note must be discarded, not left to leak onto the next agent surface")
	}
}
