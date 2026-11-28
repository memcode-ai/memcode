package taskrun

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/runtimes"
	"github.com/memcode-ai/memcode/internal/statedir"
	"github.com/memcode-ai/memcode/internal/task"
)

// deadPID is a pid high enough that it cannot belong to a live process here.
const deadPID = 4194303

var clock = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

// repo makes a throwaway git repository with one commit. A mutating task runs
// in an isolated worktree, which needs a repo with a HEAD — so a plain temp dir
// is now correctly refused, and tests must reflect what real use looks like.
func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-qm", "initial")
	return dir
}

func store(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sample(t *testing.T, body string) task.Task {
	t.Helper()
	if body == "" {
		body = "version: 1\nname: upgrade-models\ninstructions: refresh the catalog\n"
	}
	tk, err := task.Parse([]byte(body), "test.yaml", task.ScopeProject, clock)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// runner wires a fake executor so the whole lifecycle is exercised without a
// model call.
// runner wires a fake executor and a FIXED runtime environment, so a test's
// answer never depends on what happens to be installed on the machine running
// it. Hosted-only by default: the point of most tests is not which backend.
func runner(t *testing.T, s *Store, spawn SpawnFunc) *Runner {
	t.Helper()
	return &Runner{
		Store: s, Spawn: spawn, HeartbeatEvery: 10 * time.Millisecond,
		Auth:      func() runtimes.Authorizations { return nil },
		Available: func() []string { return nil },
	}
}

func ok(text string) SpawnFunc {
	return func(context.Context, SpawnRequest) (SpawnResult, error) {
		return SpawnResult{Text: text, LogPath: "/tmp/log"}, nil
	}
}

// changing is an executor that actually edits the worktree. Needed for a
// success outcome, because "did this run change anything" is read from the
// REPOSITORY and not from what the agent claims — an agent that says it updated
// four model ids and touched nothing is a no_change run, deliberately.
func changing(text string) SpawnFunc {
	return func(_ context.Context, req SpawnRequest) (SpawnResult, error) {
		if err := os.WriteFile(filepath.Join(req.WorkDir, "changed.txt"), []byte("work\n"), 0o644); err != nil {
			return SpawnResult{}, err
		}
		return SpawnResult{Text: text, LogPath: "/tmp/log"}, nil
	}
}

// INVARIANT 1: a run snapshots its execution inputs at creation. Editing the
// task file afterwards must not change what that run meant.
func TestRunFreezesItsInputs(t *testing.T) {
	root := t.TempDir()
	before := sample(t, "version: 1\nname: upgrade-models\ninstructions: original instructions\n")
	run, err := Freeze(before, root, TriggerManual, "", clock)
	if err != nil {
		t.Fatal(err)
	}

	after := sample(t, "version: 1\nname: upgrade-models\ninstructions: completely different\n"+
		"autonomy:\n  level: read_only\n")
	afterRev, _ := after.Revision()

	if run.Revision == afterRev {
		t.Fatal("precondition: the edit should change the revision")
	}
	if !strings.Contains(run.Definition, "original instructions") {
		t.Error("the frozen definition must hold the text the run was created from")
	}
	if strings.Contains(run.Definition, "completely different") {
		t.Error("a later edit must not appear in an existing run")
	}
	// Authority is resolved at freeze time too, so a later downgrade cannot
	// retroactively claim the run had less power than it did.
	if !contains(run.Grants, "git.push_branch") {
		t.Errorf("grants = %v, want the branch tier expanded at freeze time", run.Grants)
	}
	if run.Project == "" {
		t.Error("the project must be resolved to an absolute path at freeze time")
	}
	if run.Timeout != 45*time.Minute {
		t.Errorf("timeout = %v, want the resolved limit", run.Timeout)
	}
}

// INVARIANT 2: one occurrence, one run. At-least-once dispatch must not become
// duplicate side effects.
func TestTriggeredOccurrenceIsIdempotent(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	tk := sample(t, "")

	first, err := Freeze(tk, t.TempDir(), "cron", "cron:0 10 * * MON@2026-09-14T10:00:00Z", clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Same occurrence dispatched again — a restart, a retry, a racing process.
	second, _ := Freeze(tk, t.TempDir(), "cron", "cron:0 10 * * MON@2026-09-14T10:00:00Z", clock.Add(time.Second))
	_, err = s.Create(ctx, second)
	if !errors.Is(err, ErrOccupied) {
		t.Fatalf("a repeated occurrence must be refused, got %v", err)
	}

	runs, _ := s.Recent(ctx, "upgrade-models", 10)
	if len(runs) != 1 {
		t.Errorf("got %d runs for one occurrence, want 1", len(runs))
	}
}

// Running a task by hand twice is a deliberate act, not a duplicate.
func TestManualRunsAreNeverDeduped(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	tk := sample(t, "")
	for i := 0; i < 3; i++ {
		r, err := Freeze(tk, t.TempDir(), TriggerManual, "", clock.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(ctx, r); err != nil {
			t.Fatalf("manual run %d refused: %v", i, err)
		}
	}
	runs, _ := s.Recent(ctx, "upgrade-models", 10)
	if len(runs) != 3 {
		t.Errorf("got %d manual runs, want 3", len(runs))
	}
}

// Two processes racing to execute the same row: exactly one may claim it.
func TestClaimIsExclusive(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	r, _ := Freeze(sample(t, ""), t.TempDir(), TriggerManual, "", clock)
	r, _ = s.Create(ctx, r)

	first, err := s.Claim(ctx, r.ID, clock)
	if err != nil || !first {
		t.Fatalf("first claim = %v, %v; want true", first, err)
	}
	second, err := s.Claim(ctx, r.ID, clock)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("a second process must not be able to claim a running run")
	}
}

// The whole loop: definition -> run -> execution -> persisted outcome -> inbox.
func TestEndToEndLoop(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	tk := sample(t, "")
	r := runner(t, s, changing("Updated 4 model ids. Tests pass."))

	run, err := r.Run(ctx, tk, repo(t), TriggerManual, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.State != StateDone {
		t.Errorf("state = %q, want done", run.State)
	}
	if run.Outcome != OutcomeSuccess {
		t.Errorf("outcome = %q, want success", run.Outcome)
	}
	if run.Summary == "" {
		t.Error("a finished run needs a summary")
	}
	if run.FinishedAt.IsZero() {
		t.Error("a finished run needs a finish time")
	}
	if run.Revision == "" || run.Definition == "" {
		t.Error("the run must carry its frozen definition")
	}

	// It lands in the inbox as unseen — the durable ledger, not a notification.
	unseen, err := s.Unseen(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unseen) != 1 || unseen[0].ID != run.ID {
		t.Errorf("unseen = %+v, want the finished run", unseen)
	}
}

func TestOutcomeClassification(t *testing.T) {
	cases := []struct {
		name string
		fn   SpawnFunc
		want Outcome
	}{
		{"success", changing("Updated 4 model ids."), OutcomeSuccess},
		// The agent CLAIMS work but the repository is untouched: no_change wins,
		// because the outcome is a fact and not a narration.
		{"claimed but unchanged", ok("Updated 4 model ids."), OutcomeNoChange},
		{"no change", ok("Checked every provider. No changes needed."), OutcomeNoChange},
		{"needs attention", changing("Anthropic changed a contract; this needs your decision."), OutcomeNeedsAttention},
		// A run refused a capability did not do its job, however calm the prose.
		{"denied capability", changing("I tried to write the file but the tool call was denied."), OutcomeNeedsAttention},
		{"executor error", func(context.Context, SpawnRequest) (SpawnResult, error) {
			return SpawnResult{}, errors.New("spawn failed")
		}, OutcomeFailed},
		{"nonzero exit", func(context.Context, SpawnRequest) (SpawnResult, error) {
			return SpawnResult{ExitCode: 2, Text: "boom"}, nil
		}, OutcomeFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := store(t)
			run, err := runner(t, s, c.fn).Run(context.Background(), sample(t, ""), repo(t), TriggerManual, "")
			if err != nil {
				t.Fatal(err)
			}
			if run.Outcome != c.want {
				t.Errorf("outcome = %q, want %q (summary %q)", run.Outcome, c.want, run.Summary)
			}
		})
	}
}

// A run that exceeds its timeout fails rather than hanging the ledger.
func TestTimeoutFailsTheRun(t *testing.T) {
	s := store(t)
	tk := sample(t, "version: 1\nname: slow\ninstructions: takes forever\nlimits:\n  timeout: 50ms\n")
	r := runner(t, s, func(ctx context.Context, _ SpawnRequest) (SpawnResult, error) {
		<-ctx.Done()
		return SpawnResult{}, ctx.Err()
	})
	run, err := r.Run(context.Background(), tk, repo(t), TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != OutcomeFailed || !strings.Contains(run.Summary, "timed out") {
		t.Errorf("outcome = %q / %q, want a timeout failure", run.Outcome, run.Summary)
	}
}

// INVARIANT 3: gateway/process restart semantics are explicit, not accidental.
// A run whose process died is FAILED as interrupted — never silently resumed,
// never silently re-run.
func TestRestartInterruptsOrphanedRun(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	r, _ := Freeze(sample(t, ""), t.TempDir(), TriggerManual, "", clock)
	r, _ = s.Create(ctx, r)
	if claimed, _ := s.Claim(ctx, r.ID, clock); !claimed {
		t.Fatal("claim failed")
	}

	// Simulate the owning process dying: our host, a pid that cannot be alive.
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET pid=? WHERE id=?`, deadPID, r.ID); err != nil {
		t.Fatal(err)
	}

	n, err := s.Reconcile(ctx, clock)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d runs, want 1", n)
	}
	got, _ := s.Get(ctx, r.ID)
	if got.State != StateDone || got.Outcome != OutcomeInterrupted {
		t.Errorf("state/outcome = %q/%q, want done/interrupted", got.State, got.Outcome)
	}
	if !strings.Contains(got.Detail, "Not resumed and not re-run automatically") {
		t.Errorf("an interrupted run must say what it did NOT do, got %q", got.Detail)
	}

	// And it must stay settled: a second reconcile changes nothing, and the run
	// is never quietly resurrected into running.
	if n2, _ := s.Reconcile(ctx, clock); n2 != 0 {
		t.Errorf("second reconcile touched %d runs, want 0", n2)
	}
}

// A run owned by another host is judged only on heartbeat staleness — we cannot
// ask about a pid we cannot see.
func TestRestartWaitsOutHeartbeatForRemoteHost(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	r, _ := Freeze(sample(t, ""), t.TempDir(), TriggerManual, "", clock)
	r, _ = s.Create(ctx, r)
	s.Claim(ctx, r.ID, clock)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runs SET host='another-machine', pid=1, heartbeat_at=? WHERE id=?`,
		ts(clock), r.ID); err != nil {
		t.Fatal(err)
	}

	// Fresh heartbeat: leave it alone, it may genuinely still be working.
	if n, _ := s.Reconcile(ctx, clock.Add(30*time.Second)); n != 0 {
		t.Errorf("a live remote run must not be interrupted, touched %d", n)
	}
	// Stale heartbeat: now it is gone.
	if n, _ := s.Reconcile(ctx, clock.Add(StaleAfter+time.Minute)); n != 1 {
		t.Error("a stale remote run must be interrupted")
	}
}

// An interrupted run cannot be re-executed by accident; the claim refuses it.
func TestInterruptedRunCannotBeReExecuted(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	tk := sample(t, "")
	r, _ := Freeze(tk, t.TempDir(), TriggerManual, "", clock)
	r, _ = s.Create(ctx, r)
	s.Claim(ctx, r.ID, clock)
	s.Finish(ctx, r.ID, OutcomeInterrupted, "died", "", "", clock)

	var executed bool
	rn := runner(t, s, func(context.Context, SpawnRequest) (SpawnResult, error) {
		executed = true
		return SpawnResult{Text: "ran again"}, nil
	})
	got, err := rn.Execute(ctx, r, tk)
	if err != nil {
		t.Fatal(err)
	}
	if executed {
		t.Error("a settled run must never be executed a second time")
	}
	if got.Outcome != OutcomeInterrupted {
		t.Errorf("outcome = %q, want the original interrupted verdict preserved", got.Outcome)
	}
}

// Finish is terminal: a late finisher cannot overwrite a settled verdict.
func TestFinishIsTerminal(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	r, _ := Freeze(sample(t, ""), t.TempDir(), TriggerManual, "", clock)
	r, _ = s.Create(ctx, r)
	s.Claim(ctx, r.ID, clock)
	s.Finish(ctx, r.ID, OutcomeInterrupted, "died", "", "", clock)
	s.Finish(ctx, r.ID, OutcomeSuccess, "actually fine", "", "", clock)

	got, _ := s.Get(ctx, r.ID)
	if got.Outcome != OutcomeInterrupted {
		t.Errorf("outcome = %q, want the first verdict to stand", got.Outcome)
	}
}

// Seen is three states, not a boolean: being shown a banner is not the same as
// having dealt with something.
func TestSeenStateMachine(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	r := runner(t, s, ok("done"))
	run, _ := r.Run(ctx, sample(t, ""), repo(t), TriggerManual, "")

	if run.Seen != SeenUnseen {
		t.Fatalf("seen = %q, want unseen", run.Seen)
	}
	if err := s.MarkSeen(ctx, []string{run.ID}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, run.ID)
	if got.Seen != SeenSeen {
		t.Errorf("seen = %q, want seen", got.Seen)
	}
	if left, _ := s.Unseen(ctx, 10); len(left) != 0 {
		t.Error("a seen run must leave the unseen list")
	}

	if err := s.Acknowledge(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	// Showing a banner again must not drag an acknowledged run backwards.
	if err := s.MarkSeen(ctx, []string{run.ID}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, run.ID)
	if got.Seen != SeenAcknowledged {
		t.Errorf("seen = %q, want acknowledged to stick", got.Seen)
	}
}

// Authority decides how the execution is permitted, and a read-only task must
// not even be handed the editing tools.
func TestAuthorityShapesExecution(t *testing.T) {
	ro := sample(t, "version: 1\nname: audit\ninstructions: look only\nautonomy:\n  level: read_only\n")
	// Authority is a CAPABILITY restriction, not a stricter permission mode:
	// ModeAsk does not hold, because the authorization judge downgrades a prompt
	// to an allow when the request plainly asked for the action, and a task's
	// instructions always do.
	if got := modeFor(ro); got != permissions.ModeAuto {
		t.Errorf("mode = %q, want auto for every tier", got)
	}
	// Authority is a capability PROJECTION now: the tier decides which tools and
	// command classes the child receives, and a read-only tier keeps its shell
	// (a task that cannot build or test is not useful) while losing the tools
	// and commands that could change anything that outlives the run.
	cap, err := ro.Capability()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"edit_file", "apply_patch", "github"} {
		if !contains(cap.DenyTools, want) {
			t.Errorf("read_only must deny the %s tool, got %v", want, cap.DenyTools)
		}
	}
	if contains(cap.DenyTools, "shell") {
		t.Error("read_only must KEEP its shell — builds, tests and queries are the point")
	}
	for _, want := range []string{"git push", "git commit", "gh"} {
		if !contains(cap.DenyCommands, want) {
			t.Errorf("read_only must deny %q, got %v", want, cap.DenyCommands)
		}
	}
	if !cap.ProtectProject {
		t.Error("read_only must not execute in the user's checkout")
	}
	if cap.Mutating {
		t.Error("read_only is not a mutating tier")
	}

	br := sample(t, "")
	if got := modeFor(br); got != permissions.ModeAuto {
		t.Errorf("branch mode = %q, want auto", got)
	}
	bcap, err := br.Capability()
	if err != nil {
		t.Fatal(err)
	}
	if len(bcap.DenyTools) != 0 {
		t.Errorf("the branch tier keeps its tools, got denied %v", bcap.DenyTools)
	}
	if contains(bcap.DenyCommands, "git push") {
		t.Error("the branch tier may push a new branch")
	}
	// But never the operations that make work authoritative, at any tier.
	for _, want := range []string{"git push --force", "git merge", "gh pr merge"} {
		if !contains(bcap.DenyCommands, want) {
			t.Errorf("%q must be denied at every tier, got %v", want, bcap.DenyCommands)
		}
	}
	if !bcap.Mutating {
		t.Error("the branch tier is a mutating tier")
	}
}

// The request handed to the executor comes from the FROZEN run, so it carries
// the authority decided at creation.
func TestSpawnRequestCarriesFrozenInputs(t *testing.T) {
	s := store(t)
	var seen SpawnRequest
	rn := runner(t, s, func(_ context.Context, req SpawnRequest) (SpawnResult, error) {
		seen = req
		return SpawnResult{Text: "ok"}, nil
	})
	tk := sample(t, "version: 1\nname: audit\ninstructions: look only\nautonomy:\n  level: read_only\n")
	run, err := rn.Run(context.Background(), tk, repo(t), TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if seen.RunID != run.ID {
		t.Errorf("RunID = %q, want %q", seen.RunID, run.ID)
	}
	if !contains(seen.DenyTools, "edit_file") {
		t.Errorf("the executor must receive the capability ceiling, got %v", seen.DenyTools)
	}
	if !contains(seen.DenyCommands, "git push") {
		t.Errorf("the executor must receive the command ceiling, got %v", seen.DenyCommands)
	}
	// The run WORKS in an isolated worktree, never the user's checkout — kept with
	// the project's memcode state, so nothing is created inside the project.
	if seen.WorkDir == run.Project {
		t.Error("the run must not execute in the project's own checkout")
	}
	if !strings.HasPrefix(seen.WorkDir, filepath.Join(statedir.For(run.Project), "worktrees")) {
		t.Errorf("working dir = %q, want a worktree in the project's state dir", seen.WorkDir)
	}
	if seen.Project != run.Project {
		t.Errorf("bookkeeping root = %q, want the project %q", seen.Project, run.Project)
	}
}

func TestDisabledTaskRefused(t *testing.T) {
	s := store(t)
	tk := sample(t, "version: 1\nname: off\ninstructions: x\nenabled: false\n")
	_, err := runner(t, s, ok("x")).Run(context.Background(), tk, repo(t), TriggerManual, "")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("err = %v, want a disabled-task refusal", err)
	}
}

func TestRecentAndActive(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	r := runner(t, s, ok("done"))
	root := repo(t)
	for i := 0; i < 2; i++ {
		if _, err := r.Run(ctx, sample(t, ""), root, TriggerManual, ""); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := s.Recent(ctx, "upgrade-models", 10)
	if len(runs) != 2 {
		t.Errorf("recent = %d, want 2", len(runs))
	}
	active, _ := s.Active(ctx, "upgrade-models")
	if len(active) != 0 {
		t.Errorf("active = %d, want 0 once finished", len(active))
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// A ledger created before a column existed must migrate, not explode. Every
// other test here uses a fresh database, which is exactly why this one builds
// the OLD shape by hand: the first version of the milestone-3 schema passed the
// whole suite and then failed on the real ~/.config/memcode/tasks.db, because
// CREATE TABLE IF NOT EXISTS silently skips an existing table.
func TestOpenMigratesAnOlderLedger(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")

	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The milestone-2 shape: no backlog, no occurred_at.
	if _, err := old.ExecContext(ctx, `
		CREATE TABLE runs (
		  id TEXT PRIMARY KEY, task TEXT NOT NULL, revision TEXT NOT NULL,
		  definition TEXT NOT NULL, trigger_kind TEXT NOT NULL,
		  trigger_id TEXT NOT NULL DEFAULT '', project TEXT NOT NULL,
		  grants TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
		  model TEXT NOT NULL DEFAULT '', timeout_ns INTEGER NOT NULL DEFAULT 0,
		  max_cost_usd REAL NOT NULL DEFAULT 0, state TEXT NOT NULL,
		  outcome TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
		  detail TEXT NOT NULL DEFAULT '', log_path TEXT NOT NULL DEFAULT '',
		  host TEXT NOT NULL DEFAULT '', pid INTEGER NOT NULL DEFAULT 0,
		  started_at TEXT NOT NULL, heartbeat_at TEXT NOT NULL DEFAULT '',
		  finished_at TEXT NOT NULL DEFAULT '', seen TEXT NOT NULL DEFAULT 'unseen'
		);
		INSERT INTO runs (id, task, revision, definition, trigger_kind, project, state, started_at)
		VALUES ('run_old', 'legacy', 'sha256:x', 'version: 1', 'manual', '/repo', 'done', '2026-09-10T00:00:00Z');`,
	); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("opening an older ledger must migrate it, got %v", err)
	}
	defer s.Close()

	// The old row survives and reads back through the new columns.
	got, err := s.Get(ctx, "run_old")
	if err != nil {
		t.Fatalf("reading a pre-migration row: %v", err)
	}
	if got.Task != "legacy" || got.Backlog != 0 || !got.OccurredAt.IsZero() {
		t.Errorf("migrated row = %+v, want the old data with zeroed new columns", got)
	}
	// And the new columns are writable.
	r, err := s.Create(ctx, Run{ID: "run_new", Task: "t", Revision: "sha256:y",
		Definition: "version: 1", TriggerKind: "cron", TriggerID: "cron:x@2026",
		Project: "/repo", StartedAt: clock, OccurredAt: clock, Backlog: 3})
	if err != nil {
		t.Fatal(err)
	}
	back, _ := s.Get(ctx, r.ID)
	if back.Backlog != 3 {
		t.Errorf("backlog = %d, want 3 after migration", back.Backlog)
	}
	// Migration is idempotent.
	s.Close()
	if s2, err := Open(ctx, path); err != nil {
		t.Errorf("re-opening a migrated ledger: %v", err)
	} else {
		s2.Close()
	}
}
