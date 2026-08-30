# Personal Agents

Personal Agents are domain-general, persistent environment agents operated through `memcode personal`. Personal is the cockpit; the existing Gateway daemon is the engine room for durable trigger intake and scheduled wakes.

## Quick start

```
memcode personal                                   # interactive cockpit (like memcode admin)
memcode personal create <name> "<objective>"
memcode personal policy set <name> policy.json     # stage a draft
memcode personal approve-policy <name> <hash>      # approve by hash
memcode personal run <name>                        # one bounded wake
memcode personal triggers add <name> interval 30m  # recurring wakes (gateway)
memcode personal doctor <name>
```

Bare `memcode personal` opens an interactive management session — the same TUI as `memcode admin` — where you manage agents in plain language through typed, gated `pa_*` operations (objective, policy, resources, triggers, wake, inbox, answer, history, lifecycle). The subcommands are the same operations in scriptable form.

A minimal policy (`policy.json`):

```json
{
  "objective_scope": "primary",
  "consequence_classes": ["observe", "local_mutation"],
  "max_seconds": 300,
  "max_actions_per_period": 8,
  "max_delegation_depth": 1
}
```

## How it works

- **Objective** — the approved desired outcome and success criteria. The executive breaks it into subgoals (data, not compiled workflow types).
- **Bounded wakes** — each `run`/trigger wake is a single bounded LLM loop. It ends by calling `report`, scheduling the next wake with `schedule_wake`, or suspending with `ask_user`. The agent never runs continuously.
- **Policy gate** — consequential work requires an approved policy. `RunOnce` fails closed before any model call if no policy is approved, or if the policy is expired/revoked. Approving a policy activates the objective.
- **Resource grants** — `resources add` grants filesystem roots (canonical, symlink-resolved) with an access mode. `read_file`/`write_file` in the executive are confined to grants; the agent's own home and generated workspace are always available. `resources revoke` takes effect at the next dispatch.
- **Journal** — consequential executive actions (e.g. `write_file`) are journaled with reserve → running → succeeded/failed and the policy hash, before dispatch.
- **Triggers** — durable `interval` / `cron` / `one-shot` / `next_wake` records in the agent home. The running gateway polls them every 15s, claims each due trigger atomically, and runs a wake.
- **Human-in-the-loop** — `ask_user` suspends a run durably: the interaction is recorded in the agent's DB and the exact continuation (transcript + tool_use_id) is saved under `runs/<id>/`. `personal inbox` lists pending questions; `personal answer <name> <id> <answer>` resolves it and resumes with the matching tool_result — no replay of completed actions, no double-resume.

## Commands

Interactive cockpit: bare `memcode personal` (typed `pa_*` tools). Scriptable subcommands: `create` `list` `show` `run` `inbox` `answer` `pause` `resume` `stop` `delete` · `policy set|show` + `approve-policy` · `resources add|list|revoke` · `triggers add|list|pause|resume` · `history` · `doctor`

## Controls and safety

`pause`/`stop` change objective status so future wakes refuse to run. `delete` removes the config entry but keeps the agent home; `--delete-home` is the explicit destructive path. State lives under `~/.memcode/agents/<id>/` (`personal.db` with WAL + versioned migrations, `policies/`, `runs/`, `workspace/`).

Generated code is untrusted: `RunGenerated` uses staged inputs, a scrubbed environment, executable allowlists, and bounded time/output, and fails closed when a hardened sandbox (Linux `bwrap`) is unavailable. `doctor` reports sandbox availability as informational.

## Current scope

Implemented and tested: objective/subgoal/fact store, policy gate, journaled bounded executive with observe + local-mutation tools, durable triggers via the gateway, suspend/resume, resources, history, and doctor.

Not yet wired into the executive loop (the primitives exist and are unit-tested): dynamically delegated sub-agents, the existing-Chrome broker, external-consequence classes (external_effect/financial/legal/destructive), and adaptive pacing as a live input to dispatch. Native desktop automation remains a future backend.
