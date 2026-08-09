package runtime

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

type errProvider struct{}

func (errProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return wire.Response{}, errors.New("provider boom")
}

// A turn whose model call fails must leave LastError set (so the one-shot / resumed one-shot
// paths return a non-zero exit code), and a clean turn must leave it nil.
func TestLastErrorReflectsTurnOutcome(t *testing.T) {
	ctx := context.Background()

	openSess := func(p interface {
		Complete(context.Context, wire.Request) (wire.Response, error)
	}) *Session {
		st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return newSess(st, p, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	}

	// Failed turn → LastError non-nil.
	bad := openSess(errProvider{})
	chat := bad.StartChat(ctx)
	bad.Submit(ctx, chat, "do something")
	bad.EndChat(ctx)
	if bad.LastError() == nil {
		t.Fatal("LastError should be set after a failed turn")
	}

	// Clean turn → LastError nil.
	good := openSess(&captureProvider{})
	chat = good.StartChat(ctx)
	good.Submit(ctx, chat, "where is X handled?")
	good.EndChat(ctx)
	if err := good.LastError(); err != nil {
		t.Fatalf("LastError should be nil after a clean turn, got %v", err)
	}
}
