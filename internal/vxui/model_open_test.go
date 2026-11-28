package vxui

import (
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
)

// /model opens its picker straight from the cached list: no network wait, no
// busy state, and so no "Thinking…" line that would pass for a model call.
func TestModelPickerOpensWithoutBusy(t *testing.T) {
	st, _, be, runner := newRecRunnerCapture(t)
	now := time.Now()
	st.openPinPicker("")
	be.drain()
	_ = runner.HandleFrame(now)
	if !st.modelPicking {
		t.Fatal("picker should be open after one frame")
	}
	if st.busy() {
		t.Fatal("opening the picker must not mark the session busy")
	}
	if len(st.modelEntries) == 0 {
		t.Fatal("picker opened with no rows; the embedded catalog should stand in")
	}
}

// Non-model async work names itself and never shows "Thinking…" or effort.
func TestLabeledAsyncStatusIsNotThinking(t *testing.T) {
	st, _, _, _ := newRecRunnerCapture(t)
	st.setBusy(runtime.OwnerAsync)
	st.asyncLabel = "signing in"
	defer st.setBusy(runtime.OwnerNone)
	rt, ok := st.statusRow().(ui.RichText)
	if !ok {
		t.Fatalf("status row is %T", st.statusRow())
	}
	var b strings.Builder
	for _, sp := range rt.Spans {
		b.WriteString(sp.Text)
	}
	if got := b.String(); strings.Contains(got, "Thinking") || !strings.Contains(got, "signing in…") {
		t.Fatalf("status row = %q", got)
	}
	if st.reasoningRow() != nil {
		t.Fatal("a non-model op must not show a reasoning line")
	}
}
