// Package task is memcode's autonomous Task: a reusable, declarative,
// auditable unit of work the agent can execute with no human present.
//
// A Task is NOT inherently recurring. It is a runnable object first —
// `memcode task run <name>` is a first-class entry point, not a testing
// affordance — and a trigger is an optional attachment. That ordering is the
// whole point: cron is one way a Task becomes eligible, never what a Task is.
//
// Three objects stay deliberately separate:
//
//	Task     what to do, how, with what authority   (YAML, hashable)
//	Trigger  when it becomes eligible               (0..n, optional)
//	Run      one immutable execution                (a DB row, package taskrun)
//
// Every run records the Revision of the definition that produced it, so
// "why did this task push that branch?" stays answerable after the YAML has
// been edited five times. The autonomy package hashes delegation policies for
// the same reason and by the same method (see autonomy.CanonicalPolicy).
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/runtimes"
)

// Version is the schema version every task file must declare. An explicit
// version on a user-editable file is what lets the format change later without
// guessing at the shape of what is on disk.
const Version = 1

// Scope says where a definition was loaded from. Project scope wins over global
// on a name collision: a repo that ships its own task means it for that repo.
type Scope string

const (
	ScopeProject Scope = "project"
	ScopeGlobal  Scope = "global"
)

// Task is one task definition, as authored in YAML plus the resolved facts the
// loader attaches. Zero-value fields are filled by ApplyDefaults before
// validation or hashing, so a sparse file and its fully-written equivalent
// produce the same Revision.
type Task struct {
	Version      int         `yaml:"version" json:"version"`
	Name         string      `yaml:"name" json:"name"`
	Description  string      `yaml:"description,omitempty" json:"description,omitempty"`
	Enabled      *bool       `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Project      string      `yaml:"project,omitempty" json:"project,omitempty"`
	Ownership    Ownership   `yaml:"ownership,omitempty" json:"ownership"`
	Triggers     []Trigger   `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	Instructions string      `yaml:"instructions" json:"instructions"`
	Execution    Execution   `yaml:"execution,omitempty" json:"execution"`
	Runtime      Runtime     `yaml:"runtime,omitempty" json:"runtime"`
	Autonomy     Autonomy    `yaml:"autonomy,omitempty" json:"autonomy"`
	Git          Git         `yaml:"git,omitempty" json:"git"`
	Verify       Verify      `yaml:"verify,omitempty" json:"verify"`
	Delivery     Delivery    `yaml:"delivery,omitempty" json:"delivery"`
	Limits       Limits      `yaml:"limits,omitempty" json:"limits"`
	Concurrency  Concurrency `yaml:"concurrency,omitempty" json:"concurrency"`

	// Resolved by the loader, never authored and never hashed.
	Path  string `yaml:"-" json:"-"`
	Scope Scope  `yaml:"-" json:"-"`
}

// Ownership answers "whose responsibility is this?", which is not the same
// question as "where was the conversation happening?".
//
// A task created while someone happened to be sitting in one checkout can
// easily belong to something larger. memcode itself is the example: the model
// catalog is one responsibility implemented in two repositories, and a task
// that updated only whichever one was open would be quietly wrong forever.
//
// So a task names the projects its responsibility spans, and how their work
// relates. Project is still where the task is ANCHORED — the repo it was
// created against, and the one whose worktree a single-project run uses.
// Projects widens that to the full set when the responsibility is genuinely
// cross-cutting.
type Ownership struct {
	// Projects are the checkouts this responsibility spans. Empty means the
	// task's own Project and nothing else, which is the common case.
	Projects []string `yaml:"projects,omitempty" json:"projects,omitempty"`

	// Coordination decides what happens when the work succeeds in some
	// projects and not others. This is a real product decision, not a detail:
	// "PR opened in repo A" while repo B could not be updated is a WORSE
	// outcome than doing nothing, if the two changes only make sense together.
	Coordination Coordination `yaml:"coordination,omitempty" json:"coordination,omitempty"`

	// Responsibility states what this task is responsible FOR, independently of
	// today's file layout. It is what a run judges its own share against when
	// the repository has moved under it.
	//
	// It is NOT a licence to go looking for more projects. The approved project
	// set is fixed at creation and changes only when the user changes the
	// automation. Drift is allowed WITHIN an approved boundary — paths, build
	// commands, package managers, moved code, refactors, whatever the run finds
	// there. Expanding the boundary is not drift; it is new authority, and
	// authority comes from a person.
	//
	// So if the responsibility grows into a repository nobody approved, the run
	// stops and says so. Discovery is not authorization, the same way a login
	// found in another tool's files is not consent to use it.
	Responsibility string `yaml:"responsibility,omitempty" json:"responsibility,omitempty"`
}

// Coordination is the publication semantics of a multi-project run.
type Coordination string

const (
	// CoordIndependent publishes each project's work on its own merits. Right
	// when the projects merely share a chore ("keep dependencies current"):
	// repo B being stuck is no reason to withhold repo A's upgrade.
	CoordIndependent Coordination = "independent"

	// CoordCoordinated publishes all of it or none of it. Right when the
	// changes only make sense together, which is the whole reason the task is
	// cross-project rather than two tasks. A partial success is not a success;
	// it is a half-applied change to a system that was consistent before.
	CoordCoordinated Coordination = "coordinated"
)

// Targets is the set of checkouts a run must work through, anchor first.
//
// Always non-empty for a valid task, so callers never special-case the
// single-project shape.
func (t Task) Targets() []string { return t.TargetsFrom(t.Project) }

// TargetsFrom is Targets with the anchor supplied by the caller.
//
// A definition does not always carry its own project: a global task is anchored
// by the run, and a file authored without a `project:` key is anchored by where
// it was loaded from. The anchor is a property of the RUN in those cases, and
// pretending otherwise silently produces an empty target list — which for the
// lease means no lock at all.
func (t Task) TargetsFrom(anchor string) []string {
	if len(t.Ownership.Projects) == 0 {
		if anchor == "" {
			return nil
		}
		return []string{anchor}
	}
	out := make([]string, 0, len(t.Ownership.Projects)+1)
	seen := map[string]bool{}
	for _, p := range append([]string{anchor}, t.Ownership.Projects...) {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// CrossProject reports whether this responsibility spans more than one checkout.
func (t Task) CrossProject() bool { return len(t.Targets()) > 1 }

// Trigger is one way a task becomes eligible to run. Exactly one kind may be
// set. The list is plural from day one so adding a second kind later is not a
// schema migration; only the time forms and manual are implemented today.
type Trigger struct {
	Cron  string `yaml:"cron,omitempty" json:"cron,omitempty"`
	Every string `yaml:"every,omitempty" json:"every,omitempty"`
	At    string `yaml:"at,omitempty" json:"at,omitempty"`
	// TZ evaluates Cron in a named zone ("America/Los_Angeles"); empty = local.
	TZ string `yaml:"tz,omitempty" json:"tz,omitempty"`
	// Missed decides what happens when the daemon was not running at the moment
	// this trigger was due. Meaningless for Manual.
	Missed Missed `yaml:"missed,omitempty" json:"missed,omitempty"`
	// MaxCatchUp bounds how many missed occurrences catch_up will actually run.
	// Without a bound, a laptop returning after months with an hourly task would
	// enqueue thousands of historical runs at once. Ignored unless Missed is
	// catch_up; the excess is dropped newest-first-kept and surfaced on the run.
	MaxCatchUp int `yaml:"max_catch_up,omitempty" json:"max_catch_up,omitempty"`
	// Manual is an explicit "this trigger only fires by hand". A task with no
	// triggers at all is already manual-only; this exists so a file can say so.
	Manual bool `yaml:"manual,omitempty" json:"manual,omitempty"`

	// Reserved kinds. Declared so an author who writes one gets a clear "not
	// implemented yet" instead of an unknown-field parse error, and so the
	// schema slot is taken.
	OnPush  *OnPush  `yaml:"on_push,omitempty" json:"on_push,omitempty"`
	Webhook *Webhook `yaml:"webhook,omitempty" json:"webhook,omitempty"`
}

// OnPush and Webhook are reserved trigger kinds, rejected by Validate today.
type OnPush struct {
	Branch string `yaml:"branch,omitempty" json:"branch,omitempty"`
}
type Webhook struct {
	Secret string `yaml:"secret,omitempty" json:"secret,omitempty"`
}

// Missed is the catch-up policy for a trigger the daemon slept through. This
// matters more than it looks on a laptop: without it, "0 2 * * *" silently
// means "only if the machine happened to be awake at 2am", and a weekly job can
// go months without running while appearing healthy.
type Missed string

const (
	// MissedRunOnce runs the task once on the next daemon start, then resumes
	// the normal cadence. The default, and the right answer for maintenance.
	MissedRunOnce Missed = "run_once"
	// MissedSkip forgets the occurrence entirely (the old schedule behaviour).
	MissedSkip Missed = "skip"
	// MissedCatchUp replays missed occurrences, up to Trigger.MaxCatchUp. Rarely
	// what anyone wants.
	MissedCatchUp Missed = "catch_up"
)

// DefaultMaxCatchUp bounds catch_up when a trigger does not say. HardMaxCatchUp
// bounds what a trigger may ASK for: a YAML value is a ceiling request, not an
// override, and nothing should be able to schedule a thousand-run stampede.
const (
	DefaultMaxCatchUp = 10
	HardMaxCatchUp    = 100
)

// Mode is how the task's work gets done.
type Mode string

const (
	// ModeAgent reasons through the whole task every run.
	ModeAgent Mode = "agent"
	// ModeProcedure runs a learned deterministic procedure and nothing else.
	ModeProcedure Mode = "procedure"
	// ModeHybrid runs the procedure first and escalates to the agent only when
	// the procedure reports there is something to think about. This is the
	// difference between a 40k-token Monday and a free one.
	ModeHybrid Mode = "hybrid"
)

// Execution selects the work strategy. Procedure names an entry in the repo's
// procedure store; the concept is deliberately not "shell script", because a
// procedure may later be an HTTP call, a tool sequence or a Go helper.
type Execution struct {
	Mode      Mode   `yaml:"mode,omitempty" json:"mode,omitempty"`
	Procedure string `yaml:"procedure,omitempty" json:"procedure,omitempty"`
	// KnownGood records an approach that WORKED once, with the date it worked.
	// It is a hint and never the definition: a repository drifts, and a run
	// that treats last quarter's steps as authoritative will confidently do the
	// wrong thing. A run starts here and checks whether the assumptions still
	// hold before relying on any of it.
	KnownGood string `yaml:"known_good,omitempty" json:"known_good,omitempty"`
	// EscalateWhen gates the agent half of a hybrid run.
	EscalateWhen string `yaml:"escalate_when,omitempty" json:"escalate_when,omitempty"`
}

// EscalateOnChanges is the only escalation condition implemented today.
const EscalateOnChanges = "procedure_reports_changes"

// Runtime is where the task's inference runs. It is four separate decisions
// rather than one string, because "use my Claude subscription" quietly bundles
// availability, permission, capability and what-to-do-when-it-breaks, and those
// have different answers and different owners.
//
//	Strategy  how to choose
//	Allowed   the user's ordered preference — order is obeyed, not optimized
//	Model     a pinned catalog model, or auto
//	Fallback  what may be tried when the RUNTIME fails (not when the task does)
//
// A runtime being present on the machine authorizes nothing. See
// internal/runtimes.
type Runtime struct {
	Strategy string   `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	Allowed  []string `yaml:"allowed,omitempty" json:"allowed,omitempty"`
	Model    string   `yaml:"model,omitempty" json:"model,omitempty"`
	Fallback []string `yaml:"fallback,omitempty" json:"fallback,omitempty"`
}

// ModelAuto means the task expressed no model preference.
const ModelAuto = "auto"

// Autonomy is the task's authority CEILING. Level is a friendly preset that
// expands to concrete grants; Grants adds named capabilities on top. The policy
// engine only ever reasons about grants.
//
// A ceiling is not an override. Nothing written here can lift a task above
// memcode's hard floor — permissions.Decide still returns NeedPrompt for a
// catastrophic command in every mode, and an unknown grant is refused at parse
// time rather than interpreted generously.
type Autonomy struct {
	Level  Level   `yaml:"level,omitempty" json:"level,omitempty"`
	Grants []Grant `yaml:"grants,omitempty" json:"grants,omitempty"`
}

// PRMode decides whether a task that changed code opens a pull request.
type PRMode string

const (
	PRNever PRMode = "never"
	// PRWhenChanges opens a pull request when the run produced a commit.
	PRWhenChanges PRMode = "when_changes"
	// PRAlways means always FOR A PRODUCED COMMIT — never "manufacture an empty
	// one to satisfy the configuration". A run with no diff opens nothing under
	// either setting; the difference between them is reserved for future
	// conditions on an actual change, not for inventing artifacts.
	PRAlways PRMode = "always"
)

// Git controls how code changes leave an unattended run. Worktree isolation is
// the default because an autonomous run must never disturb the branch a human
// is sitting on.
type Git struct {
	Worktree    *bool  `yaml:"worktree,omitempty" json:"worktree,omitempty"`
	PullRequest PRMode `yaml:"pull_request,omitempty" json:"pull_request,omitempty"`
	Branch      string `yaml:"branch,omitempty" json:"branch,omitempty"`
	// Remote is where a branch is published. Default origin.
	Remote string `yaml:"remote,omitempty" json:"remote,omitempty"`
}

// DefaultBranchPattern names the branch an autonomous run pushes. {name} and
// {date} are substituted; the auto/ prefix makes provenance obvious in a branch
// list.
const DefaultBranchPattern = "auto/{name}-{date}"

// Verify is how a run proves it succeeded. Without it "the agent stopped" gets
// mistaken for "the task worked", which is how autonomous systems quietly rot.
type Verify struct {
	Commands []string `yaml:"commands,omitempty" json:"commands,omitempty"`

	// Across are checks that run ONCE, after every project has done its share,
	// to test that the projects still agree with each other. Per-project checks
	// cannot answer that: each side can be internally perfect and the pair
	// still inconsistent, which on a cross-cutting change is precisely the
	// failure worth catching. They run in the anchor project's working copy
	// with MEMCODE_TASK_PROJECTS listing every project's working copy.
	Across []string `yaml:"across,omitempty" json:"across,omitempty"`
}

// Notify says when an optional sink fires.
type Notify string

const (
	NotifyNever    Notify = "never"
	NotifyFailures Notify = "failures"
	NotifyChanges  Notify = "changes"
	NotifyAlways   Notify = "always"
)

// Delivery is where results go. The inbox is the durable ledger and is always
// on; everything else is notification layered over it.
type Delivery struct {
	Desktop Notify `yaml:"desktop,omitempty" json:"desktop,omitempty"`
	// Channel reuses the gateway's existing "channel:conversation" address when
	// the user has one paired. Empty means inbox only.
	Channel   string `yaml:"channel,omitempty" json:"channel,omitempty"`
	ChannelOn Notify `yaml:"channel_on,omitempty" json:"channel_on,omitempty"`
}

// Limits bound a single run. An unattended task that can spin forever is a
// bill, not a feature.
type Limits struct {
	Timeout    string  `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MaxCostUSD float64 `yaml:"max_cost_usd,omitempty" json:"max_cost_usd,omitempty"`
}

// DefaultTimeout bounds a run that does not set its own.
const DefaultTimeout = "45m"

// ConcurrencyPolicy decides what happens when a task is due while an
// overlapping run is still going.
type ConcurrencyPolicy string

const (
	// ConcurrencyQueue waits for the in-flight run. The default: two tasks
	// rewriting the same repo at once is a merge conflict with extra steps.
	ConcurrencyQueue ConcurrencyPolicy = "queue"
	// ConcurrencySkip drops this occurrence.
	ConcurrencySkip ConcurrencyPolicy = "skip"
)

// Concurrency serializes runs that share a key. Key "project" (the default)
// resolves to the task's project root, so every mutating task in a repo takes
// the same lock while read-only tasks run freely.
type Concurrency struct {
	Key    string            `yaml:"key,omitempty" json:"key,omitempty"`
	Policy ConcurrencyPolicy `yaml:"policy,omitempty" json:"policy,omitempty"`
}

// ConcurrencyKeyProject is the default key: one mutating run per repo.
const ConcurrencyKeyProject = "project"

// IsEnabled reports whether the task may run. Absent means enabled — a file
// someone wrote is meant to work.
func (t Task) IsEnabled() bool { return t.Enabled == nil || *t.Enabled }

// UsesWorktree reports whether runs get an isolated worktree. Absent means yes.
func (t Task) UsesWorktree() bool { return t.Git.Worktree == nil || *t.Git.Worktree }

// Manual reports whether the task only ever runs by hand. A task with no
// triggers is manual — that is a normal, complete task, not an unfinished one.
func (t Task) Manual() bool {
	for _, tr := range t.Triggers {
		if !tr.Manual {
			return false
		}
	}
	return true
}

// ReadOnly reports whether this task may change anything at all.
func (t Task) ReadOnly() bool { return t.Autonomy.Level == LevelReadOnly }

// Timeout resolves the run bound.
func (t Task) Timeout() time.Duration {
	d, err := time.ParseDuration(t.Limits.Timeout)
	if err != nil || d <= 0 {
		d, _ = time.ParseDuration(DefaultTimeout)
	}
	return d
}

// ApplyDefaults fills every unset field with its documented default. It runs
// before validation and before hashing, so a sparse file and the fully-written
// equivalent are the same task with the same Revision.
func (t *Task) ApplyDefaults() {
	if t.Version == 0 {
		t.Version = Version
	}
	t.Name = strings.TrimSpace(t.Name)
	t.Instructions = strings.TrimSpace(t.Instructions)
	if t.Enabled == nil {
		on := true
		t.Enabled = &on
	}
	if t.Execution.Mode == "" {
		t.Execution.Mode = ModeAgent
	}
	// A cross-project task must state its publication semantics, because the
	// question only exists once there is more than one project. Defaulting to
	// independent is the conservative reading of an unstated intent: it never
	// withholds work that stands on its own, and a responsibility that truly
	// needs all-or-nothing has to say so.
	if t.Ownership.Coordination == "" && len(t.Ownership.Projects) > 0 {
		t.Ownership.Coordination = CoordIndependent
	}
	if t.Execution.Mode == ModeHybrid && t.Execution.EscalateWhen == "" {
		t.Execution.EscalateWhen = EscalateOnChanges
	}
	if t.Runtime.Strategy == "" {
		t.Runtime.Strategy = string(runtimes.StrategyPreferAuthorized)
	}
	if len(t.Runtime.Allowed) == 0 && t.Runtime.Strategy != string(runtimes.StrategyExplicit) {
		// Default preference: every subscription runtime, in registry order, then
		// the hosted gateway. Nothing here is usable without an explicit
		// authorization, so a default that NAMES subscriptions does not grant
		// any — it only says which ones the user would be asked about.
		//
		// NOT filled for the explicit strategy. Defaulting there would make
		// "require exactly this runtime" silently mean "require whatever happens
		// to be first in the registry", which is the opposite of pinning one.
		// Validation refuses the empty list instead.
		for _, r := range runtimes.All() {
			t.Runtime.Allowed = append(t.Runtime.Allowed, r.ID)
		}
	}
	if t.Runtime.Model == "" {
		t.Runtime.Model = ModelAuto
	}
	if len(t.Runtime.Fallback) == 0 {
		t.Runtime.Fallback = []string{runtimes.Hosted}
	}
	if t.Autonomy.Level == "" {
		t.Autonomy.Level = LevelBranch
	}
	if t.Git.Worktree == nil {
		// A read-only task changes nothing, so there is nothing to isolate.
		on := t.Autonomy.Level != LevelReadOnly
		t.Git.Worktree = &on
	}
	if t.Git.PullRequest == "" {
		// Normalize the UNSET case only. An author who explicitly asked for a PR
		// from a read-only task gets told it contradicts (see validateGit) rather
		// than having their line quietly rewritten — silently narrowing stated
		// intent is the same misleading failure as silently widening it.
		if t.Autonomy.Level == LevelReadOnly {
			t.Git.PullRequest = PRNever
		} else {
			t.Git.PullRequest = PRWhenChanges
		}
	}
	if t.Git.Branch == "" {
		t.Git.Branch = DefaultBranchPattern
	}
	if t.Git.Remote == "" {
		t.Git.Remote = "origin"
	}
	if t.Delivery.Desktop == "" {
		t.Delivery.Desktop = NotifyFailures
	}
	if t.Delivery.Channel != "" && t.Delivery.ChannelOn == "" {
		t.Delivery.ChannelOn = NotifyAlways
	}
	if t.Limits.Timeout == "" {
		t.Limits.Timeout = DefaultTimeout
	}
	if t.Concurrency.Key == "" {
		t.Concurrency.Key = ConcurrencyKeyProject
	}
	if t.Concurrency.Policy == "" {
		t.Concurrency.Policy = ConcurrencyQueue
	}
	for i := range t.Triggers {
		if t.Triggers[i].Missed == "" && !t.Triggers[i].Manual {
			t.Triggers[i].Missed = MissedRunOnce
		}
		if t.Triggers[i].Missed == MissedCatchUp && t.Triggers[i].MaxCatchUp == 0 {
			t.Triggers[i].MaxCatchUp = DefaultMaxCatchUp
		}
	}
}

// Revision is the content hash of the normalized definition: sha256 over
// canonical JSON, sorted where order carries no meaning. Two files that differ
// only in key order or in omitted-but-defaulted fields hash identically; any
// semantic edit changes the hash.
//
// Every run records this. It is what makes an execution auditable months later,
// after the YAML has moved on.
func (t Task) Revision() (string, error) {
	n := t
	n.ApplyDefaults()
	n.Path, n.Scope = "", ""
	// Grants are a set. Verify.Commands and Triggers are sequences whose order
	// the user chose, so they are left alone.
	n.Autonomy.Grants = append([]Grant(nil), n.Autonomy.Grants...)
	sort.Slice(n.Autonomy.Grants, func(i, j int) bool { return n.Autonomy.Grants[i] < n.Autonomy.Grants[j] })
	b, err := json.Marshal(n)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
