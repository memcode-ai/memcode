package task

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/runtimes"
)

// nameRe is the task name charset. Names become filenames, branch names, lock
// keys and CLI arguments, so they stay boring on purpose.
var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidName reports whether s is a usable task name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Validate checks a task after ApplyDefaults. Every failure names the field and
// says what a working value looks like, because the reader is usually someone
// hand-editing YAML with no schema in front of them.
//
// now is injectable so one-shot "at" triggers can be validated against a fixed
// clock in tests.
func (t Task) Validate(now time.Time) error {
	if t.Version != Version {
		return fmt.Errorf("version %d is not supported (this memcode understands version %d)", t.Version, Version)
	}
	if t.Name == "" {
		return fmt.Errorf("a task needs a name")
	}
	if !ValidName(t.Name) {
		return fmt.Errorf("bad name %q: use lowercase words joined by hyphens, e.g. upgrade-model-catalog", t.Name)
	}
	if t.Instructions == "" {
		return fmt.Errorf("an agent task needs instructions")
	}
	if err := t.validateExecution(); err != nil {
		return err
	}
	if err := t.validateOwnership(); err != nil {
		return err
	}
	if !ValidLevel(t.Autonomy.Level) {
		return fmt.Errorf("unknown autonomy level %q (use %s)", t.Autonomy.Level, joinLevels(Levels()))
	}
	// Expanding resolves the level and refuses unknown grants. Doing it here
	// means a typo fails at load, not at 3am with nobody watching.
	if _, err := t.Grants(); err != nil {
		return err
	}
	if err := t.validateRuntime(); err != nil {
		return err
	}
	if err := t.validateGit(); err != nil {
		return err
	}
	if err := t.validateDelivery(); err != nil {
		return err
	}
	if err := t.validateLimits(); err != nil {
		return err
	}
	if t.Concurrency.Policy != ConcurrencyQueue && t.Concurrency.Policy != ConcurrencySkip {
		return fmt.Errorf("unknown concurrency policy %q (use queue or skip)", t.Concurrency.Policy)
	}
	seen := map[string]bool{}
	for i, tr := range t.Triggers {
		if err := tr.Validate(now); err != nil {
			return fmt.Errorf("trigger %d: %w", i+1, err)
		}
		// Two identical cadences on one task is always a mistake and would
		// double every run.
		key := tr.key()
		if seen[key] {
			return fmt.Errorf("trigger %d duplicates an earlier trigger (%s)", i+1, key)
		}
		seen[key] = true
	}
	return nil
}

// validateOwnership refuses a cross-project task that has not said how its
// projects relate, or that names a project by a path a scheduler cannot resolve.
func (t Task) validateOwnership() error {
	for _, p := range t.Ownership.Projects {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("ownership.projects: %q must be an absolute path — an unattended run "+
				"has no working directory to resolve it against", p)
		}
	}
	switch t.Ownership.Coordination {
	case "", CoordIndependent, CoordCoordinated:
	default:
		return fmt.Errorf("unknown ownership.coordination %q (use independent or coordinated)",
			t.Ownership.Coordination)
	}
	// Coordination on a single-project task is not a harmless extra key: it
	// says a decision was made about projects that are not there, which usually
	// means the project list was lost somewhere.
	if len(t.Ownership.Projects) == 0 && t.Ownership.Coordination != "" {
		return fmt.Errorf("ownership.coordination is set but ownership.projects is empty — " +
			"coordination only means something across more than one project")
	}
	return nil
}

// validateExecution checks the mechanical half.
//
// There is no execution MODE. A task has instructions, and optionally some
// commands that need no judgement; "run this, then work out what it means" is
// an ordinary instruction, not a third kind of task. An enum over agent /
// procedure / hybrid only created states that had to be kept consistent with
// the fields they described.
func (t Task) validateExecution() error {
	for i, step := range t.Execution.Steps {
		if strings.TrimSpace(step) == "" {
			return fmt.Errorf("execution.steps[%d] is empty", i)
		}
	}
	return nil
}

func (t Task) validateRuntime() error {
	if !runtimes.ValidStrategy(runtimes.Strategy(t.Runtime.Strategy)) {
		var names []string
		for _, s := range runtimes.Strategies() {
			names = append(names, string(s))
		}
		return fmt.Errorf("unknown runtime.strategy %q (use %s)", t.Runtime.Strategy, strings.Join(names, ", "))
	}
	for _, id := range append(append([]string{}, t.Runtime.Allowed...), t.Runtime.Fallback...) {
		if _, ok := runtimes.Get(id); !ok {
			return fmt.Errorf("unknown runtime %q (known: %s)", id, strings.Join(runtimes.IDs(), ", "))
		}
	}
	if runtimes.Strategy(t.Runtime.Strategy) == runtimes.StrategyExplicit && len(t.Runtime.Allowed) == 0 {
		return fmt.Errorf("runtime.strategy explicit needs runtime.allowed to name the required runtime")
	}
	return nil
}

func (t Task) validateGit() error {
	switch t.Git.PullRequest {
	case PRNever, PRWhenChanges, PRAlways:
	default:
		return fmt.Errorf("unknown git.pull_request %q (use never, when_changes or always)", t.Git.PullRequest)
	}
	if t.Git.PullRequest != PRNever && !t.MayOpenPR() {
		return fmt.Errorf("git.pull_request is %q but autonomy level %q cannot push a branch or open a PR",
			t.Git.PullRequest, t.Autonomy.Level)
	}
	if strings.TrimSpace(t.Git.Branch) == "" {
		return fmt.Errorf("git.branch cannot be empty")
	}
	return nil
}

func (t Task) validateDelivery() error {
	if !validNotify(t.Delivery.Desktop) {
		return fmt.Errorf("unknown delivery.desktop %q (use never, failures, changes or always)", t.Delivery.Desktop)
	}
	if t.Delivery.Channel != "" {
		if err := gwconfig.ValidateDeliverTo(t.Delivery.Channel); err != nil {
			return fmt.Errorf("delivery.channel: %w", err)
		}
		if !validNotify(t.Delivery.ChannelOn) {
			return fmt.Errorf("unknown delivery.channel_on %q (use never, failures, changes or always)", t.Delivery.ChannelOn)
		}
	}
	return nil
}

func validNotify(n Notify) bool {
	switch n {
	case NotifyNever, NotifyFailures, NotifyChanges, NotifyAlways:
		return true
	}
	return false
}

func (t Task) validateLimits() error {
	d, err := time.ParseDuration(t.Limits.Timeout)
	if err != nil {
		return fmt.Errorf("bad limits.timeout %q: %w (a Go duration like 45m or 2h)", t.Limits.Timeout, err)
	}
	if d <= 0 {
		return fmt.Errorf("limits.timeout must be positive")
	}
	if t.Limits.MaxCostUSD < 0 {
		return fmt.Errorf("limits.max_cost_usd cannot be negative")
	}
	return nil
}

// Validate checks one trigger. The time forms are delegated to the gateway's
// shared spec validation rather than reparsed here: one validator means a
// cadence that parses on this surface parses on the scheduler, and a second
// parser is exactly how the previous two schedulers drifted apart.
func (tr Trigger) Validate(now time.Time) error {
	if tr.OnPush != nil {
		return fmt.Errorf("on_push triggers are not implemented yet")
	}
	if tr.Webhook != nil {
		return fmt.Errorf("webhook triggers are not implemented yet")
	}
	timed := strings.TrimSpace(tr.Cron) != "" || strings.TrimSpace(tr.Every) != "" || strings.TrimSpace(tr.At) != ""
	if tr.Manual {
		if timed {
			return fmt.Errorf("a manual trigger cannot also set cron, every or at")
		}
		return nil
	}
	if !timed {
		return fmt.Errorf("set cron, every or at — or manual: true")
	}
	if _, err := gwconfig.ValidateScheduleSpec(tr.Cron, tr.Every, tr.At, now); err != nil {
		return err
	}
	if tr.TZ != "" {
		if _, err := time.LoadLocation(tr.TZ); err != nil {
			return fmt.Errorf("bad tz %q: %w", tr.TZ, err)
		}
		if strings.TrimSpace(tr.Cron) == "" {
			return fmt.Errorf("tz only applies to a cron trigger")
		}
	}
	switch tr.Missed {
	case MissedRunOnce, MissedSkip, MissedCatchUp:
	default:
		return fmt.Errorf("unknown missed %q (use run_once, skip or catch_up)", tr.Missed)
	}
	if strings.TrimSpace(tr.At) != "" && tr.Missed == MissedCatchUp {
		return fmt.Errorf("catch_up is meaningless for a one-shot at trigger")
	}
	if tr.MaxCatchUp != 0 {
		if tr.Missed != MissedCatchUp {
			return fmt.Errorf("max_catch_up only applies to missed: catch_up")
		}
		if tr.MaxCatchUp < 0 {
			return fmt.Errorf("max_catch_up must be positive")
		}
		if tr.MaxCatchUp > HardMaxCatchUp {
			return fmt.Errorf("max_catch_up %d exceeds the hard limit of %d — a backlog that "+
				"large is a stampede, not a catch-up", tr.MaxCatchUp, HardMaxCatchUp)
		}
	}
	return nil
}

// key identifies a trigger's cadence for duplicate detection.
func (tr Trigger) key() string {
	switch {
	case tr.Manual:
		return "manual"
	case strings.TrimSpace(tr.Cron) != "":
		return "cron:" + tr.Cron + "/" + tr.TZ
	case strings.TrimSpace(tr.Every) != "":
		return "every:" + tr.Every
	default:
		return "at:" + tr.At
	}
}
