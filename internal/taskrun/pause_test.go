package taskrun

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/task"
)

// The escalation line is a DECLARED protocol, not error-string matching. It has
// to survive the ways an agent actually writes: extra prose around it, a lower
// case verdict, punctuation after the word, and a change of mind.
func TestParseEscalation(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want Escalation
		why  string
	}{
		{"absent", "Updated three modules. All tests pass.", EscalateNone, ""},
		{"continue", "Done.\nESCALATION: continue", EscalateNone, ""},
		{"transient", "Registry timed out.\nESCALATION: retry_later", EscalateRetry, ""},
		{"pause with reason", "ESCALATION: pause_task foo/v3 needs one of two incompatible migrations",
			EscalatePause, "foo/v3 needs one of two incompatible migrations"},
		{"punctuated", "escalation: pause_task. the check cannot be evaluated any more",
			EscalatePause, "the check cannot be evaluated any more"},
		{"embedded in prose", "I'll stop here.\n\nESCALATION: needs_attention someone should look",
			EscalateAttention, "someone should look"},
		// An agent that reconsiders should be taken at its final word.
		{"last wins", "ESCALATION: pause_task early guess\nActually I fixed it.\nESCALATION: continue",
			EscalateNone, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := ParseEscalation(c.out)
			if got != c.want {
				t.Errorf("escalation = %q, want %q", got, c.want)
			}
			if why != c.why {
				t.Errorf("reason = %q, want %q", why, c.why)
			}
		})
	}
}

// A pause must keep the ORIGINAL diagnosis. Re-pausing with a later, vaguer
// reason would overwrite the one piece of evidence the user needs to resolve it.
func TestPauseKeepsTheFirstDiagnosis(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	now := time.Now()

	first := Pause{Task: "deps", Project: "/repo", RunID: "run1",
		Reason: "foo/v3 needs one of two incompatible migrations", Since: now}
	if err := s.Pause(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(ctx, Pause{Task: "deps", Project: "/repo", RunID: "run2",
		Reason: "failed again", Since: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.PausedTask(ctx, "deps", "/repo")
	if err != nil || !ok {
		t.Fatalf("expected a pause, got ok=%v err=%v", ok, err)
	}
	if got.Reason != first.Reason {
		t.Errorf("reason = %q, want the original diagnosis %q", got.Reason, first.Reason)
	}
	if got.RunID != "run1" {
		t.Errorf("run = %q, want the run that made the call", got.RunID)
	}
}

func TestResumeLiftsExactlyOnce(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	if err := s.Pause(ctx, Pause{Task: "deps", Project: "/repo", Reason: "x", Since: time.Now()}); err != nil {
		t.Fatal(err)
	}
	lifted, err := s.Resume(ctx, "deps", "/repo")
	if err != nil || !lifted {
		t.Fatalf("first resume should lift it: %v %v", lifted, err)
	}
	if lifted, _ := s.Resume(ctx, "deps", "/repo"); lifted {
		t.Error("resuming a task that is not paused must report that, not succeed silently")
	}
	if _, ok, _ := s.PausedTask(ctx, "deps", "/repo"); ok {
		t.Error("still paused after resume")
	}
}

// Two projects, two tasks: a pause is scoped, and one suspended automation must
// not stop an unrelated one.
func TestPausesAreScopedToTaskAndProject(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	now := time.Now()
	for _, p := range []Pause{
		{Task: "deps", Project: "/a", Reason: "one", Since: now.Add(-time.Hour)},
		{Task: "deps", Project: "/b", Reason: "two", Since: now},
	} {
		if err := s.Pause(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := s.PausedTask(ctx, "deps", "/c"); ok {
		t.Error("a pause in /a and /b must not suspend /c")
	}
	all, err := s.Paused(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("Paused() = %d entries, %v", len(all), err)
	}
	// Oldest first: the responsibility abandoned longest ago is raised first.
	if all[0].Project != "/a" {
		t.Errorf("expected the oldest pause first, got %s", all[0].Project)
	}
}

// The whole point of pausing: a suspended task does not fire. Running into the
// same wall every week is worse than stopping, because it buries a real
// decision under identical failures nobody reads.
func TestPausedTasksDoNotFire(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	root := repo(t)
	tk := triggered(t, fmt.Sprintf(daily, "run_once"))
	now := at(t, "2026-09-11T09:30:00Z")

	var spawns int
	var mu sync.Mutex
	r := runner(t, s, func(context.Context, SpawnRequest) (SpawnResult, error) {
		mu.Lock()
		spawns++
		mu.Unlock()
		return SpawnResult{Text: "done"}, nil
	})
	if err := s.SetWatermark(ctx, tk.Name, TriggerKey(tk.Triggers[0]), at(t, "2026-09-08T02:00:00Z"), now); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(ctx, Pause{Task: tk.Name, Project: root,
		Reason: "two incompatible migrations; needs a decision", Since: now}); err != nil {
		t.Fatal(err)
	}

	res := r.Poll(ctx, []task.Task{tk}, root, now)
	if len(res.Errs) != 0 {
		t.Fatalf("a paused task is not an error condition: %v", res.Errs)
	}
	if len(res.Started) != 0 {
		t.Errorf("a paused task must not fire, started %d run(s)", len(res.Started))
	}
	if spawns != 0 {
		t.Errorf("no agent should have been spawned, got %d", spawns)
	}

	// And it fires again once the person has dealt with it.
	if _, err := s.Resume(ctx, tk.Name, root); err != nil {
		t.Fatal(err)
	}
	if res := r.Poll(ctx, []task.Task{tk}, root, now); len(res.Started) != 1 {
		t.Errorf("after resume the task should be eligible again, started %d", len(res.Started))
	}
}
