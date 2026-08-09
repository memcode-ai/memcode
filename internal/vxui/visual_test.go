package vxui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	vaxis "github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/widgets/term"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/theme"
	"github.com/memcode-ai/memcode/internal/wire"
	"path/filepath"
)

// slowProvider mimics a real gateway turn: it takes ~800ms, so the busy spinner re-renders the
// live region many times between the user echo and the reply — the condition that exposes a
// render that erases already-committed scrollback.
type slowProvider struct{}

func (slowProvider) Complete(ctx context.Context, _ wire.Request) (wire.Response, error) {
	select {
	case <-time.After(800 * time.Millisecond):
	case <-ctx.Done():
		return wire.Response{}, ctx.Err()
	}
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "ACKTOKEN"}}}, nil
}

func newSlowSession(t *testing.T) *runtime.Session {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return runtime.New(st, llm.NewRunner(slowProvider{}), t.TempDir(), "fake-model", permissions.ModeAuto, io.Discard)
}

// vxConsole is a headless vaxis Console that records output for replay through a term emulator.
type vxConsole struct {
	mu  sync.Mutex
	out bytes.Buffer
	in  strings.Reader
}

func newVxConsole() *vxConsole { return &vxConsole{in: *strings.NewReader("\x1b[?1;2c")} }
func (c *vxConsole) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.in.Len() == 0 {
		return 0, io.EOF
	}
	return c.in.Read(p)
}
func (c *vxConsole) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(p)
}
func (c *vxConsole) Fd() uintptr                       { return 0 }
func (c *vxConsole) SetRaw() error                     { return nil }
func (c *vxConsole) Reset() error                      { return nil }
func (c *vxConsole) Close() error                      { return nil }
func (c *vxConsole) Size() (int, int, int, int, error) { return 80, 24, 0, 0, nil }
func (c *vxConsole) Output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

// vxBackend wraps a real (headless) vaxis as a ui.Backend — mirrors the library's unexported
// vaxisBackend so a test exercises the genuine primary-screen render path.
type vxBackend struct {
	vx *vaxis.Vaxis
	ev chan ui.Event
}

func (b *vxBackend) Events() <-chan ui.Event { return b.ev }
func (b *vxBackend) Size() ui.Size {
	w := b.vx.Window()
	return ui.Size{Width: w.Width, Height: w.Height}
}
func (b *vxBackend) TerminalSize() ui.Resize { return b.vx.Size() }
func (b *vxBackend) Render(p *ui.Painter) error {
	win := b.vx.Window()
	win.Clear()
	size := p.Size()
	for y := 0; y < size.Height; y++ {
		for x := 0; x < size.Width; x++ {
			win.SetCell(x, y, p.Cell(x, y))
		}
	}
	if cursor, ok := p.Cursor(); ok {
		b.vx.ShowCursor(cursor.Col, cursor.Row, cursor.Shape)
	} else {
		b.vx.HideCursor()
	}
	b.vx.Render()
	return nil
}
func (b *vxBackend) Dispatch(fn func())                 { b.vx.PostEvent(ui.SyncFunc(fn)) }
func (b *vxBackend) SetMouseShape(s ui.MouseShape)      { b.vx.SetMouseShape(s) }
func (b *vxBackend) Close() error                       { b.vx.Close(); return nil }
func (b *vxBackend) Append(p []byte)                    { b.vx.Append(p) }
func (b *vxBackend) AppendString(s string)              { b.vx.AppendString(s) }
func (b *vxBackend) AppendWriter() io.Writer            { return b.vx.AppendWriter() }
func (b *vxBackend) SetPrimaryScreenRegionHeight(h int) { b.vx.SetPrimaryScreenRegionHeight(h) }

// TestBannerSurvivesVaxisStartup answers "did vaxis delete the pre-printed banner?" — it
// replays the banner followed by vaxis's startup+render output through a terminal emulator and
// checks the wordmark survives.
func TestBannerSurvivesVaxisStartup(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	banner := bannerString(context.Background(), sess, theme.Active().Palette)

	console := newVxConsole()
	vx, err := vaxis.New(vaxis.Options{
		DisableMouse:  true,
		WithConsole:   console,
		PrimaryScreen: &vaxis.PrimaryScreenOptions{RegionHeight: 5},
	})
	if err != nil {
		t.Fatalf("vaxis.New: %v", err)
	}
	win := vx.Window()
	win.Clear()
	win.Println(0, vaxis.Segment{Text: "live region"})
	vx.Render()
	vx.Close()

	vt := term.New()
	vt.Resize(80, 40)
	vt.WriteString(banner)           // banner reaches the terminal first
	vt.WriteString(console.Output()) // then vaxis startup + render
	grid := strings.Join(vt.Rows(), "\n")
	// The banner is the matrix-glyph wordmark (no "██" blocks); its stable "↺ ready" line is the
	// signature that the pre-printed banner survived vaxis startup instead of being wiped.
	if !strings.Contains(grid, "↺ ready") {
		t.Fatalf("vaxis startup WIPED the pre-printed banner:\n%s", grid)
	}
}

// TestSubmitEchoVisibleOnRealRender drives a real submit through the genuine vaxis primary-screen
// render path and asserts the user echo AND the reply are VISIBLE in the rendered grid (not just
// emitted) — catching write-then-erase bugs that the recording backend can't see.
func TestSubmitEchoVisibleOnRealRender(t *testing.T) {
	theme.Set("aurora")
	sess := newSlowSession(t)
	console := newVxConsole()
	vx, err := vaxis.New(vaxis.Options{
		DisableMouse:  true,
		WithConsole:   console,
		PrimaryScreen: &vaxis.PrimaryScreenOptions{RegionHeight: 6},
	})
	if err != nil {
		t.Fatalf("vaxis.New headless: %v", err)
	}
	defer vx.Close()

	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &vxBackend{vx: vx, ev: make(chan ui.Event, 256)}
	runner := ui.NewRunner(app, be, nil)
	go func() {
		for ev := range vx.Events() {
			be.ev <- ev
		}
	}()

	runner.Start(time.Now())
	_ = runner.HandleFrame(time.Now())

	vx.PostEvent(vaxis.Key{Text: "h", Keycode: 'h'})
	vx.PostEvent(vaxis.Key{Text: "i", Keycode: 'i'})
	vx.PostEvent(vaxis.Key{Keycode: vaxis.KeyEnter})

	grid := func() string {
		vt := term.New()
		vt.Resize(80, 24)
		vt.WriteString(console.Output())
		return strings.Join(vt.Rows(), "\n")
	}

	deadline := time.After(4 * time.Second)
	tick := time.NewTicker(15 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case ev := <-be.ev:
			runner.HandleEvent(ev, time.Now())
		case <-tick.C:
			_ = runner.HandleFrame(time.Now())
			if g := grid(); strings.Contains(g, "→ hi") && strings.Contains(g, "ACKTOKEN") {
				break loop
			}
		case <-deadline:
			break loop
		}
	}

	g := grid()
	if !strings.Contains(g, "→ hi") {
		t.Errorf("user echo '→ hi' NOT visible in rendered grid:\n%s\n--- raw ---\n%q", g, console.Output())
	}
	if !strings.Contains(g, "ACKTOKEN") {
		t.Errorf("reply 'ACKTOKEN' NOT visible in rendered grid:\n%s", g)
	}
}
