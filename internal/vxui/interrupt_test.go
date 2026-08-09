package vxui

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	vaxis "github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/theme"
	"github.com/memcode-ai/memcode/internal/wire"
)

// blockingProvider never returns on its own — it hangs until the request's context is
// canceled, then reports that. It stands in for a real in-flight model call so a test can
// drive Esc mid-turn and prove the turn actually stops, not just that the keypress was
// swallowed.
type blockingProvider struct {
	calls atomic.Int32
}

func (p *blockingProvider) Complete(ctx context.Context, _ wire.Request) (wire.Response, error) {
	p.calls.Add(1)
	<-ctx.Done()
	return wire.Response{}, ctx.Err()
}

// TestEscapeInterruptsBusyTurn: the status row advertises "esc to interrupt" while a turn
// is running, but plain Escape (no card/picker open) was a dead key — only Ctrl+C actually
// cancelled anything. Esc must cancel an in-flight turn exactly like Ctrl+C does.
func TestEscapeInterruptsBusyTurn(t *testing.T) {
	theme.Set("aurora")
	prov := &blockingProvider{}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess := runtime.New(st, llm.NewRunner(prov), t.TempDir(), "fake-model", permissions.ModeAuto, io.Discard)

	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now) // mount + paint

	for _, r := range "hi there" {
		runner.HandleEvent(vaxis.Key{Text: string(r), Keycode: r}, now)
	}
	_ = runner.HandleFrame(now)
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)
	_ = runner.HandleFrame(now)

	// Wait for the turn to actually reach the (blocked) model call.
	for i := 0; i < 200 && prov.calls.Load() == 0; i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(5 * time.Millisecond)
	}
	if prov.calls.Load() == 0 {
		t.Fatalf("turn never reached the model call — can't exercise the interrupt")
	}

	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, now)
	_ = runner.HandleFrame(now)

	for i := 0; i < 200 && !strings.Contains(be.recorded(), "interrupted"); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(be.recorded(), "interrupted") {
		t.Fatalf("Esc didn't interrupt the busy turn.\nrecorded=%q", be.recorded())
	}
}
