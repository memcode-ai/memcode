package runtime

import (
	"strings"
	"testing"
)

// TestBuildSteerNoteSingular locks the existing wording for the common case: exactly one
// mid-turn steer. No todo instruction here — one instruction doesn't need tracking.
func TestBuildSteerNoteSingular(t *testing.T) {
	note := buildSteerNote([]string{"actually use TLS 1.3 only"})
	if want := "actually use TLS 1.3 only"; !strings.Contains(note, want) {
		t.Errorf("steer text missing: %q", note)
	}
	if strings.Contains(note, "todo add") {
		t.Errorf("a lone steer shouldn't ask for todo tracking: %q", note)
	}
	if !strings.Contains(note, "not a new task") {
		t.Errorf("singular framing missing: %q", note)
	}
}

// TestBuildSteerNotePlural: several steers can land in ONE drain (an explicit +message
// alongside a batch the classifier judged related in the same pass). Steers are the RELATED
// case by construction — the note must go plural (name the count, not a singular "it"
// describing a joined list) but must NOT ask for per-item todo tracking; that's reserved for
// genuinely separate/disparate requests, a different path entirely.
func TestBuildSteerNotePlural(t *testing.T) {
	steers := []string{"also handle the empty-file case", "and add a regression test for it"}
	note := buildSteerNote(steers)
	for _, want := range steers {
		if !strings.Contains(note, want) {
			t.Errorf("steer text missing: %q in %q", want, note)
		}
	}
	if strings.Contains(note, "todo") {
		t.Errorf("steers are related — they must NOT ask for todo tracking: %q", note)
	}
	if !strings.Contains(note, "2 notes") {
		t.Errorf("plural note should state the count: %q", note)
	}
}

// TestBuildSeparateNote: the FYI note for genuinely SEPARATE follow-ups (the classifier's
// "not related" verdict) is a distinct tone from buildSteerNote — no "fold this in"
// language, and it must mention the todo tracking (the model has no other way to learn a
// mutation it didn't itself make).
func TestBuildSeparateNote(t *testing.T) {
	note := buildSeparateNote([]string{"add a dashboard page"})
	if !strings.Contains(note, "add a dashboard page") {
		t.Errorf("separate text missing: %q", note)
	}
	if strings.Contains(note, "fold") {
		t.Errorf("a separate note must NOT ask to fold it in: %q", note)
	}
	if !strings.Contains(note, "todo list") {
		t.Errorf("separate note should mention the todo tracking: %q", note)
	}
	if !strings.Contains(note, "NOT a") && !strings.Contains(note, "NOT refinements") {
		t.Errorf("separate note should clearly distinguish itself from a refinement: %q", note)
	}
}

// TestBuildSeparateNotePlural mirrors the plural steer case: several separate follow-ups
// batched in one classify pass state the count, not a singular "it".
func TestBuildSeparateNotePlural(t *testing.T) {
	texts := []string{"add a dashboard page", "fix the readme typo"}
	note := buildSeparateNote(texts)
	for _, want := range texts {
		if !strings.Contains(note, want) {
			t.Errorf("separate text missing: %q in %q", want, note)
		}
	}
	if !strings.Contains(note, "2 separate thing(s)") {
		t.Errorf("plural note should state the count: %q", note)
	}
}
