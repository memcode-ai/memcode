package vxui

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	vaxis "github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui/uitest"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/theme"
	"github.com/memcode-ai/memcode/internal/todos"
	"github.com/memcode-ai/memcode/internal/wire"
)

// recBackend is a headless ui.Backend that records scrollback appends, so a test can drive a
// real submit through the framework and assert what reaches scrollback (uitest can't — it has
// no primary-screen backend and AppendString panics there).
type recBackend struct {
	mu   sync.Mutex
	app  []string
	pend []func()
	ev   chan ui.Event
}

func (b *recBackend) Events() <-chan ui.Event     { return b.ev }
func (b *recBackend) Size() ui.Size               { return ui.Size{Width: 80, Height: 24} }
func (b *recBackend) Render(*ui.Painter) error    { return nil }
func (b *recBackend) SetMouseShape(ui.MouseShape) {}
func (b *recBackend) Close() error                { return nil }
func (b *recBackend) Append(p []byte)             { b.AppendString(string(p)) }
func (b *recBackend) AppendWriter() io.Writer {
	return writerFunc(func(p []byte) (int, error) { b.Append(p); return len(p), nil })
}
func (b *recBackend) Dispatch(fn func()) {
	b.mu.Lock()
	b.pend = append(b.pend, fn)
	b.mu.Unlock()
}
func (b *recBackend) AppendString(s string) {
	b.mu.Lock()
	b.app = append(b.app, s)
	b.mu.Unlock()
}
func (b *recBackend) recorded() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.app, "")
}
func (b *recBackend) drain() {
	b.mu.Lock()
	p := b.pend
	b.pend = nil
	b.mu.Unlock()
	for _, fn := range p {
		fn()
	}
}

// TestSlashMenuNav verifies the slash menu opens on "/" and ↑/↓ move the highlight.
func TestSlashMenuNav(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	app := uitest.New(&appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"})
	app.Pump(80, 24)

	app.Key("/")
	app.Pump(80, 24)
	if !app.Contains("❯ /help") {
		t.Fatalf("slash menu didn't open with /help highlighted.\n%q", app.Text())
	}
	app.Send(vaxis.Key{Keycode: vaxis.KeyDown})
	app.Pump(80, 24)
	if !app.Contains("❯ /login") {
		t.Fatalf("Down didn't move highlight to /login (the catalog's second entry).\n%q", app.Text())
	}
}

// TestThemePickerOpensAndNavigates verifies /theme opens the modal picker and ↑↓ works.
func TestThemePickerOpensAndNavigates(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	app := uitest.New(&appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"})
	app.Pump(80, 24)
	for _, r := range "/theme" {
		app.Key(string(r))
	}
	app.Pump(80, 24)
	app.Enter() // run /theme -> opens picker
	app.Pump(80, 24)
	if !app.Contains("Select a theme") {
		t.Fatalf("/theme didn't open the picker.\n%q", app.Text())
	}
	// A key RELEASE for Enter must NOT close the just-opened picker (kitty sends press+release).
	app.Send(vaxis.Key{Keycode: vaxis.KeyEnter, EventType: vaxis.EventRelease})
	app.Pump(80, 24)
	if !app.Contains("Select a theme") {
		t.Fatalf("Enter release closed the picker (release not filtered).\n%q", app.Text())
	}
	app.Send(vaxis.Key{Keycode: vaxis.KeyDown})
	app.Pump(80, 24)
	if !app.Contains("↑↓ preview") {
		t.Fatalf("picker hint missing.\n%q", app.Text())
	}
}

// TestSubmitEchoesAndResponds drives a real submit through the framework with a recording
// backend and asserts BOTH the user echo and the engine reply reach scrollback.
func TestSubmitEchoesAndResponds(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now) // mount + paint (AutoFocus composer)

	runner.HandleEvent(vaxis.Key{Text: "h", Keycode: 'h'}, now)
	runner.HandleEvent(vaxis.Key{Text: "i", Keycode: 'i'}, now)
	_ = runner.HandleFrame(now)
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)

	// Echo is styled (SGR runs split "→ " and "hi"), so check both tokens rather than a
	// contiguous substring; the visual/grid test asserts the rendered "→ hi".
	if rec := be.recorded(); !strings.Contains(rec, "→") || !strings.Contains(rec, "hi") {
		t.Fatalf("user echo missing from scrollback after submit.\nrecorded=%q", be.recorded())
	}
	for i := 0; i < 80 && !strings.Contains(be.recorded(), "ACKTOKEN"); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(be.recorded(), "ACKTOKEN") {
		t.Fatalf("engine reply missing from scrollback.\nrecorded=%q", be.recorded())
	}
}

// TestShiftEnterInsertsNewline locks the multi-line composer: Shift+Enter is a SOFT return
// (inserts "\n", does not submit), and Enter submits the whole multi-line buffer. The vaxis
// rewrite had dropped this — Enter was the only binding and the composer was stuck on one
// line. Old behavior would have submitted "ab"; the fix submits "a\nb".
func TestShiftEnterInsertsNewline(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)

	runner.HandleEvent(vaxis.Key{Text: "a", Keycode: 'a'}, now)
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter, Modifiers: vaxis.ModShift}, now) // soft return
	runner.HandleEvent(vaxis.Key{Text: "b", Keycode: 'b'}, now)
	_ = runner.HandleFrame(now)

	// Shift+Enter must NOT have submitted — nothing echoed to scrollback yet.
	if rec := be.recorded(); strings.TrimSpace(rec) != "" {
		t.Fatalf("Shift+Enter submitted instead of inserting a newline.\nrecorded=%q", rec)
	}

	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now) // Enter submits the whole buffer
	rec := be.recorded()
	if !strings.Contains(rec, "a") || !strings.Contains(rec, "b") || strings.Contains(rec, "ab") {
		t.Fatalf("expected the submitted buffer to span two lines (a, then b — never adjacent \"ab\").\nrecorded=%q", rec)
	}
}

// TestShiftBackspaceDeletes locks the fix for: holding Shift while deleting (common right after
// typing capitals) reported "Shift+BackSpace", which the name-matched switch missed, so the
// character wasn't deleted. Shift is meaningless for cursor/editing keys, so it must still delete.
func TestShiftBackspaceDeletes(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)

	runner.HandleEvent(vaxis.Key{Text: "q", Keycode: 'q'}, now)
	runner.HandleEvent(vaxis.Key{Text: "z", Keycode: 'z'}, now)
	// Shift held while deleting — the keyboard protocol reports Shift+BackSpace; it must still delete.
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyBackspace, Modifiers: vaxis.ModShift}, now)
	_ = runner.HandleFrame(now)
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now) // submit what's left
	_ = runner.HandleFrame(now)

	rec := be.recorded()
	if strings.Contains(rec, "qz") {
		t.Fatalf("Shift+BackSpace did not delete — the submitted buffer still has \"qz\".\nrecorded=%q", rec)
	}
	if !strings.Contains(rec, "q") {
		t.Fatalf("expected the surviving char \"q\" in the echo.\nrecorded=%q", rec)
	}
}

// TestPasteDoesNotAutoSubmit locks the bracketed-paste fix: a multi-line paste is buffered
// into ONE turn and does NOT submit until Enter — the vaxis port had regressed so each embedded
// newline fired its own turn (the "splits into individual commands and executes immediately"
// bug). A SMALL paste (≤5 lines) inlines into the composer; Enter then sends it as one turn.
func TestPasteDoesNotAutoSubmit(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)

	runner.HandleEvent(vaxis.PasteStartEvent{}, now)
	for _, r := range "first line\nsecond line\nthird line" {
		runner.HandleEvent(vaxis.Key{Text: string(r), EventType: vaxis.EventPaste}, now)
	}
	runner.HandleEvent(vaxis.PasteEndEvent{}, now)
	_ = runner.HandleFrame(now)

	// The regression: a multi-line paste must NOT have submitted anything yet.
	if rec := be.recorded(); strings.TrimSpace(rec) != "" {
		t.Fatalf("paste auto-submitted before Enter.\nrecorded=%q", rec)
	}

	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now) // now it sends, as ONE turn
	// A small (≤5-line) paste inlines, so the echo carries the literal content — as one turn.
	if rec := be.recorded(); !strings.Contains(rec, "first line") || !strings.Contains(rec, "third line") {
		t.Fatalf("expected the inlined paste content in the echo.\nrecorded=%q", rec)
	}
	for i := 0; i < 80 && !strings.Contains(be.recorded(), "ACKTOKEN"); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(be.recorded(), "ACKTOKEN") {
		t.Fatalf("engine never received the pasted turn.\nrecorded=%q", be.recorded())
	}
}

// TestLargePasteCollapsesToChip: a paste over the small-inline threshold (here, >5 lines) is
// collapsed to a [pasted #n] chip rather than dumped into the composer, and still doesn't
// auto-submit. This is the counterpart to the small-paste inline path above.
func TestLargePasteCollapsesToChip(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)

	runner.HandleEvent(vaxis.PasteStartEvent{}, now)
	for _, r := range "l1\nl2\nl3\nl4\nl5\nl6\nl7" { // 7 lines → over the inline cap
		runner.HandleEvent(vaxis.Key{Text: string(r), EventType: vaxis.EventPaste}, now)
	}
	runner.HandleEvent(vaxis.PasteEndEvent{}, now)
	_ = runner.HandleFrame(now)

	if rec := be.recorded(); strings.TrimSpace(rec) != "" {
		t.Fatalf("paste auto-submitted before Enter.\nrecorded=%q", rec)
	}
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)
	if rec := be.recorded(); !strings.Contains(rec, "pasted") {
		t.Fatalf("expected a [pasted …] chip in the echo for a large paste.\nrecorded=%q", rec)
	}
}

// TestDraggedImageCollapsesToChip: a drag-and-drop (a bracketed paste of an image path)
// collapses to an [Image #1] chip, doesn't auto-submit, and the chip survives to the echo.
func TestDraggedImageCollapsesToChip(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)

	img := filepath.Join(t.TempDir(), "shot.png") // valid PNG signature so the content sniff classifies it as an image
	if err := os.WriteFile(img, []byte("\x89PNG\r\n\x1a\nshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner.HandleEvent(vaxis.PasteStartEvent{}, now)
	for _, r := range img {
		runner.HandleEvent(vaxis.Key{Text: string(r), EventType: vaxis.EventPaste}, now)
	}
	runner.HandleEvent(vaxis.PasteEndEvent{}, now)
	_ = runner.HandleFrame(now)
	if rec := be.recorded(); strings.TrimSpace(rec) != "" {
		t.Fatalf("image paste auto-submitted before Enter.\nrecorded=%q", rec)
	}
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)
	if rec := be.recorded(); !strings.Contains(rec, "[Image #1]") {
		t.Fatalf("dragged image did not collapse to an [Image #1] chip.\nrecorded=%q", rec)
	}
}

// TestLargePasteTruncated: a paste past maxPasteBytes is capped (not buffered whole / sent
// whole) and flagged to the user — a 1GB paste can't OOM or bomb the context.
func TestLargePasteTruncated(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)

	runner.HandleEvent(vaxis.PasteStartEvent{}, now)
	for i := 0; i < maxPasteBytes+4096; i++ { // exceed the inline cap
		runner.HandleEvent(vaxis.Key{Text: "x", EventType: vaxis.EventPaste}, now)
	}
	runner.HandleEvent(vaxis.PasteEndEvent{}, now)
	_ = runner.HandleFrame(now)

	// The truncation notice is written to scrollback at commit, before any submit.
	if rec := be.recorded(); !strings.Contains(rec, "truncated") {
		t.Fatalf("oversized paste was not flagged as truncated.\nrecorded=%q", rec)
	}
}

// TestMultipleImagesCollapse: a drop of several image paths becomes one chip EACH
// ([Image #1] [Image #2] …), not a single [pasted] blob.
func TestMultipleImagesCollapse(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)

	dir := t.TempDir()
	var paths []string
	for _, n := range []string{"a.png", "b.png", "c.png"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("\x89PNG\r\n\x1a\n"+n), 0o644); err != nil { // valid PNG signature
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	runner.HandleEvent(vaxis.PasteStartEvent{}, now)
	for _, r := range strings.Join(paths, " ") {
		runner.HandleEvent(vaxis.Key{Text: string(r), EventType: vaxis.EventPaste}, now)
	}
	runner.HandleEvent(vaxis.PasteEndEvent{}, now)
	_ = runner.HandleFrame(now)
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)

	rec := be.recorded()
	for _, want := range []string{"[Image #1]", "[Image #2]", "[Image #3]"} {
		if !strings.Contains(rec, want) {
			t.Fatalf("missing %s — multi-image drop didn't collapse per-image.\nrecorded=%q", want, rec)
		}
	}
}

// fakeProvider returns a fixed turn so a session can be constructed without a gateway.
type fakeProvider struct{}

func (fakeProvider) Complete(_ context.Context, _ wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "ACKTOKEN"}}}, nil
}

// Advise satisfies provider.Advisor so /advisor dispatches through AskAdvisor in tests
// (production uses the real gateway client). Returns a fixed advice string.
func (fakeProvider) Advise(_ context.Context, _ string, _ string) (string, error) {
	return "ADVISORTOKEN", nil
}

func newTestSession(t *testing.T) *runtime.Session {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess := runtime.New(st, llm.NewRunner(fakeProvider{}), t.TempDir(), "fake-model", permissions.ModeAuto, io.Discard)
	// The tests drive frames by hand and never dispose the app state, so end
	// the chat here — it joins the async learning loop, whose writes under the
	// repo root otherwise race the TempDir cleanup (the CI flake).
	t.Cleanup(func() { sess.EndChat(context.Background()) })
	return sess
}

// stateCapture is a test-only root widget that wraps appWidget but captures a pointer to the
// created appState — so a test can set state (e.g. s.todos) before driving an event. It mirrors
// appWidget's CreateState and routes every Build/HandleEvent to the real state.
type stateCapture struct {
	appWidget
	state **appState
}

func (w *stateCapture) CreateState() ui.State {
	s := &appState{w: &w.appWidget}
	*w.state = s
	return s
}

// TestLiveRegionRenders drives the real appWidget through the ui framework's headless
// harness and asserts the live region (status · composer · footer) builds and paints.
// Scrollback append needs a primary-screen backend (it panics under uitest), so it's
// verified on a real terminal — the framework owns that path now, not hand-rolled code.
func TestLiveRegionRenders(t *testing.T) {
	sess := newTestSession(t)
	app := uitest.New(&appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"})
	app.Pump(80, 24)

	for _, want := range []string{"memcode", "○ idle", "→"} {
		if !app.Contains(want) {
			t.Fatalf("live region missing %q.\n--- rendered ---\n%q", want, app.Text())
		}
	}
}

// newRecRunner wires a real appWidget to a recBackend runner and mounts it — the shared
// scaffolding for any test that drives a slash command and asserts its scrollback output.
// Returns the session, the backend (to read recorded scrollback), and the runner (to pump).
func newRecRunner(t *testing.T) (*runtime.Session, *recBackend, *ui.Runner) {
	t.Helper()
	theme.Set("aurora")
	sess := newTestSession(t)
	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now) // mount + paint
	return sess, be, runner
}

// newRecRunnerCapture is the same as newRecRunner but uses a stateCapture root so the test can
// grab the appState and set fields (e.g. s.todos) before driving an event.
func newRecRunnerCapture(t *testing.T) (*appState, *runtime.Session, *recBackend, *ui.Runner) {
	t.Helper()
	theme.Set("aurora")
	sess := newTestSession(t)
	var st *appState
	root := &stateCapture{appWidget: appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}, state: &st}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now) // mount + paint → InitState ran → st is live
	return st, sess, be, runner
}

// typeSlash sends the runes of a slash command, pumps, and submits it with Enter. now is the
// timestamp for HandleEvent/HandleFrame; the runner owns the timeline.
func typeSlash(runner *ui.Runner, cmd string, now time.Time) {
	for _, r := range cmd {
		runner.HandleEvent(vaxis.Key{Text: string(r), Keycode: r}, now)
	}
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)
}

// TestSlashAdvisorDispatches: /advisor runs AskAdvisor through the (fake) provider and the
// advice reaches scrollback. Locks the async dispatch wiring — the old stub printed "isn't
// ported yet".
func TestSlashAdvisorDispatches(t *testing.T) {
	sess, be, runner := newRecRunner(t)
	_ = sess
	now := time.Now()
	typeSlash(runner, "/advisor how should I test this?", now)

	// /advisor is async (runAsync) — drain dispatched closures until the advice lands.
	for i := 0; i < 80 && !strings.Contains(be.recorded(), "ADVISORTOKEN"); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(be.recorded(), "ADVISORTOKEN") {
		t.Fatalf("advisor response missing from scrollback.\nrecorded=%q", be.recorded())
	}
}

// TestSlashTodosSnapshot: /todos mirrors the live task checklist into scrollback. Uses a
// stateCapture root so the test can seed s.todos before running the command.
func TestSlashTodosSnapshot(t *testing.T) {
	st, sess, be, runner := newRecRunnerCapture(t)
	_ = sess
	now := time.Now()
	st.todos = todos.FromTitles([]string{"inspect the loader", "add the upsert path", "update the tests"})
	typeSlash(runner, "/todos", now)
	_ = runner.HandleFrame(now)
	rec := be.recorded()
	if !strings.Contains(rec, "Tasks") || !strings.Contains(rec, "0/3 done") {
		t.Fatalf("/todos summary missing from scrollback.\nrecorded=%q", rec)
	}
	if !strings.Contains(rec, "inspect the loader") || !strings.Contains(rec, "add the upsert path") {
		t.Fatalf("/todos checklist items missing from scrollback.\nrecorded=%q", rec)
	}
}

// TestSlashTodosEmpty: /todos with no active tasks says so, rather than printing an empty list.
func TestSlashTodosEmpty(t *testing.T) {
	_, be, runner := newRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/todos", now)
	_ = runner.HandleFrame(now)
	if !strings.Contains(be.recorded(), "no active tasks") {
		t.Fatalf("expected 'no active tasks' for an empty todo list.\nrecorded=%q", be.recorded())
	}
}

// TestSlashTodoAliasDispatchesAsTodos: /todo is a pure alias of /todos (slashAliases). It must
// dispatch to the same handler — so a seeded checklist reaches scrollback either way.
func TestSlashTodoAliasDispatchesAsTodos(t *testing.T) {
	st, sess, be, runner := newRecRunnerCapture(t)
	_ = sess
	now := time.Now()
	st.todos = todos.FromTitles([]string{"wire the command", "write the test"})
	typeSlash(runner, "/todo", now)
	_ = runner.HandleFrame(now)
	if !strings.Contains(be.recorded(), "wire the command") {
		t.Fatalf("/todo alias didn't dispatch as /todos.\nrecorded=%q", be.recorded())
	}
}

// TestSlashThemesAliasOpensPicker: /themes is a pure alias of /theme (slashAliases). It must
// open the SAME theme picker as /theme — the regression was that the alias was registered in
// slashAliases but runSlash never consulted the map, so /themes fell to "unknown command".
func TestSlashThemesAliasOpensPicker(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	app := uitest.New(&appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"})
	app.Pump(80, 24)
	for _, r := range "/themes" {
		app.Key(string(r))
	}
	app.Pump(80, 24)
	app.Enter() // run /themes → must open the picker (same as /theme)
	app.Pump(80, 24)
	if !app.Contains("Select a theme") {
		t.Fatalf("/themes alias didn't open the theme picker.\n%q", app.Text())
	}
}

// TestSlashAliasesAllDispatch is the guard that would have caught the /themes regression: EVERY
// alias in slashAliases must dispatch to its canonical handler — i.e. it must NOT print
// "unknown command". Before the fix, runSlash hand-listed aliases as second case labels and
// /themes (added only to the map) fell through to the default. Now runSlash resolves aliases
// from the map before the switch, so this holds for every alias by construction.
func TestSlashAliasesAllDispatch(t *testing.T) {
	for alias, canonical := range slashAliases {
		t.Run(alias, func(t *testing.T) {
			_, be, runner := newRecRunner(t)
			now := time.Now()
			typeSlash(runner, alias, now)
			// Drain async dispatch + pump frames until the handler settles.
			for i := 0; i < 20; i++ {
				be.drain()
				_ = runner.HandleFrame(now)
			}
			rec := be.recorded()
			if strings.Contains(rec, "unknown command") {
				t.Fatalf("alias %q (→ %q) printed 'unknown command' — it didn't dispatch.\nrecorded=%q", alias, canonical, rec)
			}
		})
	}
}

// TestSlashDebugSummary: /debug prints a compact runtime summary — session id, model, token
// counts, context fill, wire-trace state — to scrollback. Locks the synchronous dispatch.
func TestSlashDebugSummary(t *testing.T) {
	sess, be, runner := newRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/debug", now)
	_ = runner.HandleFrame(now)
	rec := be.recorded()
	for _, want := range []string{"session ", "model ", "tokens", "context ", "wire trace"} {
		if !strings.Contains(rec, want) {
			t.Fatalf("/debug summary missing %q.\nrecorded=%q", want, rec)
		}
	}
	_ = sess
}

// TestSyncPickerOpens: /sync opens the multi-select target picker (rendered in the live region,
// so this is a uitest/grid assertion, not scrollback).
func TestSyncPickerOpens(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	app := uitest.New(&appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"})
	app.Pump(80, 24)
	for _, r := range "/sync" {
		app.Key(string(r))
	}
	app.Pump(80, 24)
	app.Enter() // run /sync → opens picker
	app.Pump(80, 24)
	if !app.Contains("Select sync targets") {
		t.Fatalf("/sync didn't open the picker.\n%q", app.Text())
	}
	if !app.Contains("Space toggle") {
		t.Fatalf("sync picker hint missing.\n%q", app.Text())
	}
}

// TestSyncPickerToggles: Space toggles the row under the cursor — the checkbox flips between
// ☑ and ☐ in the rendered view. Locks the multi-select UX (Space, not Enter, is the toggle).
func TestSyncPickerToggles(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	app := uitest.New(&appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"})
	app.Pump(80, 24)
	for _, r := range "/sync" {
		app.Key(string(r))
	}
	app.Pump(80, 24)
	app.Enter()
	app.Pump(80, 24)
	// Snapshot the checkbox state of the first row, then toggle with Space and re-check.
	before := app.Text()
	app.Send(vaxis.Key{Keycode: vaxis.KeySpace}) // Space toggles the highlighted row
	app.Pump(80, 24)
	after := app.Text()
	if before == after {
		t.Fatalf("Space didn't change the picker view (checkbox didn't toggle).\nbefore=%q\nafter=%q", before, after)
	}
	// The cursor row (❯) should now show the opposite box. At least one ☑/☐ count must differ.
	if strings.Count(before, "☑") == strings.Count(after, "☑") {
		t.Fatalf("Space didn't flip a checkbox (☑ count unchanged).\nbefore=%q\nafter=%q", before, after)
	}
}

// TestSyncPickerCancels: Esc closes the picker without syncing — the "Select sync targets"
// header disappears from the rendered view. Uses the recBackend runner (not uitest) because
// handleSyncKey calls sysln("sync cancelled") on Esc, and scrollback append needs a primary
// screen. The picker state is observable via the next Build.
func TestSyncPickerCancels(t *testing.T) {
	_, be, runner := newRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/sync", now)
	_ = runner.HandleFrame(now) // /sync runs → picker opens
	// Confirm the picker is open by checking the rendered view captured a "Select sync targets"
	// Build — the recBackend records appends, but we can verify the picker closed by confirming
	// the cancel message reached scrollback (handleSyncKey prints "sync cancelled" on Esc).
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, now) // Esc cancels
	_ = runner.HandleFrame(now)
	if !strings.Contains(be.recorded(), "sync cancelled") {
		t.Fatalf("Esc didn't close the sync picker (no 'sync cancelled' in scrollback).\nrecorded=%q", be.recorded())
	}
}

// TestSlashModelInCatalog locks that /model is in the slash catalog (so it
// autocompletes, is recognized by isKnownSlash, and shows in /help).
func TestSlashModelInCatalog(t *testing.T) {
	found := false
	for _, c := range slashCommands {
		if c.name == "/model" {
			found = true
			break
		}
	}
	if !found {
		t.Error("/model missing from slashCommands catalog")
	}
	// /model must be recognized by isKnownSlash (with and without args).
	if !isKnownSlash("/model", false) {
		t.Error("/model not recognized by isKnownSlash")
	}
	if !isKnownSlash("/model anthropic", false) {
		t.Error("/model (with args) not recognized by isKnownSlash")
	}
}

// TestSlashModelSetsVendor: /model <vendor> keeps working as the legacy Automatic
// strong-tier switch (it also clears any pin). Locks the direct-set path (no picker).
func TestSlashModelSetsVendor(t *testing.T) {
	sess, be, runner := newRecRunner(t)
	_ = sess
	now := time.Now()
	typeSlash(runner, "/model anthropic", now)
	_ = runner.HandleFrame(now)
	rec := be.recorded()
	if !strings.Contains(rec, "strong tier Anthropic") {
		t.Fatalf("/model anthropic didn't print the confirmation.\nrecorded=%q", rec)
	}
	if sess.Vendor() != "anthropic" {
		t.Fatalf("session vendor = %q, want \"anthropic\"", sess.Vendor())
	}
	if sess.Pin() != "" {
		t.Fatalf("vendor switch must clear the pin, got %q", sess.Pin())
	}
}

// TestSlashModelPinsLabel: /model <label> pins that model (the gateway validates
// for real — an unknown label falls through to Automatic server-side), and
// /model auto releases it. The vendor is untouched by a pin.
func TestSlashModelPinsLabel(t *testing.T) {
	sess, be, runner := newRecRunner(t)
	_ = sess
	now := time.Now()
	typeSlash(runner, "/model sonnet", now)
	_ = runner.HandleFrame(now)
	rec := be.recorded()
	if !strings.Contains(rec, "model → sonnet") {
		t.Fatalf("/model sonnet didn't print the pin confirmation.\nrecorded=%q", rec)
	}
	if sess.Pin() != "sonnet" {
		t.Fatalf("session pin = %q, want \"sonnet\"", sess.Pin())
	}
	if sess.Vendor() != "" {
		t.Fatalf("a pin must not change the vendor, got %q", sess.Vendor())
	}
	typeSlash(runner, "/model auto", now)
	_ = runner.HandleFrame(now)
	if sess.Pin() != "" {
		t.Fatalf("/model auto must clear the pin, got %q", sess.Pin())
	}
}

// TestSubmitEchoesSlashCommand: submitting a slash command echoes the typed line into
// scrollback — the same composer-prompt style used for a chat turn — BEFORE the command's
// own confirmation line. Before this, a slash command left no trace of what was invoked, only
// its result (e.g. "model → sonnet" with no idea /model was the command that produced it).
func TestSubmitEchoesSlashCommand(t *testing.T) {
	_, be, runner := newRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/mode ask", now)
	_ = runner.HandleFrame(now)
	rec := be.recorded()
	echoAt := strings.Index(rec, "/mode ask")
	resultAt := strings.Index(rec, "mode → ask")
	if echoAt < 0 {
		t.Fatalf("typed command wasn't echoed to scrollback.\nrecorded=%q", rec)
	}
	if resultAt < 0 {
		t.Fatalf("command result missing from scrollback.\nrecorded=%q", rec)
	}
	if echoAt >= resultAt {
		t.Fatalf("echo must appear before the result, got echo@%d result@%d in %q", echoAt, resultAt, rec)
	}
}

// TestModelPickerEnterUsesFriendlyNameAndWindow: picking a model from the /model picker
// confirms with the picker's friendly name + context window ("model → Sonnet 4.6 · 1M
// context"), not the bare gateway label ("model → sonnet") — the confirmation should read as
// well as the picker row did. Seeds the picker state directly (stateCapture) rather than
// driving the real async gateway fetch, which isn't reachable offline in tests.
func TestModelPickerEnterUsesFriendlyNameAndWindow(t *testing.T) {
	st, sess, be, runner := newRecRunnerCapture(t)
	now := time.Now()
	st.modelEntries = []modelEntry{
		{}, // row 0: Automatic
		{label: "sonnet", name: "Sonnet 4.6", desc: "Efficient for routine tasks", window: 1_000_000},
	}
	st.modelSel = 1
	st.modelPicking = true
	_ = runner.HandleFrame(now)

	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)
	_ = runner.HandleFrame(now)

	rec := be.recorded()
	if !strings.Contains(rec, "model → Sonnet 4.6") {
		t.Fatalf("confirmation didn't use the picker's friendly name.\nrecorded=%q", rec)
	}
	if !strings.Contains(rec, "1M context") {
		t.Fatalf("confirmation didn't include the context window.\nrecorded=%q", rec)
	}
	if sess.Pin() != "sonnet" {
		t.Fatalf("session pin = %q, want \"sonnet\"", sess.Pin())
	}
	if st.modelPicking {
		t.Fatalf("picker must close after Enter")
	}
}

// TestIsKnownSlashConsolidated is the guard test for the catalog collapse: EVERY catalog command
// and EVERY alias must be recognized, and a non-command path must not be. This locks that you
// can't add a command to one list (catalog or alias) and forget the other — isKnownSlash and
// autocomplete both flow from the same two tables now.
func TestIsKnownSlashConsolidated(t *testing.T) {
	// Every catalog entry is recognized, with and without args.
	for _, c := range slashCommands {
		if !isKnownSlash(c.name, false) {
			t.Errorf("catalog command %q not recognized by isKnownSlash", c.name)
		}
		if !isKnownSlash(c.name+" some args here", false) {
			t.Errorf("catalog command %q (with args) not recognized by isKnownSlash", c.name)
		}
	}
	// Every alias resolves to its canonical command.
	for alias, canonical := range slashAliases {
		if !isKnownSlash(alias, false) {
			t.Errorf("alias %q not recognized by isKnownSlash", alias)
		}
		if !isKnownSlash(canonical, false) {
			t.Errorf("canonical %q (target of alias %q) not in catalog", canonical, alias)
		}
	}
	// A non-command path is NOT a slash command (the whole point of isKnownSlash).
	if isKnownSlash("/var/log/syslog", false) {
		t.Errorf("non-command path /var/log/syslog was misrecognized as a slash command")
	}
	if isKnownSlash("/Users/someone/file.go", false) {
		t.Errorf("non-command path was misrecognized as a slash command")
	}
	// Empty and whitespace-only lines are not commands.
	if isKnownSlash("", false) || isKnownSlash("   ", false) {
		t.Errorf("empty/whitespace line misrecognized as a slash command")
	}
}

// TestSlashCommandsIncludeDispatch: /dispatch and /agents are in the slash catalog (so they
// autocomplete, are recognized by isKnownSlash, and show in /help). The consolidated guard test
// above already verifies recognition — this locks the specific entries exist.
func TestSlashCommandsIncludeDispatch(t *testing.T) {
	found := map[string]bool{}
	for _, c := range slashCommands {
		found[c.name] = true
	}
	if !found["/dispatch"] {
		t.Error("/dispatch missing from slashCommands catalog")
	}
	if !found["/agents"] {
		t.Error("/agents missing from slashCommands catalog")
	}
}

// TestSlashDispatchUsageOnEmpty: /dispatch with no task prints a usage line to scrollback
// (no agent is spawned). Locks the synchronous validation path.
func TestSlashDispatchUsageOnEmpty(t *testing.T) {
	_, be, runner := newRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/dispatch", now)
	_ = runner.HandleFrame(now)
	if !strings.Contains(be.recorded(), "usage:") {
		t.Fatalf("expected usage line for /dispatch with no task.\nrecorded=%q", be.recorded())
	}
}

// TestSlashAgentsNoAgents: /agents with no dispatched agents says so. Locks the async list
// path (jobs.List on a temp dir returns nothing).
func TestSlashAgentsNoAgents(t *testing.T) {
	_, be, runner := newRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/agents", now)
	// /agents is async (runAsync) — drain dispatched closures until the result lands. Wait
	// for the specific result text, not a bare "agents" substring — the echoed "/agents"
	// command itself now contains that substring and would satisfy it immediately.
	for i := 0; i < 80 && !strings.Contains(be.recorded(), "no dispatched agents"); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(be.recorded(), "no dispatched agents") {
		t.Fatalf("expected 'no dispatched agents' for an empty job dir.\nrecorded=%q", be.recorded())
	}
}

// TestSlashAgentsStopUsageOnEmptyId: /agents stop with no id prints a usage line. Locks
// the stop subcommand routing (it must parse "stop <id>" and not fall through to the list).
func TestSlashAgentsStopUsageOnEmptyId(t *testing.T) {
	_, be, runner := newRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/agents stop", now)
	_ = runner.HandleFrame(now)
	if !strings.Contains(be.recorded(), "usage:") {
		t.Fatalf("expected usage line for /agents stop with no id.\nrecorded=%q", be.recorded())
	}
}

// TestSlashAgentsStopNotFoundError: /agents stop <id> on a non-existent job reports the
// error to scrollback (the jobs.Stop call returns "no job"). Locks the async stop path
// end-to-end through the TUI.
func TestSlashAgentsStopNotFoundError(t *testing.T) {
	_, be, runner := newRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/agents stop job_nonexistent", now)
	for i := 0; i < 80 && !strings.Contains(be.recorded(), "couldn't stop"); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(be.recorded(), "couldn't stop") {
		t.Fatalf("expected a 'couldn't stop' error for a non-existent job id.\nrecorded=%q", be.recorded())
	}
}

// TestFooterAgentCount: the footer renders "N agents" when s.agents > 0. Uses a uitest
// app with a stateCapture root so we can set s.agents before pumping, then assert the
// rendered text contains the agent count.
func TestFooterAgentCount(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	var st *appState
	root := &stateCapture{appWidget: appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}, state: &st}
	app := uitest.New(root)
	app.Pump(80, 24)
	// Set agents=3 on the live state and re-pump → footer should show "3 agents".
	st.SetState(func() { st.agents = 3 })
	app.Pump(80, 24)
	if !app.Contains("3 agents") {
		t.Fatalf("footer should show '3 agents' when s.agents=3.\n--- rendered ---\n%q", app.Text())
	}
	// Reset to 0 → "agents" should NOT appear in the footer.
	st.SetState(func() { st.agents = 0 })
	app.Pump(80, 24)
	// The footer may contain "agents" from other sources (e.g. /agents in the slash menu),
	// but the footer row itself shouldn't have "N agent(s)". Check the specific segment is gone:
	// when agents=0 the footer doesn't add the segment at all.
	if app.Contains("3 agents") || app.Contains("1 agent") || app.Contains("2 agents") {
		t.Fatalf("footer should NOT show agent count when s.agents=0.\n--- rendered ---\n%q", app.Text())
	}
}

// TestAgentCountFromJobsDir: agentCount reads the jobs directory and returns the number of
// running agents. With no jobs dir, it returns 0; with a finished job meta, it returns 0
// (only running jobs count).
func TestAgentCountFromJobsDir(t *testing.T) {
	dir := t.TempDir()
	// No jobs dir → 0.
	if n := agentCount(dir); n != 0 {
		t.Errorf("expected 0 agents with no jobs dir, got %d", n)
	}
	// Create a jobs dir with a finished job meta → still 0 (only running counts).
	jobsDir := filepath.Join(dir, ".memcode", "jobs", "job_test1")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"id":"job_test1","task":"test","mode":"auto","pid":999999,"status":"done","exit_code":0,"started_at":"2025-01-01T00:00:00Z"}`)
	if err := os.WriteFile(filepath.Join(jobsDir, "meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := agentCount(dir); n != 0 {
		t.Errorf("expected 0 running agents with a finished job, got %d", n)
	}
}

// TestSeedSeenAgentsNoNotifyOnLaunch: seedSeenAgents initializes the map at current statuses
// so already-finished jobs don't trigger a spurious notification on the first refreshFooter.
func TestSeedSeenAgentsNoNotifyOnLaunch(t *testing.T) {
	dir := t.TempDir()
	// Create a finished job.
	jobsDir := filepath.Join(dir, ".memcode", "jobs", "job_done")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"id":"job_done","task":"test","mode":"auto","pid":999999,"status":"done","exit_code":0,"started_at":"2025-01-01T00:00:00Z"}`)
	if err := os.WriteFile(filepath.Join(jobsDir, "meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	seen := seedSeenAgents(dir)
	if seen["job_done"] != "done" {
		t.Errorf("seed should record done status, got %q", seen["job_done"])
	}
	// No notifications (or report-backs) should fire (the job was already done when seeded).
	notes, backs := agentDoneNotifications(dir, seen)
	if len(notes) > 0 {
		t.Errorf("expected no notifications for a pre-finished job, got %d: %v", len(notes), notes)
	}
	if len(backs) > 0 {
		t.Errorf("expected no report-backs for a pre-finished job, got %d", len(backs))
	}
}

// TestSlashForkInCatalog locks /fork into the slash catalog (autocomplete,
// isKnownSlash, /help).
func TestSlashForkInCatalog(t *testing.T) {
	found := false
	for _, c := range slashCommands {
		if c.name == "/fork" {
			found = true
			break
		}
	}
	if !found {
		t.Error("/fork missing from slashCommands catalog")
	}
	if !isKnownSlash("/fork", false) || !isKnownSlash("/fork sess_abc", false) {
		t.Error("/fork not recognized by isKnownSlash")
	}
}

// A fresh session has no saved transcript yet — /fork must refuse gracefully and
// leave the live session running.
func TestSlashForkNothingToFork(t *testing.T) {
	sess, be, runner := newRecRunner(t)
	before := sess.SessionID()
	now := time.Now()
	typeSlash(runner, "/fork", now)
	_ = runner.HandleFrame(now)
	if !strings.Contains(be.recorded(), "nothing to fork") {
		t.Fatalf("/fork on an empty session should refuse.\nrecorded=%q", be.recorded())
	}
	if sess.SessionID() != before {
		t.Fatal("a refused fork must not change the session")
	}
}

// /fork with a saved transcript enters a NEW session id carrying the history; the
// original stays resumable.
func TestSlashForkForks(t *testing.T) {
	sess, be, runner := newRecRunner(t)
	root, src := sess.Root(), sess.SessionID()
	dir := filepath.Join(root, ".memcode", "sessions", src)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"session_id":"` + src + `","saved_at":"2026-07-15T00:00:00Z","messages":[{"role":"user","blocks":[{"type":"text","text":"hi"}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "messages.json"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	typeSlash(runner, "/fork", now)
	_ = runner.HandleFrame(now)
	if !strings.Contains(be.recorded(), "forked "+src) {
		t.Fatalf("/fork didn't announce the fork.\nrecorded=%q", be.recorded())
	}
	if sess.SessionID() == src {
		t.Fatal("/fork must enter a NEW session id")
	}
}

// TestEscTwiceClearsComposer: with text in the composer, one Esc arms and a second
// Esc clears it. A single Esc leaves the text alone.
func TestEscTwiceClearsComposer(t *testing.T) {
	st, _, _, runner := newRecRunnerCapture(t)
	now := time.Now()

	runner.HandleEvent(vaxis.Key{Text: "h", Keycode: 'h'}, now)
	runner.HandleEvent(vaxis.Key{Text: "i", Keycode: 'i'}, now)
	if st.composer != "hi" {
		t.Fatalf("composer = %q, want %q", st.composer, "hi")
	}
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, now) // arms, does not clear
	if st.composer != "hi" {
		t.Fatalf("a single Esc must not clear the composer, got %q", st.composer)
	}
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, now) // clears
	if st.composer != "" {
		t.Fatalf("two Escapes must clear the composer, got %q", st.composer)
	}
}

// TestEscDisarmedByOtherKey: a key pressed between the two Escapes disarms the
// double-Esc, so the second Esc doesn't clear.
func TestEscDisarmedByOtherKey(t *testing.T) {
	st, _, _, runner := newRecRunnerCapture(t)
	now := time.Now()

	runner.HandleEvent(vaxis.Key{Text: "h", Keycode: 'h'}, now)
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, now)   // arm
	runner.HandleEvent(vaxis.Key{Text: "x", Keycode: 'x'}, now) // disarms + types
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, now)   // arms again, does not clear
	if st.composer != "hx" {
		t.Fatalf("an intervening key must disarm double-Esc; composer = %q, want %q", st.composer, "hx")
	}
}
