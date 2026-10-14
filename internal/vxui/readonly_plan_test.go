package vxui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
)

// labelsOf extracts the plain labels from a planOptions() result, for assertion convenience.
func labelsOf(opts []choice) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.label
	}
	return out
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func mustOpts(as *appState) []choice {
	opts, _ := as.planOptions()
	return opts
}

// TestPlanOptionsReadOnly locks the THIRD, orthogonal axis planOptions() composes when the
// session is in Read Only: "Execute" (stays Ask) and "Execute in Auto" instead of a single
// "Execute" row — composed with the existing dirty-tree commit split exactly as that axis
// composes today, so a dirty tree + Read Only legitimately shows all 4 variants.
func TestPlanOptionsReadOnly(t *testing.T) {
	as, sess, _, _ := newRecRunnerCapture(t)
	sess.EnterPlan(context.Background())
	sess.SetReadOnlyChat(true)

	as.planCommitAsk = false
	labels := labelsOf(mustOpts(as))
	for _, want := range []string{"Execute", "Execute in Auto"} {
		if !containsLabel(labels, want) {
			t.Errorf("clean tree + Read Only: missing %q, got %v", want, labels)
		}
	}

	as.planCommitAsk = true
	labels = labelsOf(mustOpts(as))
	for _, want := range []string{
		"Commit first, then execute", "Execute without committing",
		"Commit first, then execute in Auto", "Execute in Auto without committing",
	} {
		if !containsLabel(labels, want) {
			t.Errorf("dirty tree + Read Only: missing %q, got %v", want, labels)
		}
	}

	// Not Read Only: byte-for-byte unchanged — a single "Execute" row (clean tree).
	sess.SetReadOnlyChat(false)
	as.planCommitAsk = false
	labels = labelsOf(mustOpts(as))
	if containsLabel(labels, "Execute in Auto") {
		t.Errorf("outside Read Only there must be no \"Execute in Auto\" row, got %v", labels)
	}
	if !containsLabel(labels, "Execute") {
		t.Errorf("outside Read Only, plain \"Execute\" must still be offered, got %v", labels)
	}
}

// TestPlanExecuteAsLeavesReadOnlyInChosenMode locks planExecuteAs: picking "Execute" keeps
// Ask, picking "Execute in Auto" promotes to Auto — either way chatReadOnly clears and the
// plan actually executes (ExitPlan leaves plan mode).
func TestPlanExecuteAsLeavesReadOnlyInChosenMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode permissions.Mode
	}{
		{"ask", permissions.ModeAsk},
		{"auto", permissions.ModeAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			as, sess, be, runner := newRecRunnerCapture(t)
			now := time.Now()
			sess.EnterPlan(context.Background(), runtime.WithTask("do a thing"))
			as.submit("draft the plan")
			waitFor(t, be, runner, now, func() bool { return sess.PlanPresentable() })
			// The selector only opens once the planning turn has fully returned
			// (app.go raises it after RunTransaction). Approving before that
			// races the turn's own tail, which no user can do.
			waitFor(t, be, runner, now, func() bool { return !as.busy() })
			sess.SetReadOnlyChat(true)

			as.planExecuteAs(tc.mode)

			if sess.ReadOnlyChat() {
				t.Error("planExecuteAs must clear chatReadOnly")
			}
			if got := sess.Mode(); got != tc.mode {
				t.Errorf("Mode() = %v, want %v", got, tc.mode)
			}
			if sess.Planning() {
				t.Error("planExecuteAs must leave plan mode (ExitPlan approved)")
			}
			waitFor(t, be, runner, now, func() bool { return !as.busy() })
		})
	}
}

// TestYoloFromReadOnlyExitsAndAnnouncesAuto drives a full plan→present turn with /yolo
// started from a Read Only session: the auto-execute path must leave Read Only — hardcoded
// Auto, never Ask — announce the transition to scrollback, and actually run the apply phase.
func TestYoloFromReadOnlyExitsAndAnnouncesAuto(t *testing.T) {
	as, sess, be, runner := newRecRunnerCapture(t)
	now := time.Now()
	sess.SetReadOnlyChat(true)
	sess.EnterPlan(context.Background(), runtime.WithYolo(), runtime.WithTask("do a thing"))

	as.submit("draft the plan")

	waitFor(t, be, runner, now, func() bool {
		return strings.Contains(be.recorded(), "exiting Read Only")
	})
	if sess.ReadOnlyChat() {
		t.Error("yolo auto-execute from Read Only must clear chatReadOnly")
	}
	if got := sess.Mode(); got != permissions.ModeAuto {
		t.Errorf("yolo auto-execute must land in Auto, got %v", got)
	}
	waitFor(t, be, runner, now, func() bool { return !as.busy() })
}
