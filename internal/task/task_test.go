package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

// isolate points both task roots at temp dirs so tests never read or write the
// developer's real ~/.config/memcode.
func isolate(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	return root
}

func parse(t *testing.T, yaml string) (Task, error) {
	t.Helper()
	return Parse([]byte(yaml), "test.yaml", ScopeProject, now)
}

func mustParse(t *testing.T, yaml string) Task {
	t.Helper()
	tk, err := parse(t, yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return tk
}

const minimal = `
version: 1
name: upgrade-model-catalog
instructions: Check each provider for model changes.
`

// A task with NO triggers is complete and normal — it is a runnable object
// first and a scheduled one only if someone attaches a cadence.
func TestTaskWithoutTriggersIsValid(t *testing.T) {
	tk := mustParse(t, minimal)
	if len(tk.Triggers) != 0 {
		t.Fatalf("expected no triggers, got %d", len(tk.Triggers))
	}
	if !tk.Manual() {
		t.Error("a task with no triggers must report Manual()")
	}
	if !tk.IsEnabled() {
		t.Error("absent enabled must mean enabled")
	}
}

func TestDefaults(t *testing.T) {
	tk := mustParse(t, minimal)
	for _, c := range []struct{ got, want string }{
		{tk.Runtime.Strategy, "prefer_authorized_subscription"},
		{tk.Runtime.Model, ModelAuto},
		{string(tk.Autonomy.Level), string(LevelBranch)},
		{string(tk.Git.PullRequest), string(PRWhenChanges)},
		{tk.Git.Branch, DefaultBranchPattern},
		{tk.Limits.Timeout, DefaultTimeout},
		{tk.Concurrency.Key, ConcurrencyKeyProject},
		{string(tk.Concurrency.Policy), string(ConcurrencyQueue)},
		{string(tk.Delivery.Desktop), string(NotifyFailures)},
	} {
		if c.got != c.want {
			t.Errorf("default = %q, want %q", c.got, c.want)
		}
	}
	if !tk.UsesWorktree() {
		t.Error("worktree isolation must default on")
	}
}

func TestPluralTriggers(t *testing.T) {
	tk := mustParse(t, `
version: 1
name: two-cadences
instructions: do the thing
triggers:
  - cron: "0 10 * * MON"
    tz: America/Los_Angeles
  - every: 6h
`)
	if len(tk.Triggers) != 2 {
		t.Fatalf("got %d triggers, want 2", len(tk.Triggers))
	}
	if tk.Triggers[0].Missed != MissedRunOnce {
		t.Errorf("missed default = %q, want run_once", tk.Triggers[0].Missed)
	}
	if tk.Manual() {
		t.Error("a task with timed triggers is not manual")
	}
}

func TestTriggerValidation(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"no kind", "triggers:\n  - tz: UTC\n", "set cron, every or at"},
		{"two kinds", "triggers:\n  - cron: \"0 9 * * *\"\n    every: 1h\n", "exactly one"},
		{"bad cron", "triggers:\n  - cron: \"not a cron\"\n", "bad cron"},
		{"bad every", "triggers:\n  - every: forever\n", "bad every"},
		{"bad tz", "triggers:\n  - cron: \"0 9 * * *\"\n    tz: Mars/Olympus\n", "bad tz"},
		{"tz without cron", "triggers:\n  - every: 1h\n    tz: UTC\n", "tz only applies"},
		{"manual plus cron", "triggers:\n  - manual: true\n    cron: \"0 9 * * *\"\n", "cannot also set"},
		{"bad missed", "triggers:\n  - every: 1h\n    missed: whenever\n", "unknown missed"},
		{"catch_up on one-shot", "triggers:\n  - at: 30m\n    missed: catch_up\n", "meaningless"},
		{"duplicate cadence", "triggers:\n  - every: 1h\n  - every: 1h\n", "duplicates"},
		{"reserved on_push", "triggers:\n  - on_push:\n      branch: main\n", "not implemented yet"},
		{"reserved webhook", "triggers:\n  - webhook:\n      secret: s\n", "not implemented yet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse(t, minimal+c.yaml)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

// An unknown key must fail loudly. A typo'd `autonmy:` that parses cleanly
// would hand a task the DEFAULT authority while its author believed they had
// restricted it — the worst possible direction for a silent failure.
func TestStrictDecodingRejectsTypos(t *testing.T) {
	_, err := parse(t, minimal+"autonmy:\n  level: read_only\n")
	if err == nil {
		t.Fatal("a misspelled top-level key must be an error")
	}
}

func TestUnknownLevelAndGrantRejected(t *testing.T) {
	if _, err := parse(t, minimal+"autonomy:\n  level: god_mode\n"); err == nil ||
		!strings.Contains(err.Error(), "unknown autonomy level") {
		t.Errorf("god_mode must be refused, got %v", err)
	}
	if _, err := parse(t, minimal+"autonomy:\n  grants: [\"prod.deploy\"]\n"); err == nil ||
		!strings.Contains(err.Error(), "unknown grant") {
		t.Errorf("an invented grant must be refused, got %v", err)
	}
}

// The authority ladder. read_only prepares nothing; branch prepares a change
// for review and stops there.
func TestGrantLadder(t *testing.T) {
	ro := mustParse(t, minimal+"autonomy:\n  level: read_only\n")
	if ro.MayMutate() {
		t.Error("read_only must not mutate the filesystem")
	}
	if ro.MayOpenPR() {
		t.Error("read_only must not open a PR")
	}
	if ro.Git.PullRequest != PRNever {
		t.Errorf("read_only must force pull_request to never, got %q", ro.Git.PullRequest)
	}

	br := mustParse(t, minimal)
	for _, g := range []Grant{GrantFilesystemMutate, GrantProcessExec, GrantGitCreateBranch,
		GrantGitCommit, GrantGitPushBranch, GrantGitHubOpenPR} {
		if !br.HasGrant(g) {
			t.Errorf("branch tier must grant %s", g)
		}
	}
	if !br.MayOpenPR() {
		t.Error("branch tier must be able to open a PR")
	}
}

// There is deliberately NO grant spelling for the things that would make
// autonomous work authoritative. This is the ceiling-is-not-an-override
// invariant at the schema level: you cannot ask for these, however you write
// the YAML.
func TestNoGrantForAuthoritativeActions(t *testing.T) {
	for _, forbidden := range []string{
		"git.force_push", "git.push_main", "git.merge", "github.merge_pr",
		"deploy", "prod.deploy", "secrets.rotate", "package.publish", "dns.update",
	} {
		if _, err := ExpandGrants(LevelBranch, []Grant{Grant(forbidden)}); err == nil {
			t.Errorf("%q must not be a grantable capability", forbidden)
		}
	}
	// And no shipped tier quietly includes one.
	for _, lvl := range Levels() {
		grants, err := ExpandGrants(lvl, nil)
		if err != nil {
			t.Fatalf("ExpandGrants(%s): %v", lvl, err)
		}
		for _, g := range grants {
			if !knownGrants[g] {
				t.Errorf("tier %s expands to unlisted grant %q", lvl, g)
			}
		}
	}
}

// A PR request the autonomy level cannot honour is a contradiction, and must
// fail at load rather than at 3am with nobody watching.
func TestPRBeyondAutonomyRejected(t *testing.T) {
	_, err := parse(t, minimal+"autonomy:\n  level: read_only\ngit:\n  pull_request: always\n")
	if err == nil || !strings.Contains(err.Error(), "cannot push a branch") {
		t.Errorf("err = %v, want a pull_request/autonomy contradiction", err)
	}
}

func TestExecutionValidation(t *testing.T) {
	// The mode enum is gone: a task has instructions, and optionally commands
	// that need no judgement. "Run this, then work out what it means" is an
	// ordinary instruction, not a third kind of task.
	//
	// A file written against the old schema must fail LOUDLY rather than load
	// with its execution settings silently ignored.
	for _, c := range []struct{ name, yaml, wantErr string }{
		{"old mode key", "execution:\n  mode: hybrid\n", "field mode not found"},
		{"old procedure key", "execution:\n  procedure: check-models\n", "field procedure not found"},
		{"empty step", "execution:\n  steps:\n    - \"\"\n", "is empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse(t, minimal+c.yaml)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
	tk := mustParse(t, minimal+"execution:\n  steps:\n    - go get -u ./...\n    - go mod tidy\n")
	if len(tk.Execution.Steps) != 2 {
		t.Errorf("steps = %v, want both", tk.Execution.Steps)
	}
}

func TestBadNameRejected(t *testing.T) {
	for _, bad := range []string{"Upgrade Models", "upgrade_models", "-leading", "trailing-", "UPPER"} {
		if ValidName(bad) {
			t.Errorf("%q must not be a valid task name", bad)
		}
	}
	if !ValidName("upgrade-model-catalog") {
		t.Error("a normal hyphenated name must be valid")
	}
}

// The revision hash is what makes a run auditable months later. It must ignore
// presentation and catch meaning.
func TestRevisionStableUnderReordering(t *testing.T) {
	a := mustParse(t, `
version: 1
name: rev-check
instructions: do the thing
autonomy:
  level: branch
  grants: ["git.commit", "filesystem.read"]
limits:
  timeout: 30m
`)
	b := mustParse(t, `
version: 1
limits:
  timeout: 30m
autonomy:
  grants: ["filesystem.read", "git.commit"]
  level: branch
instructions: do the thing
name: rev-check
`)
	ra, err := a.Revision()
	if err != nil {
		t.Fatal(err)
	}
	rb, err := b.Revision()
	if err != nil {
		t.Fatal(err)
	}
	if ra != rb {
		t.Errorf("key order and grant order must not change the revision:\n %s\n %s", ra, rb)
	}
	if !strings.HasPrefix(ra, "sha256:") {
		t.Errorf("revision = %q, want a sha256: prefix", ra)
	}

	// A sparse file and its fully-defaulted equivalent are the same task.
	sparse := mustParse(t, minimal)
	full := sparse
	full.ApplyDefaults()
	rs, _ := sparse.Revision()
	rf, _ := full.Revision()
	if rs != rf {
		t.Error("applying defaults must not change the revision")
	}
}

func TestRevisionChangesOnSemanticEdit(t *testing.T) {
	base := mustParse(t, minimal)
	baseRev, _ := base.Revision()

	edits := map[string]func(*Task){
		"instructions": func(t *Task) { t.Instructions = "something else" },
		"autonomy":     func(t *Task) { t.Autonomy.Level = LevelReadOnly },
		"grant added":  func(t *Task) { t.Autonomy.Grants = []Grant{GrantGitCommit} },
		"runtime":      func(t *Task) { t.Runtime.Strategy = "hosted_only" },
		"runtime order": func(t *Task) {
			t.Runtime.Allowed = []string{"codex", "memcode-hosted"}
		},
		"trigger": func(t *Task) { t.Triggers = []Trigger{{Every: "1h", Missed: MissedRunOnce}} },
		"verify":  func(t *Task) { t.Verify.Commands = []string{"go test ./..."} },
		"timeout": func(t *Task) { t.Limits.Timeout = "2h" },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			mod := base
			edit(&mod)
			rev, err := mod.Revision()
			if err != nil {
				t.Fatal(err)
			}
			if rev == baseRev {
				t.Errorf("editing %s must change the revision", name)
			}
		})
	}

	// Where a task was loaded from is not part of what it does.
	moved := base
	moved.Path, moved.Scope = "/somewhere/else.yaml", ScopeGlobal
	if rev, _ := moved.Revision(); rev != baseRev {
		t.Error("path and scope must not affect the revision")
	}
}

// Project scope shadows global: a repo that ships a task means it for that repo.
func TestProjectScopeWinsOverGlobal(t *testing.T) {
	root := isolate(t)
	globalDir, err := GlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(globalDir, "shared", "version: 1\nname: shared\ninstructions: global version\n")
	write(globalDir, "global-only", "version: 1\nname: global-only\ninstructions: only global\n")
	write(ProjectDir(root), "shared", "version: 1\nname: shared\ninstructions: project version\n")

	tasks, errs := Load(root, now)
	if len(errs) != 0 {
		t.Fatalf("unexpected load errors: %v", errs)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2 (shared deduped)", len(tasks))
	}
	got, err := Get(root, "shared", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Instructions != "project version" {
		t.Errorf("instructions = %q, want the project copy", got.Instructions)
	}
	if got.Scope != ScopeProject {
		t.Errorf("scope = %q, want project", got.Scope)
	}
}

// One unparseable file must not take the scheduler down with it.
func TestBadFileDoesNotBlockGoodOnes(t *testing.T) {
	root := isolate(t)
	dir := ProjectDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "good.yaml"), []byte("version: 1\nname: good\ninstructions: fine\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("version: 1\nname: broken\nautonomy:\n  level: nope\n"), 0o644)

	tasks, errs := Load(root, now)
	if len(tasks) != 1 || tasks[0].Name != "good" {
		t.Errorf("good task must still load, got %+v", tasks)
	}
	if len(errs) != 1 {
		t.Errorf("broken task must be reported once, got %v", errs)
	}
}

// The filename is the addressable identity, so a disagreeing name is refused
// rather than leaving `task run <name>` pointing at a file nobody edited.
func TestFilenameMustMatchName(t *testing.T) {
	root := isolate(t)
	dir := ProjectDir(root)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "on-disk.yaml"), []byte("version: 1\nname: in-file\ninstructions: x\n"), 0o644)

	_, errs := Load(root, now)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "does not match the filename") {
		t.Errorf("errs = %v, want a filename mismatch", errs)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	root := isolate(t)
	tk := mustParse(t, minimal)
	tk.Verify.Commands = []string{"go build ./...", "go test ./..."}
	tk.Triggers = []Trigger{{Cron: "0 10 * * MON", TZ: "UTC", Missed: MissedRunOnce}}

	path, err := Save(root, tk, ScopeProject)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if want := filepath.Join(ProjectDir(root), "upgrade-model-catalog.yaml"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	back, err := Get(root, "upgrade-model-catalog", now)
	if err != nil {
		t.Fatal(err)
	}
	rev1, _ := tk.Revision()
	rev2, _ := back.Revision()
	if rev1 != rev2 {
		t.Error("a saved task must reload to the same revision")
	}
	// Command ORDER is the user's choice and must survive.
	if len(back.Verify.Commands) != 2 || back.Verify.Commands[0] != "go build ./..." {
		t.Errorf("verify commands = %v, want build before test", back.Verify.Commands)
	}
}

func TestGetNotFound(t *testing.T) {
	root := isolate(t)
	if _, err := Get(root, "nope", now); err == nil {
		t.Fatal("expected an error")
	}
}

func TestVersionMismatchRejected(t *testing.T) {
	_, err := parse(t, "version: 99\nname: future\ninstructions: x\n")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("err = %v, want an unsupported-version error", err)
	}
}

func TestTimeoutFallsBackWhenAbsurd(t *testing.T) {
	tk := mustParse(t, minimal)
	tk.Limits.Timeout = "nonsense"
	if got := tk.Timeout(); got != 45*time.Minute {
		t.Errorf("Timeout() = %v, want the 45m default", got)
	}
}

// The runtime block defaults to naming every known backend in preference order,
// which grants nothing: a named runtime is one the user could be ASKED about,
// and nothing here is usable without a recorded authorization.
func TestRuntimeDefaults(t *testing.T) {
	tk := mustParse(t, minimal)
	if tk.Runtime.Strategy != "prefer_authorized_subscription" {
		t.Errorf("strategy = %q", tk.Runtime.Strategy)
	}
	if len(tk.Runtime.Allowed) == 0 {
		t.Fatal("allowed should default to the known runtimes in order")
	}
	if tk.Runtime.Allowed[len(tk.Runtime.Allowed)-1] != "memcode-hosted" {
		t.Errorf("allowed = %v, want the hosted gateway last", tk.Runtime.Allowed)
	}
	if len(tk.Runtime.Fallback) != 1 || tk.Runtime.Fallback[0] != "memcode-hosted" {
		t.Errorf("fallback = %v, want the hosted gateway", tk.Runtime.Fallback)
	}
}

func TestRuntimeValidation(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"unknown strategy", "runtime:\n  strategy: vibes\n", "unknown runtime.strategy"},
		{"unknown runtime", "runtime:\n  allowed: [\"gpt-telepathy\"]\n", "unknown runtime"},
		{"unknown fallback", "runtime:\n  fallback: [\"nope\"]\n", "unknown runtime"},
		{"explicit with no allowed", "runtime:\n  strategy: explicit\n  allowed: []\n", "needs runtime.allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse(t, minimal+c.yaml)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

// Changing where a task runs changes what the task IS, so the revision moves.
func TestRuntimeChangesTheRevision(t *testing.T) {
	base := mustParse(t, minimal)
	baseRev, _ := base.Revision()
	for name, edit := range map[string]func(*Task){
		"strategy": func(t *Task) { t.Runtime.Strategy = "hosted_only" },
		"order":    func(t *Task) { t.Runtime.Allowed = []string{"memcode-hosted"} },
		"model":    func(t *Task) { t.Runtime.Model = "claude-sonnet-5" },
		"fallback": func(t *Task) { t.Runtime.Fallback = []string{"codex", "memcode-hosted"} },
	} {
		t.Run(name, func(t *testing.T) {
			mod := base
			edit(&mod)
			if rev, _ := mod.Revision(); rev == baseRev {
				t.Errorf("editing runtime.%s must change the revision", name)
			}
		})
	}
}
