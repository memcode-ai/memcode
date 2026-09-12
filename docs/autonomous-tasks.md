# Autonomous tasks

A task is a standing responsibility memcode carries out on its own: keep
dependencies current, keep the model catalog accurate, keep security advisories
triaged. You approve it once; after that it runs on a schedule with nobody
watching and puts the result in front of you.

This is the reference. For what the feature is and why you would want it, see
the README.

## Creating one

Ask for the outcome rather than the job:

```
keep this project's dependencies up to date
```

memcode does the work now, then offers to keep doing it. Accepting runs a short
design phase: it validates the automation against your repo, then shows you the
contract — what it will do, how it knows it worked, what it may change, and
what was just proved. Nothing is written until you say yes.

If the work was already done in that conversation, that run is the proof and
the job is not repeated.

To change one later, just say so: "make that monthly", "only the CLI repo",
"use the v3 migration path". Editing the YAML by hand works too.

## Where they live

```
<repo>/.memcode/tasks/<name>.yaml     this repository's own responsibilities
~/.config/memcode/tasks/<name>.yaml   responsibilities no single repo owns
```

Project tasks are meant to be reviewed and committed; memcode's gitignore is
written to let them through. A project task wins over a global one with the
same name.

A responsibility spanning several repositories is stored globally, because
putting it inside one of them would give that repo an ownership it does not
have.

## The file

```yaml
version: 1
name: dependency-updates
description: Keep dependencies current
enabled: true

triggers:
  - every: 168h            # or `cron: "0 10 * * MON"` with an optional `tz:`
    missed: run_once       # skip | run_once | catch_up

instructions: |
  Work out which modules have newer stable releases and upgrade them.
  Adapt the code where an upgrade breaks it.

execution:
  steps:                   # optional: the mechanical part, run without a model
    - go get -u ./...
    - go mod tidy

autonomy:
  level: branch            # read_only | branch

git:
  pull_request: when_changes   # never | when_changes | always

verify:
  commands: ["go build ./...", "go test ./..."]

limits: { timeout: 45m }
```

### Triggers

Zero triggers is valid: the task exists and you run it by hand. `missed`
decides what happens when the machine was asleep at the appointed time —
`skip` forgets it, `run_once` runs the missed occurrence once, `catch_up` runs
every one it owes you.

### Authority

`read_only` can read and run read-only commands, and nothing else. `branch` can
also change files, commit, push a branch of its own and open a pull request. It
can never touch your default branch, force-push, or merge.

Nothing in the file raises memcode's own floor. A destructive action still
needs a person, and with nobody watching that means it is refused.

### Steps

Commands that need no judgement, worked out when the task was set up. A run
does these directly and pays no model for them. If they do the whole job and
nothing changed, no agent starts at all.

They are an optimisation, never the definition. A step that fails means
something moved, so the agent takes over from there. Nothing here can make a
run succeed — only `verify.commands` can.

### Cross-repo responsibilities

```yaml
ownership:
  projects:
    - /Users/you/src/product-cli
    - /Users/you/src/product-web
  coordination: coordinated    # independent | coordinated
  responsibility: keeping model support consistent across the product

verify:
  across: ["./scripts/check-consistency.sh"]
```

`independent` publishes each project on its own merits. `coordinated` publishes
all of them or none: nothing is pushed anywhere until every project has passed.

Past that point there is no transaction across git remotes. If a push lands in
one repo and fails in another, the run says so plainly and keeps every commit;
re-running publishes only what is still missing.

`verify.across` runs once at the end, in the first project's working copy, with
`MEMCODE_TASK_PROJECTS` listing every project's working copy.

The project list is fixed when you approve it. A run adapts freely inside those
repos, but if the work appears to have moved into one that is not listed, it
changes nothing there and asks you.

## How a run behaves

Every run works in its own checkout, never your working tree. It re-derives the
current state rather than replaying the steps that worked when the task was
written, then verifies with your own commands. Outcomes are recorded
separately: whether the agent finished, whether verification passed, and what
the run actually achieved.

Only verified changes are published. A failure keeps its worktree, and the run
record says where.

## When it stops

A task suspends itself when carrying on would mean inventing intent it does not
have, taking authority it was not given, or making a consequential choice for
you — two incompatible migration paths, a check that can no longer be
evaluated, work that moved outside its repos. Those do not clear on their own,
and failing identically every week buries the decision.

Paused tasks are raised the next time you talk to memcode, and:

```
memcode task paused              what stopped, and why
memcode task resume <name>       let it run again
```

Answering in conversation is usually enough: say what you decided and memcode
revises the task, validates the revision, and resumes it — but only if the
revision can still be shown to work. A repair is a new revision, and the run
that failed keeps the definition it actually executed.

## Commands

```
memcode task list                every task, project and global
memcode task show <name>         one task in full
memcode task check               validate the files without running anything
memcode task run <name>          run it now
memcode task history [name]      past runs
memcode task inbox               runs you have not looked at
memcode task ack <run-id>        mark one dealt with
memcode task show-run <run-id>   one run in full
memcode task paused              tasks that stopped and need a decision
memcode task resume <name>       lift a suspension
memcode task suggestions         work memcode has noticed you repeat
```

Scheduled runs need the daemon: `memcode gateway install`.
