package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/gateway/occurrence"
	"github.com/memcode-ai/memcode/internal/task"
	"github.com/memcode-ai/memcode/internal/taskrun"
)

// openRuns opens the run ledger and settles anything a dead process left
// behind. Reconciling on every open means an interrupted run is discovered the
// next time anyone looks, rather than sitting as "running" forever.
func openRuns(ctx context.Context) (*taskrun.Store, error) {
	s, err := taskrun.OpenDefault(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.Reconcile(ctx, time.Now()); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// outcomeMark is the one-glyph verdict used in listings.
func outcomeMark(o taskrun.Outcome) string {
	switch o {
	case taskrun.OutcomeSuccess, taskrun.OutcomeNoChange:
		return "OK"
	case taskrun.OutcomeNeedsAttention:
		return "!!"
	case taskrun.OutcomeInterrupted:
		return ".."
	default:
		return "XX"
	}
}

var taskCmd = &cobra.Command{
	Use:     "task",
	Aliases: []string{"tasks"},
	Short:   "Inspect and run autonomous tasks",
	Long: `A task is a bounded unit of work memcode can run without you watching — refresh a model
catalog, review security advisories, upgrade dependencies — declared in YAML and runnable by
name.

A task is not the same thing as a schedule. It is runnable on demand with ` + "`memcode task run`" + `,
and a trigger is something you attach when you want it to happen on its own.

Definitions live in two places, with the project copy winning a name collision:

  <repo>/.memcode/tasks/<name>.yaml    travels with the repo, reviewable in a PR
  ~/.config/memcode/tasks/<name>.yaml  this machine only`,
	RunE: func(cmd *cobra.Command, args []string) error { return taskListCmd.RunE(cmd, args) },
}

// taskRoot resolves the project root for task lookup. A task can name its own
// project, so this is only the starting point, not necessarily where it runs.
func taskRoot() string {
	root, _, err := config.Resolve(".")
	if err != nil {
		return ""
	}
	return root
}

// reportLoadErrors prints malformed definitions to stderr. They go to stderr,
// and never abort the command, because one bad file must not hide the tasks
// that are fine — the same reason the loader keeps them separate.
func reportLoadErrors(errs []error) {
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "  ! %v\n", err)
	}
}

// cadence renders a task's triggers for a listing.
func cadence(t task.Task) string {
	if len(t.Triggers) == 0 {
		return "manual"
	}
	parts := make([]string, 0, len(t.Triggers))
	for _, tr := range t.Triggers {
		switch {
		case tr.Manual:
			parts = append(parts, "manual")
		case tr.Cron != "":
			s := tr.Cron
			if tr.TZ != "" {
				s += " " + tr.TZ
			}
			parts = append(parts, s)
		case tr.Every != "":
			parts = append(parts, "every "+tr.Every)
		case tr.At != "":
			parts = append(parts, "at "+tr.At)
		}
	}
	return strings.Join(parts, ", ")
}

// taskVisibility reports whether .memcode/tasks is actually reachable by git,
// and if not, the exact remedy.
//
// The subtlety that makes this worth real code: git does NOT descend into an
// ignored directory, so a repo whose .gitignore says `.memcode/` cannot be
// fixed by ADDING an exception — `!.memcode/tasks/` under it does nothing at
// all. The ignore of the parent has to be loosened to `.memcode/*` first, which
// ignores the children while leaving the directory itself traversable. Verified
// against real `git check-ignore` behaviour in TestTaskVisibilityRemedy.
type taskVisibility struct {
	Rule     string   // the winning ignore rule, e.g. ".gitignore:4:.memcode/"
	Remedy   []string // the lines that actually expose tasks/
	Shadowed bool
}

// RemedyLines are the ignore rules that expose .memcode/tasks while leaving the
// rest of .memcode ignored. They REPLACE a `.memcode/` rule rather than joining
// it; the second line makes the fix self-sufficient even if memcode's own
// .memcode/.gitignore is missing or has been edited.
var RemedyLines = []string{".memcode/*", "!.memcode/tasks/"}

func checkTaskVisibility(root string) taskVisibility {
	if root == "" {
		return taskVisibility{}
	}
	dir := filepath.Join(root, ".memcode", "tasks")
	if _, err := os.Stat(dir); err != nil {
		return taskVisibility{}
	}
	if _, err := exec.LookPath("git"); err != nil {
		return taskVisibility{}
	}
	probe := filepath.Join(dir, ".probe.yaml")
	// The boolean comes from -q, NOT from -v. With -v, check-ignore exits 0
	// whenever any pattern MATCHES, including a negation, so a healthy repo
	// whose !tasks/** rule matched would be reported as shadowed. -q exits 0
	// only when the path is genuinely excluded.
	if err := exec.Command("git", "-C", root, "check-ignore", "-q", probe).Run(); err != nil {
		return taskVisibility{} // visible, or not a git repo
	}
	out, err := exec.Command("git", "-C", root, "check-ignore", "-v", probe).Output()
	if err != nil {
		return taskVisibility{Remedy: RemedyLines, Shadowed: true}
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return taskVisibility{Remedy: RemedyLines, Shadowed: true}
	}
	// "<file>:<line>:<pattern>\t<path>" — the rule is the useful half.
	rule := line
	if fields := strings.SplitN(line, "\t", 2); len(fields) > 0 {
		rule = fields[0]
	}
	return taskVisibility{Rule: rule, Remedy: RemedyLines, Shadowed: true}
}

// warnIfShadowed prints what is hiding project tasks and how to fix it.
//
// It REPORTS and never fixes: the root .gitignore is a file the user curates,
// and EnsureGitignore's standing contract is that memcode does not edit it.
func warnIfShadowed(root string) {
	v := checkTaskVisibility(root)
	if !v.Shadowed {
		return
	}
	fmt.Fprintf(os.Stderr, "\n  ! project tasks are not visible to git — %s ignores them.\n", v.Rule)
	fmt.Fprintf(os.Stderr, "    They still run, but they will not travel with the repo or show up in review.\n")
	fmt.Fprintf(os.Stderr, "    Git will not descend into an ignored directory, so adding an exception under\n")
	fmt.Fprintf(os.Stderr, "    that rule does nothing. REPLACE it in .gitignore with:\n")
	for _, l := range v.Remedy {
		fmt.Fprintf(os.Stderr, "        %s\n", l)
	}
}

var taskListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List the tasks visible from this project",
	RunE: func(cmd *cobra.Command, args []string) error {
		tasks, errs := task.Load(taskRoot(), time.Now())
		reportLoadErrors(errs)
		if len(tasks) == 0 {
			fmt.Println("No tasks yet. Write one at .memcode/tasks/<name>.yaml, or ask memcode to")
			fmt.Println("box up a piece of work you expect to repeat.")
			return nil
		}
		warnIfShadowed(taskRoot())
		for _, t := range tasks {
			state := ""
			if !t.IsEnabled() {
				state = " (disabled)"
			}
			fmt.Printf("  %-28s %-22s %-9s %s%s\n",
				t.Name, cadence(t), t.Autonomy.Level, t.Description, state)
		}
		return nil
	},
}

var taskShowCmd = &cobra.Command{
	Use:     "show <name>",
	Aliases: []string{"get"},
	Short:   "Show one task's full definition and resolved authority",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root := taskRoot()
		t, err := task.Get(root, args[0], time.Now())
		if err != nil {
			return err
		}
		rev, err := t.Revision()
		if err != nil {
			return err
		}
		grants, err := t.Grants()
		if err != nil {
			return err
		}

		fmt.Printf("%s\n", t.Name)
		if t.Description != "" {
			fmt.Printf("  %s\n", t.Description)
		}
		fmt.Printf("\n  file        %s (%s scope)\n", t.Path, t.Scope)
		fmt.Printf("  revision    %s\n", rev)
		fmt.Printf("  enabled     %t\n", t.IsEnabled())
		if project, err := t.ResolveProject(root); err == nil {
			fmt.Printf("  project     %s\n", project)
		}
		fmt.Printf("  cadence     %s\n", cadence(t))
		for _, tr := range t.Triggers {
			if tr.Manual {
				continue
			}
			if n, ok, err := occurrence.Next(taskrun.Spec(tr), time.Now()); err == nil && ok {
				fmt.Printf("  next fire   %s (%s)\n", n.Local().Format(time.RFC3339), tr.Missed)
			}
		}
		fmt.Printf("  execution   %s", t.Execution.Mode)
		if t.Execution.Procedure != "" {
			fmt.Printf(" (procedure %s)", t.Execution.Procedure)
		}
		fmt.Printf("\n  runtime     %s\n", t.Runtime.Strategy)
		fmt.Printf("  allowed     %s\n", strings.Join(t.Runtime.Allowed, " > "))
		fmt.Printf("  model       %s\n", t.Runtime.Model)
		fmt.Printf("  timeout     %s\n", t.Timeout())

		// Show the EXPANDED authority, not the tier name. The tier is a label;
		// the grants are what the policy engine actually enforces, and that is
		// what someone auditing an unattended task needs to see.
		fmt.Printf("\n  authority   %s\n", t.Autonomy.Level)
		for _, g := range grants {
			fmt.Printf("                %s\n", g)
		}
		if t.MayOpenPR() {
			fmt.Printf("  pull request %s, branch %s\n", t.Git.PullRequest, t.Git.Branch)
		}
		if len(t.Verify.Commands) > 0 {
			fmt.Printf("\n  verify\n")
			for _, c := range t.Verify.Commands {
				fmt.Printf("                %s\n", c)
			}
		}
		if t.Instructions != "" {
			fmt.Printf("\n  instructions\n")
			for _, line := range strings.Split(strings.TrimRight(t.Instructions, "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
		return nil
	},
}

var taskCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Validate every task definition and report what is wrong",
	Long: `Parses every task file and reports the ones that do not load. Worth running after
hand-editing a definition: the scheduler skips a task it cannot parse, so a file with a typo
is silently not running rather than loudly broken.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		tasks, errs := task.Load(taskRoot(), time.Now())
		reportLoadErrors(errs)
		if len(errs) > 0 {
			return fmt.Errorf("%d task file(s) failed to load", len(errs))
		}
		fmt.Printf("%d task(s) OK\n", len(tasks))
		warnIfShadowed(taskRoot())
		return nil
	},
}

var taskRunCmd = &cobra.Command{
	Use:   "run <name>",
	Short: "Run a task now and record the result",
	Long: `Runs a task immediately, in the foreground, and writes a durable run record.

This is the ordinary way to use a task, not a way to test one. A task is a runnable object;
attaching a trigger is what makes it also happen on its own.

The run freezes its inputs when it starts — the definition and its revision, the resolved
project, the expanded authority, the runtime, the limits — so editing the task file while it
runs cannot change what the run meant.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		root := taskRoot()
		t, err := task.Get(root, args[0], time.Now())
		if err != nil {
			return err
		}
		store, err := openRuns(ctx)
		if err != nil {
			return err
		}
		defer store.Close()

		// Refuse to pile a second run onto a task that is already going. The full
		// concurrency policy (queue vs skip, per project) lands with triggers;
		// this is the floor that stops the obvious foot-gun.
		if active, err := store.Active(ctx, t.Name); err == nil && len(active) > 0 {
			return fmt.Errorf("%s is already running (%s) — wait for it or stop that process",
				t.Name, active[0].ID)
		}

		fmt.Printf("%s · %s · %s\n", t.Name, t.Autonomy.Level, t.Runtime.Strategy)
		run, err := taskrun.NewRunner(store, loadAuthorizations).Run(ctx, t, root, taskrun.TriggerManual, "")
		if err != nil {
			return err
		}
		fmt.Printf("\n%s  %s  %s\n", outcomeMark(run.Outcome), run.Outcome, run.Summary)
		fmt.Printf("   run %s · %s\n", run.ID, run.Revision)
		if run.LogPath != "" {
			fmt.Printf("   log %s\n", run.LogPath)
		}
		if run.Detail != "" && run.Outcome != taskrun.OutcomeSuccess {
			fmt.Printf("\n%s\n", run.Detail)
		}
		if !run.OK() {
			return fmt.Errorf("task %s: %s", t.Name, run.Outcome)
		}
		return nil
	},
}

var taskPollCmd = &cobra.Command{
	Use:   "poll",
	Short: "Run whatever is due now (what the daemon does on its own)",
	Long: `Recomputes due occurrences from the calendar and each trigger's watermark, then runs
what the missed-run policy says to run. This is exactly what the gateway daemon does every 30
seconds; running it by hand is how you watch it happen.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		root := taskRoot()
		tasks, errs := task.Load(root, time.Now())
		reportLoadErrors(errs)
		store, err := openRuns(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		res := taskrun.NewRunner(store, loadAuthorizations).Poll(ctx, tasks, root, time.Now())
		for _, e := range res.Errs {
			fmt.Fprintf(os.Stderr, "  ! %v\n", e)
		}
		if len(res.Started) == 0 {
			fmt.Printf("Nothing due.")
			if res.Skipped > 0 {
				fmt.Printf(" %d occurrence(s) accounted for without running.", res.Skipped)
			}
			fmt.Println()
			return nil
		}
		for _, r := range res.Started {
			fmt.Printf("  %s %-22s %-16s %s\n", outcomeMark(r.Outcome), r.Task, r.Outcome, r.Summary)
		}
		return nil
	},
}

var taskHistoryCmd = &cobra.Command{
	Use:     "history [name]",
	Aliases: []string{"runs"},
	Short:   "Show past runs, newest first",
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		store, err := openRuns(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		var name string
		if len(args) == 1 {
			name = args[0]
		}
		runs, err := store.Recent(ctx, name, 30)
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			fmt.Println("No runs yet.")
			return nil
		}
		for _, r := range runs {
			took := ""
			if !r.FinishedAt.IsZero() {
				took = r.FinishedAt.Sub(r.StartedAt).Round(time.Second).String()
			}
			note := r.Summary
			if r.PRURL != "" {
				note = r.PRURL + "  " + note
			}
			fmt.Printf("  %s %-16s %-22s %-8s %-7s %s\n",
				outcomeMark(r.Outcome), r.Outcome, r.Task,
				r.StartedAt.Local().Format("Jan 02 15:04"), took, note)
		}
		return nil
	},
}

var taskInboxCmd = &cobra.Command{
	Use:   "inbox",
	Short: "Show runs you have not looked at yet",
	Long: `The inbox is the durable ledger of autonomous activity. Every run lands here whether
or not anyone was watching; other delivery (a PR, a chat message, a desktop notification) is
notification layered on top of it, never a replacement.

Listing marks runs as seen. Seen is not the same as dealt with: use ` + "`task ack`" + ` for that.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		store, err := openRuns(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		runs, err := store.Unseen(ctx, 50)
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			// A suspended responsibility outranks an empty inbox. "Nothing new"
			// on a task memcode has quietly stopped fulfilling is the most
			// misleading thing this command could say.
			if n := printPaused(ctx, store); n == 0 {
				fmt.Println("Nothing new.")
			}
			return nil
		}
		printPaused(ctx, store)
		fmt.Printf("Autonomous activity — %d run(s) since you last looked\n", len(runs))
		ids := make([]string, 0, len(runs))
		for _, r := range runs {
			fmt.Printf("  %s %-22s %-16s %s\n", outcomeMark(r.Outcome), r.Task, r.Outcome, r.Summary)
			ids = append(ids, r.ID)
		}
		needs := 0
		for _, r := range runs {
			if r.Outcome == taskrun.OutcomeNeedsAttention {
				needs++
			}
		}
		if needs > 0 {
			fmt.Printf("\n%d need your attention — `memcode task show-run <id>` for the detail.\n", needs)
		}
		return store.MarkSeen(ctx, ids)
	},
}

// printPaused surfaces suspended tasks ABOVE ordinary run summaries.
//
// A paused automation is something the user delegated and memcode has stopped
// doing. That outranks "three automations ran" every time, and it is the one
// piece of state that will not resolve itself while it waits.
func printPaused(ctx context.Context, store *taskrun.Store) int {
	paused, err := store.Paused(ctx)
	if err != nil || len(paused) == 0 {
		return 0
	}
	fmt.Printf("⏸ %d automation(s) need your input\n", len(paused))
	for _, p := range paused {
		fmt.Printf("\n  %s — paused %s\n", p.Task, humanSince(p.Since))
		for _, line := range strings.Split(strings.TrimSpace(p.Reason), "\n") {
			fmt.Printf("    %s\n", line)
		}
		if p.RunID != "" {
			fmt.Printf("\n    evidence  memcode task show-run %s\n", p.RunID)
		}
		fmt.Printf("    resume    memcode task resume %s\n", p.Task)
	}
	fmt.Println()
	return len(paused)
}

func humanSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return "just now"
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

var taskPausedCmd = &cobra.Command{
	Use:   "paused",
	Short: "Automations that stopped and need a decision",
	Long: `A task suspends itself when carrying on would mean inventing intent it does not have,
taking authority it was not given, or making a consequential choice on your behalf. Those
conditions do not clear on their own, so it stops rather than rebuilding the same failure
every week.

Resolve it and run ` + "`memcode task resume <name>`" + `.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		store, err := openRuns(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		if n := printPaused(ctx, store); n == 0 {
			fmt.Println("Nothing paused.")
		}
		return nil
	},
}

var taskResumeCmd = &cobra.Command{
	Use:   "resume <name>",
	Short: "Lift a suspension and let a task run again",
	Long: `Use this once you have resolved what stopped it. Nothing is re-run immediately: the task
becomes eligible again and fires on its next occurrence.

If the answer changed what the task should DO, edit its instructions before resuming —
otherwise the next run walks into the same wall.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		store, err := openRuns(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		lifted, err := store.Resume(ctx, args[0], taskRoot())
		if err != nil {
			return err
		}
		if !lifted {
			return fmt.Errorf("%s is not paused", args[0])
		}
		fmt.Printf("%s will run again on its next occurrence.\n", args[0])
		return nil
	},
}

var taskAckCmd = &cobra.Command{
	Use:   "ack <run-id>",
	Short: "Mark a run as dealt with",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		store, err := openRuns(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.Acknowledge(ctx, args[0]); err != nil {
			return err
		}
		fmt.Printf("acknowledged %s\n", args[0])
		return nil
	},
}

var taskShowRunCmd = &cobra.Command{
	Use:   "show-run <run-id>",
	Short: "Show one run in full, including the definition it froze",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		store, err := openRuns(ctx)
		if err != nil {
			return err
		}
		defer store.Close()
		r, err := store.Get(ctx, args[0])
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", r.ID)
		fmt.Printf("  task        %s\n", r.Task)
		fmt.Printf("  revision    %s\n", r.Revision)
		fmt.Printf("  trigger     %s", r.TriggerKind)
		if r.TriggerID != "" {
			fmt.Printf(" (%s)", r.TriggerID)
		}
		fmt.Printf("\n  project     %s\n", r.Project)
		fmt.Printf("  runtime     %s", r.RuntimeResolved)
		if r.ModelResolved != "" {
			fmt.Printf(" / %s", r.ModelResolved)
		}
		if r.RuntimeRequested != "" {
			fmt.Printf("  (asked for %s", r.RuntimeRequested)
			if r.ModelRequested != "" {
				fmt.Printf(" / %s", r.ModelRequested)
			}
			fmt.Printf(")")
		}
		fmt.Println()
		if r.AuthID != "" {
			fmt.Printf("  authorized  %s (%s scope)\n", r.AuthID, r.AuthScope)
		}
		fmt.Printf("  authority   %s\n", strings.Join(r.Grants, ", "))
		fmt.Printf("  state       %s / %s (%s)\n", r.State, r.Outcome, r.Seen)
		fmt.Printf("  started     %s\n", r.StartedAt.Local().Format(time.RFC3339))
		if !r.FinishedAt.IsZero() {
			fmt.Printf("  finished    %s (%s)\n", r.FinishedAt.Local().Format(time.RFC3339),
				r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
		}
		if r.ExecStatus != "" {
			fmt.Printf("  execution   %s\n", r.ExecStatus)
		}
		if r.VerifyStatus != "" {
			fmt.Printf("  verification %s\n", r.VerifyStatus)
		}
		if r.Branch != "" {
			fmt.Printf("  branch      %s", r.Branch)
			if r.CreatedBranch {
				fmt.Printf(" (created by this run)")
			}
			fmt.Println()
		}
		if r.BaseRev != "" {
			fmt.Printf("  base        %s\n", r.BaseRev)
		}
		if r.CommitSHA != "" {
			fmt.Printf("  commit      %s", r.CommitSHA)
			if r.CreatedCommit {
				fmt.Printf(" (created by this run)")
			}
			fmt.Println()
		}
		if r.PRURL != "" {
			fmt.Printf("  pull request %s", r.PRURL)
			if r.CreatedPR {
				fmt.Printf(" (opened by this run)")
			}
			fmt.Println()
		}
		if r.Worktree != "" {
			fmt.Printf("  worktree    %s (kept for inspection)\n", r.Worktree)
		}
		if r.LogPath != "" {
			fmt.Printf("  log         %s\n", r.LogPath)
		}
		if r.Summary != "" {
			fmt.Printf("\n  %s\n", r.Summary)
		}
		if r.Detail != "" {
			fmt.Printf("\n%s\n", r.Detail)
		}
		// The frozen definition is the whole point of the record: it is what this
		// run actually ran, regardless of what the file says now.
		fmt.Printf("\n--- definition as frozen at run time ---\n%s", r.Definition)
		return nil
	},
}

func init() {
	taskCmd.AddCommand(taskListCmd, taskShowCmd, taskCheckCmd,
		taskRunCmd, taskPollCmd, taskHistoryCmd, taskInboxCmd, taskAckCmd, taskShowRunCmd,
		taskPausedCmd, taskResumeCmd)
	rootCmd.AddCommand(taskCmd)
}
