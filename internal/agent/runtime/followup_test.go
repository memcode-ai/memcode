package runtime

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

// TestRunFollowupClassifierNotesSeparate drives a real RunFollowupClassifier goroutine
// end-to-end with a fake provider that judges the follow-up SEPARATE (related=false),
// proving the classifier→scheduler wiring for the new path: the text lands in
// sched.DrainSeparate() rather than vanishing, and it is NOT folded as a steer.
func TestRunFollowupClassifierNotesSeparate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	prov := toolProvider{`{"active_title":"Active work in progress","items":[{"index":0,"related":false,"title":"Unrelated separate task"}]}`}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	sched := NewScheduler(ctx, nil, time.Now)
	kick := make(chan struct{}, 1)
	go s.RunFollowupClassifier(ctx, sched, kick)

	sched.Accept("active work", GateInput{})             // starts the active transaction
	sched.Accept("unrelated separate task", GateInput{}) // queues — the classifier will judge it
	kick <- struct{}{}

	deadline := time.After(10 * time.Second)
	for {
		if _, activeTitle, items := sched.DrainSeparate(); len(items) == 1 {
			if items[0].Text != "unrelated separate task" {
				t.Fatalf("separate text = %q, want the queued follow-up", items[0].Text)
			}
			if items[0].Title != "Unrelated separate task" {
				t.Fatalf("separate title = %q, want the classifier's synthesized title", items[0].Title)
			}
			if activeTitle != "Active work in progress" {
				t.Fatalf("activeTitle = %q, want the classifier's synthesized title for the active task", activeTitle)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the classifier to note the separate follow-up")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// It must NOT have been folded as a steer — it's the SEPARATE path, not the related one.
	if steers := sched.DrainSteers(); len(steers) != 0 {
		t.Fatalf("a separate follow-up must not be folded as a steer, got %#v", steers)
	}
}

// TestRunFollowupClassifierFailureNotesNothing locks the failure semantics that caused
// the verbatim-todo incident: when the classify call yields no usable verdict, the
// pending follow-ups must be left QUEUED — NOT marked separate with empty titles (the
// old empty-map fallthrough) and NOT folded as steers. They simply run as their own
// turns later.
func TestRunFollowupClassifierFailureNotesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	prov := toolProvider{`no verdict here`} // undecodable → errNoVerdict → ok=false
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	sched := NewScheduler(ctx, nil, time.Now)
	kick := make(chan struct{}, 1)
	go s.RunFollowupClassifier(ctx, sched, kick)

	sched.Accept("active work", GateInput{})
	sched.Accept("some follow-up while it runs", GateInput{})
	kick <- struct{}{}

	// Cover the first classify AND the single delayed retry, then assert nothing surfaced.
	time.Sleep(2*followupDebounce + 500*time.Millisecond)
	if _, _, items := sched.DrainSeparate(); len(items) != 0 {
		t.Fatalf("a failed classify must not fabricate separate verdicts, got %#v", items)
	}
	if steers := sched.DrainSteers(); len(steers) != 0 {
		t.Fatalf("a failed classify must not fold steers, got %#v", steers)
	}
	if snap := sched.Snapshot(); len(snap) != 1 || snap[0] != "some follow-up while it runs" {
		t.Fatalf("the follow-up must stay queued to run as its own turn, got %#v", snap)
	}
}

// TestRunFollowupClassifierSkipsWhilePlanning: the background follow-up classifier must
// fully stand down while Planning() is true — that intake is owned by ClassifyPlanMessage/
// DeferWhilePlanning (planfollowup.go) now. Running both would double-classify the same
// message and could produce conflicting/duplicate todo entries. Proven by a provider that
// would ALWAYS mark separate (and thus mutate the todo list) if it were ever called.
func TestRunFollowupClassifierSkipsWhilePlanning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	prov := toolProvider{`{"active_title":"Active work","items":[{"index":0,"related":false,"title":"should never be reached"}]}`}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	enterPlanForTest(s, "")

	sched := NewScheduler(ctx, nil, time.Now)
	kick := make(chan struct{}, 1)
	go s.RunFollowupClassifier(ctx, sched, kick)

	sched.Accept("active work", GateInput{})
	sched.Accept("some message while planning", GateInput{})
	kick <- struct{}{}

	// Give the debounce+classify a real chance to fire if the gate were missing, then assert
	// nothing was noted — the classifier returned early instead of calling the provider.
	time.Sleep(followupDebounce + 200*time.Millisecond)
	if _, _, items := sched.DrainSeparate(); len(items) != 0 {
		t.Fatalf("classifier ran while planning — got separate items %#v, want none", items)
	}
}
