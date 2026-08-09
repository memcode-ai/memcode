package runtime

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/store"
)

func codeNavSess(t *testing.T) *Session {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newSess(st, captureProviderNil{}, t.TempDir(), "auto", permissions.ModeAuto, io.Discard)
}

// code_nav is advertised, validates its args, and degrades with an actionable message
// when the language's server isn't available (no server installed in CI).
func TestCodeNavToolBasics(t *testing.T) {
	s := codeNavSess(t)
	ctx := context.Background()
	if !hasTool(s.toolDefs(), tools.CodeNav) {
		t.Fatal("code_nav should be advertised")
	}
	// Missing coordinates → clear validation error.
	bad, _ := json.Marshal(tools.CodeNavInput{Action: "definition", Path: "x.go"})
	if r := s.codeNavTool(ctx, bad); !r.isError || !strings.Contains(r.text(), "line") {
		t.Fatalf("missing line/col should error: %q", r.text())
	}
	// Unsupported language → not-configured message (never a crash).
	css, _ := json.Marshal(tools.CodeNavInput{Action: "hover", Path: "styles.css", Line: 1, Col: 1})
	if r := s.codeNavTool(ctx, css); !r.isError || !strings.Contains(r.text(), "no language server") {
		t.Fatalf("unsupported language should report no server: %q", r.text())
	}
}
