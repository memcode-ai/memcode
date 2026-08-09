package runtime

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

type webProv struct{ ans string }

func (p webProv) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "end_turn"}, nil
}
func (p webProv) WebSearch(ctx context.Context, q string) (string, error) { return p.ans, nil }

func TestWebSearchTool(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := newSess(st, webProv{"current answer [example.com]"}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	// query path
	r := s.webSearchTool(ctx, []byte(`{"query":"go 1.24 generics"}`))
	if out, isErr := r.text(), r.isError; isErr || out != "current answer [example.com]" {
		t.Fatalf("query search: out=%q err=%v", out, isErr)
	}
	// missing query
	if r := s.webSearchTool(ctx, []byte(`{}`)); !r.isError {
		t.Fatal("empty web_search should error")
	}
	// provider without the capability → graceful unavailable
	s2 := newSess(st, captureProviderNil{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	r2 := s2.webSearchTool(ctx, []byte(`{"query":"x"}`))
	if out, isErr := r2.text(), r2.isError; !isErr || !strings.Contains(out, "not available") {
		t.Fatalf("expected unavailable, got %q (err=%v)", out, isErr)
	}
}
