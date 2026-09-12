package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/runtimes"
	"github.com/memcode-ai/memcode/internal/task"
	"github.com/memcode-ai/memcode/internal/taskdetect"
	"github.com/memcode-ai/memcode/internal/taskrun"
)

// The task tool is how an EXPLICIT request becomes a real task inside the turn
// that asked for it.
//
// "Keep our dependencies current" is not a hint to be filed away and offered
// back later — it is an instruction. Recording a signal and surfacing a
// suggestion in some future session would be a strange way to answer someone
// who just asked for something.
//
// It refuses to guess about the things that matter. Most of a task is
// inferable; a few decisions are not, and getting those wrong shows up at 3am
// with nobody watching. When something is genuinely undecidable the tool says
// what it needs, and the agent asks.

type taskToolInput struct {
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	Instructions        string   `json:"instructions"`
	Family              string   `json:"family"`
	Operation           string   `json:"operation"`
	Target              string   `json:"target"`
	Every               string   `json:"every"`
	Cron                string   `json:"cron"`
	Verify              []string `json:"verify"`
	CodeChanges         string   `json:"code_changes"`
	KnownGood           string   `json:"known_good"`
	ClarifyingQuestions []string `json:"clarifying_questions"`
	Does                []string `json:"does"`
	SuccessCriteria     string   `json:"success_criteria"`
	SideEffects         []string `json:"side_effects"`
	JustCompleted       string   `json:"just_completed"`
	Projects            []string `json:"projects"`
	Coordination        string   `json:"coordination"`
	Responsibility      string   `json:"responsibility"`
	VerifyAcross        []string `json:"verify_across"`
}

// useTask creates an autonomous task from an explicit request.
func (s *Session) useTask(ctx context.Context, in taskToolInput) string {
	p := taskdetect.Proposal{
		Family: firstNonBlank(in.Family, in.Name), Operation: in.Operation, Target: in.Target,
		Scope: "repository", Project: s.root,
		Name: in.Name, Reason: in.Description, Instructions: in.Instructions,
		Verify: in.Verify, SuggestedEvery: in.Every, SuggestedCron: in.Cron,
		Effect: effectOf(in.CodeChanges), ExplicitRequest: true,
		ClarifyingQuestions: in.ClarifyingQuestions,
		// The user asked, so shape and recurrence are not in question here.
		Confidence: 1, Bounded: true, Reproducible: true, UnattendedSafe: true, Evaluable: true,
		Recurrence: taskdetect.Recurrence{Kind: taskdetect.RecurrenceUserPattern, Confidence: 1,
			Cause: "the user asked for this to run automatically"},
	}

	// ASK before creating, when there is something real to ask about. Returned
	// to the model rather than prompted here, so the questions arrive in the
	// conversation the user is already having.
	if gaps := p.Gaps(); len(gaps) > 0 {
		var b strings.Builder
		b.WriteString("Not enough to run this safely on its own yet. Ask the user:\n\n")
		for _, g := range gaps {
			fmt.Fprintf(&b, "  - %s\n    (%s)\n", g.Question, g.Why)
		}
		b.WriteString("\nThen call task again with the answers filled in.")
		return b.String()
	}

	t, err := p.ToTask(time.Now())
	if err != nil {
		return "could not build the task: " + err.Error()
	}
	// What worked today, recorded as a starting point for future runs. Marked
	// as a hint everywhere it is used: the repository will drift, and a run
	// that treats this as instructions will eventually be confidently wrong.
	if kg := strings.TrimSpace(in.KnownGood); kg != "" {
		t.Execution.KnownGood = fmt.Sprintf("(worked %s) %s", time.Now().UTC().Format("2006-01-02"), kg)
	}

	// WHOSE responsibility is this? Not the same question as where the
	// conversation happened. A concern implemented in two checkouts that gets
	// written into one of them keeps half the product current and reports
	// success for it.
	if err := applyOwnership(&t, in, s.root); err != nil {
		return err.Error()
	}

	// PROVE IT, THEN ASK. Creating the task and trialling it afterwards gets the
	// order backwards: the user would be approving a description, and would only
	// find out whether it actually works once it was already installed and
	// scheduled. So validation happens first, and its real result is part of
	// what they sign off on. Nothing is written until they say yes.
	//
	// Work that was just done successfully in this conversation is already that
	// evidence. Re-running it to prove a point that has been proven wastes the
	// user's time and money, and on a task with side effects it does the work
	// twice.
	var val validation
	if done := strings.TrimSpace(in.JustCompleted); done != "" {
		val = validation{ok: true, how: "the work just done in this session", detail: done}
	} else {
		trial := t
		trial.Git.PullRequest = task.PRNever // a trial publishes nothing
		val = s.trialRun(ctx, trial)
	}

	ok, reason := s.gate(ctx, permissions.Medium, false, ApprovalRequest{
		Title:    "Automation: " + t.Description,
		Label:    "Create automation",
		Detail:   taskReport(t, in, val),
		Editable: false,
	})
	if !ok {
		return "The user declined" + orEmptyReason(reason) + ". Nothing was created."
	}

	path, err := task.Save(s.root, t, storageScope(t, s.root))
	if err != nil {
		return "could not write the task: " + err.Error()
	}
	return fmt.Sprintf("Created %s — it runs %s from now on.", path, cadenceOf(t))
}

// applyOwnership records the boundary of the responsibility on the task.
//
// Paths are resolved against the current checkout and required to exist NOW,
// because a project path that is already wrong at creation time will not become
// right by 3am. Whether they are still right on a later run is a different
// problem, and the answer to that one is Discover, not validation.
func applyOwnership(t *task.Task, in taskToolInput, root string) error {
	var projects []string
	for _, p := range trimmedNonBlank(in.Projects) {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		p = filepath.Clean(p)
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			return fmt.Errorf("project %q is not a directory on this machine — name the checkouts "+
				"by their real paths, or ask the user where they are", p)
		}
		projects = append(projects, p)
	}
	// One project that IS the anchor is not cross-cutting; saying so would turn
	// every ordinary task into a multi-project run for no reason.
	if len(projects) == 1 && projects[0] == filepath.Clean(root) {
		projects = nil
	}
	if len(projects) == 0 {
		return nil
	}
	t.Ownership.Projects = projects
	t.Ownership.Responsibility = strings.TrimSpace(in.Responsibility)
	t.Verify.Across = trimmedNonBlank(in.VerifyAcross)
	switch c := task.Coordination(strings.TrimSpace(in.Coordination)); c {
	case task.CoordIndependent, task.CoordCoordinated:
		t.Ownership.Coordination = c
	default:
		t.Ownership.Coordination = task.CoordIndependent
	}
	return nil
}

// storageScope puts the definition where the responsibility is OWNED.
//
// A cross-cutting concern written into one of the repositories it spans gives
// that repo an ownership it does not have: clone it alone and the task claims
// to maintain projects that are not there; clone the other and the concern has
// vanished. Global is not "runs everywhere" — it is "no single repository owns
// this", which for a genuinely shared responsibility is simply the truth.
func storageScope(t task.Task, root string) task.Scope {
	if root == "" || t.CrossProject() {
		return task.ScopeGlobal
	}
	return task.ScopeProject
}

// validation is what was actually demonstrated before asking, as opposed to
// what was intended.
//
// Design confidence and runtime confidence are different things: the model can
// be entirely clear about the responsibility and still discover, on trying it,
// that this repo has no test command that passes or that the change scope is
// much wider than the sentence implied. Keeping the two apart is the whole
// point of validating before the approval rather than after it.
type validation struct {
	ok     bool
	how    string // where the evidence came from
	detail string
}

// taskReport is the automation contract the user signs off on: what it will do,
// how it will know it worked, what it may touch, and what was actually shown to
// work just now.
//
// Deliberately says nothing about YAML, grants, worktrees-as-mechanism or
// occurrence policy. Someone deciding whether to let a machine do this while
// they sleep needs to understand the behaviour and the blast radius, not the
// representation underneath.
func taskReport(t task.Task, in taskToolInput, val validation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", firstNonBlank(t.Description, t.Name))

	if t.CrossProject() {
		b.WriteString("Scope\n")
		for _, p := range t.Targets() {
			fmt.Fprintf(&b, "  · %s\n", p)
		}
		if t.Ownership.Coordination == task.CoordCoordinated {
			b.WriteString("  · all of them together, or none of them\n")
		} else {
			b.WriteString("  · each project stands on its own\n")
		}
		b.WriteString("  · kept outside all of them, since no one repo owns this\n\n")
	}

	b.WriteString("What it will do\n")
	for _, d := range behaviour(t, in) {
		fmt.Fprintf(&b, "  · %s\n", d)
	}

	fmt.Fprintf(&b, "\nHow it knows it worked\n  · %s\n", successCriteria(t, in))

	b.WriteString("\nWhat it may change\n")
	for _, e := range sideEffects(t, in) {
		fmt.Fprintf(&b, "  · %s\n", e)
	}

	b.WriteString("\nValidated now\n")
	switch {
	case val.ok:
		fmt.Fprintf(&b, "  · %s: %s\n", val.how, clip(val.detail, 400))
	case val.how == "":
		fmt.Fprintf(&b, "  · could not be tried — %s\n", clip(val.detail, 300))
		b.WriteString("  · it is untested, so the first real run is the first test\n")
	default:
		fmt.Fprintf(&b, "  · %s, and it did NOT work: %s\n", val.how, clip(val.detail, 400))
		b.WriteString("  · creating it now means it will probably fail the same way unattended\n")
	}
	return b.String()
}

// behaviour is the "what it will do" list. The model writes it, because it is
// the only party that knows what the work involves; these fallbacks exist so a
// terse call still produces a contract rather than an empty heading.
func behaviour(t task.Task, in taskToolInput) []string {
	out := trimmedNonBlank(in.Does)
	if len(out) == 0 {
		out = wrapLines(t.Instructions, 4)
	}
	out = append(out, "runs "+cadenceOf(t))
	// Not an implementation detail: it is the reason an unattended run cannot
	// disturb whatever the user has open at the time.
	out = append(out, "works in its own checkout, never in your working tree")
	// The drift contract, stated where they can hold us to it.
	out = append(out, "re-checks the current state each run instead of replaying today's steps")
	// The promise that makes the rest safe to agree to. Without it, "runs
	// unattended forever" is an open-ended commitment; with it, the worst case
	// is that it stops and tells you why.
	out = append(out, "stops and asks rather than guessing, if it meets a decision that is yours")
	if t.CrossProject() {
		out = append(out, "works only in the projects listed above; reaching a new one needs your approval")
	}
	return out
}

func successCriteria(t task.Task, in taskToolInput) string {
	if s := strings.TrimSpace(in.SuccessCriteria); s != "" {
		return s
	}
	if len(t.Verify.Commands) > 0 {
		s := "these must pass: " + strings.Join(t.Verify.Commands, ", ")
		if len(t.Verify.Across) > 0 {
			s += "; and across projects: " + strings.Join(t.Verify.Across, ", ")
		}
		return s
	}
	return "the agent's own judgement — nothing mechanical to check against"
}

func sideEffects(t task.Task, in taskToolInput) []string {
	out := trimmedNonBlank(in.SideEffects)
	switch t.Git.PullRequest {
	case task.PRNever:
		out = append(out, "nothing is pushed or published; changes stay local")
	default:
		if t.CrossProject() && t.Ownership.Coordination == task.CoordCoordinated {
			out = append(out, "opens pull requests in every project at once, or in none of them")
		} else {
			out = append(out, "commits to a branch of its own and opens a pull request for you to review")
		}
		out = append(out, "never touches your default branch, and never merges anything")
	}
	if t.Autonomy.Level == task.LevelReadOnly {
		out = append(out, "reads only — it cannot modify files at all")
	}
	return out
}

func trimmedNonBlank(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// wrapLines trims instructions to the first few meaningful lines — the report
// summarises, and the full text is in the file.
func wrapLines(s string, max int) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
		if len(out) == max {
			out = append(out, "…")
			break
		}
	}
	if len(out) == 0 {
		out = []string{"(no instructions)"}
	}
	return out
}

// trialRun executes a freshly created task once and reports what happened.
//
// Failure here is USEFUL, not embarrassing: it means the instructions do not
// survive being read by someone who was not in the conversation, which is
// exactly what every future run will be.
func (s *Session) trialRun(ctx context.Context, t task.Task) validation {
	store, err := taskrun.OpenDefault(ctx)
	if err != nil {
		return validation{detail: err.Error()}
	}
	defer store.Close()

	rctx, cancel := context.WithTimeout(ctx, trialTimeout)
	defer cancel()

	run, err := taskrun.NewRunner(store, s.taskAuth).Run(rctx, t, s.root, taskrun.TriggerManual, "")
	if err != nil {
		return validation{detail: err.Error()}
	}
	const how = "tried against this repo just now"
	if run.OK() {
		return validation{ok: true, how: how, detail: string(run.Outcome) + " — " + run.Summary}
	}
	return validation{how: how,
		detail: string(run.Outcome) + " — " + clip(firstLineOf(run.Summary)+" "+run.Detail, 500)}
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// trialTimeout bounds the proving run so creating a task cannot hang a turn.
const trialTimeout = 10 * time.Minute

// taskGateDetail shows what the user is actually authorizing: not "write a
// file" but "this will run by itself, with this authority, on this cadence".
func taskGateDetail(t task.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", t.Description)
	fmt.Fprintf(&b, "runs      %s\n", cadenceOf(t))
	fmt.Fprintf(&b, "authority %s (%s)\n", t.Autonomy.Level, prPolicy(t))
	if len(t.Verify.Commands) > 0 {
		fmt.Fprintf(&b, "verifies  %s\n", strings.Join(t.Verify.Commands, ", "))
	}
	return b.String()
}

func cadenceOf(t task.Task) string {
	if len(t.Triggers) == 0 {
		return "only when you run it"
	}
	tr := t.Triggers[0]
	switch {
	case tr.Cron != "":
		return tr.Cron
	case tr.Every != "":
		if d, err := time.ParseDuration(tr.Every); err == nil && d >= 24*time.Hour {
			return fmt.Sprintf("every %d days", int(d.Hours()/24))
		}
		return "every " + tr.Every
	}
	return "only when you run it"
}

func prPolicy(t task.Task) string {
	if t.Autonomy.Level == task.LevelReadOnly {
		return "reports only, changes nothing"
	}
	if t.Git.PullRequest == task.PRNever {
		return "commits to a branch, opens no PR"
	}
	return "opens a PR when it changes code"
}

func effectOf(v string) taskdetect.Effect {
	switch taskdetect.Effect(strings.TrimSpace(v)) {
	case taskdetect.EffectNone:
		return taskdetect.EffectNone
	case taskdetect.EffectExpected:
		return taskdetect.EffectExpected
	}
	return taskdetect.EffectPossible
}

// taskAuth answers "which runtimes may a trial run use". INJECTED rather than
// read here: the coding engine must not reach into the gateway's configuration
// (internal/guard.TestEngineDoesNotImportGateway), and the layering is right —
// the engine executes, the host decides what it is permitted to execute on.
// Nil means no subscription authorizations, which resolves to hosted.
func (s *Session) taskAuth() runtimes.Authorizations {
	if s.runtimeAuth == nil {
		return nil
	}
	return s.runtimeAuth()
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func orEmptyReason(r string) string {
	if strings.TrimSpace(r) == "" {
		return ""
	}
	return ": " + r
}
