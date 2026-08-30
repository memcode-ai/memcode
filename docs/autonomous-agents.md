# Autonomous agents

There is no separate kind of agent for this. An agent given a durable
**objective** and permission to run **autonomously** works on that objective
with nobody watching; everything else about it — its home, memory, skills,
model, toolsets — is the same agent you already had.

You set this up by talking to `memcode admin`, the same cockpit that manages
channels, projects and schedules. There are no CLI subcommands to learn.

## Two settings, deliberately separate

| Setting | Question it answers |
|---|---|
| `objective` | What is this agent for? |
| `autonomous` | May it act on that without being asked? |
| `schedules` | When does it wake? |
| policy | What may it do while working? |
| `browser` | Which browser environment may it drive? |

`objective` and `autonomous` are **separate grants**. Giving an agent a goal is
not the same act as letting it pursue that goal unsupervised, and all four
combinations are useful:

| autonomous | objective | Behaviour |
|---|---|---|
| ✓ | ✓ | Works the objective on its own schedule. |
| ✓ | ✗ | Scheduled work under governance — a recurring task that is policy-gated, journaled, and can pause to ask you something. |
| ✗ | ✓ | A goal you work on together; it wakes only when you ask (`gw_wake`). |
| ✗ | ✗ | An ordinary conversational agent. |

The second row matters: a plain scheduled agent used to run unattended with no
policy gate, no action journal, and no way to stop and ask. `autonomous: true`
is what turns those protections on, with or without an objective.

## What it looks like in config

`autonomous` never turns itself on — nothing here is implied by anything else.

```yaml
agents:
  jobhunt:
    objective: "Find backend roles at Series B-D startups and keep a shortlist"
    autonomous: true
    browser: existing_chrome     # the user's own signed-in Chrome
    toolsets: [browser]
schedules:
  - name: jobhunt-wake
    every: 6h
    agent: jobhunt               # deliver_to defaults to the agent itself
    task: "Advance the objective with one bounded step."
```

## Setting one up

Run `memcode admin` and say what you want. It gathers what the agent will need,
proposes the whole thing in plain language — resources, policy, whether it runs
unattended, its cadence — and builds it once you approve. The tools it uses:

| Tool | For |
|---|---|
| `gw_agent` | create; set objective, autonomous, browser; pause/resume; model, reasoning, toolsets |
| `gw_policy` | stage / show / approve the delegation policy |
| `gw_grant` | grant, list, revoke resources (a file, a directory, an MCP tool) |
| `gw_schedule` | recurring cadence (`agent=<name>`, no `deliver_to`) |
| `gw_wake` | run one bounded wake now |
| `gw_inbox` / `gw_answer` | questions it is suspended on |
| `gw_journal` | recent runs and the consequential-action journal |
| `gw_doctor` | health check |
| `gw_browser` | verify access to the user's existing Chrome |

## How a wake works

- **Bounded.** Each wake is a single bounded loop, never a continuous process.
  It ends by calling `report`, scheduling its next wake with `schedule_wake`, or
  suspending with `ask_user`.
- **Policy-gated.** Consequential work requires an approved policy. A wake fails
  closed *before* any model call if none is approved, or if it has expired or
  been revoked. Approval is pinned by hash — an unattended agent cannot ask
  permission mid-task, so the authority it will use is reviewed in advance.
- **Journaled.** Consequential actions are recorded reserve → running →
  succeeded/failed with the policy hash, before dispatch. That journal is the
  audit trail for work done while you weren't watching (`gw_journal`).
- **Confined.** `read_file`/`write_file` are limited to granted paths
  (canonicalized, symlink-resolved); its own home and workspace are always
  available. Revoking takes effect at the next dispatch.
- **Able to stop and ask.** `ask_user` suspends the run durably — the question
  goes to `gw_inbox`, and the exact continuation (full transcript plus the
  pending tool call) is saved. `gw_answer` resumes from precisely that point:
  nothing already done is repeated, and a second answer is refused.
- **Able to delegate.** `delegate` spawns a scoped worker — a full memcode agent
  with real toolsets (browser, MCP, shell, filesystem, skills) — as a detached
  job, bounded by a subset of the parent's own policy. `check_delegate` collects
  the result on a later wake.

## Scheduling

Two different things, one scheduler:

- **Cadence you choose** is an ordinary `schedules:` entry (`gw_schedule`) with
  `agent: <name>`. Leave `deliver_to` empty and the wake goes to the agent
  itself, its report journaled in its home rather than sent to a chat.
- **The agent's own next wake** ("come back in 45 minutes") is written from
  inside a run by `schedule_wake`, stored per-agent and claimed atomically so it
  cannot double-fire across restarts or across two gateway processes.

A running gateway is what fires both.

## Browser

`browser: existing_chrome` attaches the agent's browser work to your **own
already-running, signed-in Chrome**, so it can act inside accounts you are
logged into. It needs Chrome 144+ with Remote Debugging enabled at
`chrome://inspect/#remote-debugging`, a running gateway (which owns the broker
arbitrating exclusive access), and your click on Chrome's own Allow dialog —
that consent step is yours alone. Check it with `gw_browser`.

If the broker is unreachable, browser work **fails closed**. It never silently
falls back to a fresh logged-out profile, because that would quietly do
something other than what you asked.

## Memory

What an agent learns goes into `memory.md` in its home via the `remember` tool,
and is read back on every future wake — so an answer you give once is not asked
again. This is the same durable memory every memcode agent has.

Known limitation: plain prose cannot distinguish *you told me this* from *I
inferred it* from *a website said so*, nor mark a claim safe to state on your
behalf, nor mark it stale. That matters once an agent fills in a form or sends
a message about you; structured provenance is a deliberate follow-up.

## State and safety

State lives under `~/.memcode/agents/<id>/`: `memory.md`, `config.yaml` (a
readable mirror of the policies and grants held in the database), `policies/`,
`runs/`, `workspace/`, and an SQLite store with WAL and versioned migrations.

`pause` stops future unattended wakes without deleting anything. Removing an
agent from config keeps its home; deleting the home is a separate, explicit act.

Generated code is untrusted: it runs with staged inputs, a scrubbed
environment, an executable allowlist, and bounded time/output, and fails closed
where a hardened sandbox (Linux `bwrap`) is unavailable. `gw_doctor` reports
sandbox availability.

## Current scope

Working: objective/subgoal store, policy gate, journaled bounded wakes,
resource grants, suspend/resume, delegation to scoped workers, self-scheduled
and gateway-scheduled wakes, the existing-Chrome broker, and health checks.

Not yet wired: external-consequence classes beyond `external_effect` /
`external_representation` (financial, legal attestation, destructive) as live
dispatch inputs, adaptive pacing, and structured fact provenance. Native
desktop automation remains a future backend.
