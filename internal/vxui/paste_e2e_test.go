package vxui

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// captureProvider records every request so a test can assert what the ENGINE actually
// received — not just what the TUI echoed. This is the missing end of the paste
// pipeline: composer → chip → expand → Parse → userBlocks → provider request.
type captureProvider struct {
	mu   sync.Mutex
	reqs []wire.Request
}

func (p *captureProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, r)
	p.mu.Unlock()
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "ACKTOKEN"}}}, nil
}

func (p *captureProvider) requests() []wire.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]wire.Request(nil), p.reqs...)
}

// The lossless-paste regression, end to end: a large (chip-collapsing) iMessage-style
// transcript paste containing a real image path must reach the model as the FULL
// transcript text (never the [pasted …] token, never text with the path stripped)
// PLUS a native image block. This is the exact failure that shipped: attachment
// discovery destroyed the surrounding transcript.
func TestLargeMixedPasteReachesEngineLossless(t *testing.T) {
	theme.Set("aurora")

	root := t.TempDir()
	firstImage := filepath.Join(root, "IMG_0042.png")
	secondImage := filepath.Join(root, "IMG_0043.png")
	writePasteTestPNG(t, firstImage)
	writePasteTestPNG(t, secondImage)

	transcript := strings.Join([]string{
		"Alice: hey, did you see these?",
		"Bob: see what?",
		"Alice: " + firstImage,
		"Bob: wow. where was that taken?",
		"Alice: " + secondImage,
		"Alice: at the lake house last summer",
		"Bob: unreal",
		"why did the message text get stripped last time?",
	}, "\n")

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	prov := &captureProvider{}
	sess := runtime.New(st, llm.NewRunner(prov), root, "fake-model", permissions.ModeAuto, io.Discard)

	w := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(w, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)

	runner.HandleEvent(vaxis.PasteStartEvent{}, now)
	for _, r := range transcript {
		runner.HandleEvent(vaxis.Key{Text: string(r), EventType: vaxis.EventPaste}, now)
	}
	runner.HandleEvent(vaxis.PasteEndEvent{}, now)
	_ = runner.HandleFrame(now)
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)

	// Wait for the MAIN-loop request (the one carrying native image blocks) —
	// not merely the first provider call: on a slow runner the turn-intent
	// judge's call lands first, and stopping there made this flake (its prompt
	// contains the pasted text, so the text asserts passed while images read 0).
	hasImages := func() bool {
		for _, r := range prov.requests() {
			for _, m := range r.Messages {
				for _, b := range m.Blocks {
					if b.Type == "image" {
						return true
					}
				}
			}
		}
		return false
	}
	for i := 0; i < 500 && !hasImages(); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(10 * time.Millisecond)
	}
	reqs := prov.requests()
	if len(reqs) == 0 {
		t.Fatalf("engine never received the pasted turn.\nscrollback=%q", be.recorded())
	}

	// The provider records EVERY call (turn-intent judge included); assert on the
	// main-loop request — the one carrying the native image block — not call order.
	var text string
	var images int
	for _, r := range reqs {
		rText, rImages := "", 0
		for _, m := range r.Messages {
			if m.Role != "user" {
				continue
			}
			for _, b := range m.Blocks {
				switch b.Type {
				case "text":
					rText += b.Text + "\n"
				case "image":
					rImages++
				}
			}
		}
		if rImages > 0 || text == "" {
			text, images = rText, rImages
		}
		if rImages > 0 {
			break
		}
	}
	for _, line := range []string{"Alice: hey, did you see these?", "at the lake house last summer", "why did the message text get stripped last time?"} {
		if !strings.Contains(text, line) {
			t.Errorf("transcript line missing from the model request: %q\ngot text=%q", line, text)
		}
	}
	if strings.Contains(text, "[pasted #") {
		t.Errorf("the [pasted …] token leaked to the model instead of the content:\n%q", text)
	}
	for _, filename := range []string{"IMG_0042.png", "IMG_0043.png"} {
		if !strings.Contains(text, filename) {
			t.Errorf("the attachment path %q was stripped from the canonical text:\n%q", filename, text)
		}
	}
	if images != 2 {
		t.Errorf("native image blocks = %d, want 2", images)
	}
}

func writePasteTestPNG(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.Black)
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
