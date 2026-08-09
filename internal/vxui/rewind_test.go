package vxui

import (
	"context"
	"testing"

	vaxis "github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui/uitest"

	"github.com/memcode-ai/memcode/internal/checkpoint"
	"github.com/memcode-ai/memcode/internal/theme"
)

// The rewind picker is a two-stage modal: a labeled SELECTOR, then a CONFIRM —
// never a bare "type /rewind 3". This drives the live app: seed rewind points,
// open the picker, and assert the selector renders, Enter advances to a confirm
// with the destructive warning, and Esc backs out to the list (not a cancel).
func TestRewindPickerSelectorAndConfirm(t *testing.T) {
	theme.Set("aurora")
	sess := newTestSession(t)
	var st *appState
	root := &stateCapture{appWidget: appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}, state: &st}
	app := uitest.New(root)
	app.Pump(80, 24)

	st.SetState(func() {
		st.rewindPoints = []checkpoint.Manifest{
			{Seq: 3, Label: "wire OpenAI into the router", Files: []checkpoint.File{{Path: "hybrid.go"}, {Path: "provider.go"}}},
			{Seq: 2, Label: "add the openai provider type", Files: []checkpoint.File{{Path: "openai.go"}}},
		}
		st.rewindSel = 0
		st.rewindConfirm = false
		st.rewindChoosing = true
	})
	app.Pump(80, 24)

	// Selector stage: header + the labeled turns are visible (not a raw seq dump).
	for _, want := range []string{"Rewind — undo agent edits", "wire OpenAI into the router", "2 file(s)", "↑↓ choose"} {
		if !app.Contains(want) {
			t.Fatalf("selector missing %q.\n%q", want, app.Text())
		}
	}

	// Enter advances to CONFIRM — must show the warning, and must NOT have restored.
	app.Enter()
	app.Pump(80, 24)
	for _, want := range []string{"Restore to before this turn?", "discards agent edits", "Enter restore · Esc back"} {
		if !app.Contains(want) {
			t.Fatalf("confirm stage missing %q.\n%q", want, app.Text())
		}
	}
	if !st.rewindChoosing || !st.rewindConfirm {
		t.Fatal("Enter on the list must advance to confirm, not restore or close")
	}

	// Esc from confirm returns to the list (still open, back on the selector).
	app.Send(vaxis.Key{Keycode: vaxis.KeyEsc})
	app.Pump(80, 24)
	if st.rewindConfirm || !st.rewindChoosing {
		t.Fatal("Esc from confirm should return to the selector, not cancel")
	}
	if !app.Contains("↑↓ choose") {
		t.Fatalf("should be back on the selector.\n%q", app.Text())
	}
}
