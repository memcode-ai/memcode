package vxui

// Tests for the /sync multi-select lanes picker — the one modal that is
// checkboxes rather than a single-select radio: Space toggles the highlighted
// row, `a` toggles all, Enter commits, Esc cancels. Follows the other modal
// tests' pattern: seed the picker state directly (stateCapture), then drive
// keys through the runner.

import (
	"strings"
	"testing"
	"time"

	vaxis "github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/config"
)

// openSyncPicker seeds the picker open with no rows toggled, mirroring openSync
// with an empty stored selection (the async-free path tests can drive).
func openSyncPicker(t *testing.T) (*appState, *recBackend, *ui.Runner) {
	t.Helper()
	st, _, be, runner := newRecRunnerCapture(t)
	now := time.Now()
	st.syncDetected = nil
	st.syncToggles = make([]bool, len(config.SyncTargetAll))
	st.syncSel = 0
	st.syncChoosing = true
	_ = runner.HandleFrame(now)
	return st, be, runner
}

func TestSyncPickerSpaceTogglesRow(t *testing.T) {
	st, _, runner := openSyncPicker(t)
	now := time.Now()

	runner.HandleEvent(vaxis.Key{Text: " ", Keycode: ' '}, now)
	if !st.syncToggles[0] {
		t.Fatalf("Space must toggle the highlighted row on: %+v", st.syncToggles)
	}
	runner.HandleEvent(vaxis.Key{Text: " ", Keycode: ' '}, now)
	if st.syncToggles[0] {
		t.Fatalf("Space must toggle the highlighted row back off: %+v", st.syncToggles)
	}

	// Down moves the cursor; Space then toggles THAT row, not row 0.
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyDown}, now)
	if st.syncSel != 1 {
		t.Fatalf("Down: syncSel = %d, want 1", st.syncSel)
	}
	runner.HandleEvent(vaxis.Key{Text: " ", Keycode: ' '}, now)
	if st.syncToggles[0] || !st.syncToggles[1] {
		t.Fatalf("Space after Down toggled the wrong row: %+v", st.syncToggles)
	}
}

func TestSyncPickerToggleAll(t *testing.T) {
	st, _, runner := openSyncPicker(t)
	now := time.Now()

	runner.HandleEvent(vaxis.Key{Text: "a", Keycode: 'a'}, now)
	for i, on := range st.syncToggles {
		if !on {
			t.Fatalf("`a` must toggle every row on; row %d is off: %+v", i, st.syncToggles)
		}
	}
	runner.HandleEvent(vaxis.Key{Text: "a", Keycode: 'a'}, now)
	for i, on := range st.syncToggles {
		if on {
			t.Fatalf("`a` with all on must toggle every row off; row %d is on: %+v", i, st.syncToggles)
		}
	}
}

func TestSyncPickerEscCancels(t *testing.T) {
	st, be, runner := openSyncPicker(t)
	now := time.Now()

	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, now)
	_ = runner.HandleFrame(now)
	if st.syncChoosing {
		t.Fatalf("Esc must close the picker")
	}
	if rec := be.recorded(); !strings.Contains(rec, "sync cancelled") {
		t.Fatalf("Esc must announce the cancel.\nrecorded=%q", rec)
	}
}

func TestSyncPickerEnterWithNothingSelected(t *testing.T) {
	st, be, runner := openSyncPicker(t)
	now := time.Now()

	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)
	_ = runner.HandleFrame(now)
	if st.syncChoosing {
		t.Fatalf("Enter must close the picker")
	}
	if st.busy() {
		t.Fatalf("Enter with no targets must not start a sync")
	}
	if rec := be.recorded(); !strings.Contains(rec, "no targets selected") {
		t.Fatalf("Enter with no targets must say so.\nrecorded=%q", rec)
	}
}
