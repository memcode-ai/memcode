# Personal Agents

**Status:** Draft design contract  
**Date:** August 30, 2026

## Purpose

Personal Agents are domain-general, long-lived environment agents operated through:

```text
memcode personal
```

A Personal Agent accepts a user-authored objective, models relevant parts of the user's granted environment, creates and revises intermediate subgoals, schedules bounded future work, delegates dynamically scoped workers, pauses durably for human involvement, and improves its effectiveness through external generated artifacts.

Memcode is the stable runtime kernel. Self-evolution occurs in the agent-owned capability layer, not by modifying the Memcode binary or source checkout.

## Architectural invariant

> `internal/personal` contains no domain-specific workflow concepts, fixed worker roles, provider-specific business logic, or predefined user-profile schema.

Domain behavior belongs in objective data, memory, generated artifacts, installed skills, resource grants, and available tools.

## Product boundary

**Personal is the cockpit; Gateway is the engine room.**

An ordinary named agent is a durable identity with model, reasoning, memory, skills, and tool configuration. A Personal Agent is an additive named-agent kind that also owns objective state, policies, resources, triggers, action history, interactions, generated artifacts, and durable executive transcripts.

The gateway remains the single daemon and recurring-execution engine. Personal Agents do not introduce a second service or identity hierarchy.

## Objectives and subgoals

A user objective is the durable statement of desired outcome and success criteria. It is authored or approved by the user and defines the executive's scope.

A subgoal is agent-generated planning state beneath an objective. Subgoals may be created, revised, blocked, completed, or abandoned as evidence changes. They do not expand authority and are not substitutes for the objective's success criteria.

Repository objectives remain repository-scoped and unchanged. Personal objectives are global, agent-scoped records stored beneath the named agent's home.

## Agent home and ownership

A Personal Agent owns state beneath:

```text
~/.memcode/agents/<id>/
  personal.db
  policies/
  workspace/
    generated/
    scratch/
  runs/
  workers/
  .memcode/
    jobs/
    sessions/
```

Existing identity, memory, and skill files remain in the same agent home. Removing an agent from gateway configuration is non-destructive. Deleting the home requires a separate explicit destructive operation.

The SQLite store uses explicit migrations and WAL mode. It contains only domain-neutral records: objectives, subgoals, runs, triggers, policies, resources, facts, actions, generated items, and notifications.

## Delegation policy

Autonomy is governed by a canonical, versioned policy approved by hash. The policy describes objective scope, tools, resources, consequence classes, limits, budgets, pacing, escalation, notification, and stop conditions.

General consequence classes are:

```text
observe
local_mutation
external_effect
external_representation
financial
legal_attestation
destructive
```

Actions within an approved policy may proceed without repeated approval. Authority expansion requires approval of a new policy version. Restriction-only changes and revocation take effect immediately. Personal policy is an additional gate and never replaces Memcode's existing permission checks.

## Resource grants

Resources are opaque, typed grants with canonical locators, access modes, constraints, authorization provenance, policy version, and expiration or revocation state. Types may include filesystem locations, browser sessions or origins, MCP capabilities, commands, repositories, cloud tools, documents, communication channels, and generated processes.

Agents begin with their own home and explicitly enabled tools. Access outside the home requires a grant. Sensitive contents, browser credentials, cookies, and ambient secrets are never exported as resources.

## Dynamic execution envelopes

Every direct or delegated run receives a structured execution envelope identifying its objective, subgoal, parent run, policy hash, selected tools, narrowed resources, allowed consequences, budgets, browser mode, and reporting behavior.

A worker receives a strict subset of its parent's authority. Worker names and task descriptions are arbitrary data selected for the current subgoal; there are no compiled worker-role categories. Generated artifacts cannot increase their own envelope.

## Durable interaction and continuation

Generic interaction kinds are:

```text
question
approval
environment_handoff
challenge
missing_information
policy_exception
```

An interaction records the run, job, session, conversation, pending tool-use ID, structured request, policy version, lifecycle timestamps, response, and continuation metadata.

When a tool requires human involvement, the runtime persists the complete assistant response and unresolved tool-use block, creates the interaction, marks the run waiting, and exits cleanly. On answer, the runtime appends the matching tool result—or executes the exact approved saved call once—and resumes the same transcript without an extra user turn or replay of completed work.

Suspending tool calls must initially be the sole tool use in an assistant response. Stale, duplicate, mismatched, expired, or resolved interactions fail closed.

## Action journal and idempotency

Every Personal Agent action is journaled through:

```text
planned → reserved → running → succeeded | failed | uncertain | cancelled
```

The record contains objective, subgoal, run, kind, target, consequence class, policy hash, redacted request, idempotency data, result, evidence, and timestamps.

Consequential actions are policy-checked and reserved before dispatch. Ambiguous outcomes become `uncertain` and are not automatically retried. Restart recovery must reconcile uncertainty through observation or human input.

## Generated workspace and self-evolution

The generated workspace is a permissive local Git repository, not a mandatory package format. It may contain scripts, compiled programs, browser procedures, transforms, evaluators, data stores, skills, MCP servers, managed services, documentation, and operating procedures.

A lightweight database index records path, hash, purpose, provenance, parent revision, required envelope, invocation/evaluation commands, evaluation results, use time, and active revision.

After meaningful work the executive evaluates progress, cost, latency, repeated steps, failures, corrections, instability, and reuse opportunities. It may continue, change strategy, reuse, generate, improve, retire, escalate, or abandon. Repeated autonomous use requires evaluation, a Git commit, policy compatibility, and rollback after regression.

## Browser broker trust boundary

Ordinary sessions retain the existing ephemeral browser backend. Personal Agents may use an explicitly authorized connection to the user's existing Chrome through a gateway-owned broker and permission-protected local socket.

The broker owns controller lifecycle, authenticates short-lived scoped run tokens, serializes control with leases, associates created pages with an agent and run, redacts sensitive headers, and exposes narrow operations rather than raw controller access. It never exports cookies or credentials and never closes or mutates unrelated tabs.

Existing-Chrome access is broadly privileged. Policy and tab ownership reduce accidental interference but cannot make a compromised controller harmless. Connection, version, or authentication failures fail closed; they never silently fall back to another profile.

Login and environmental challenges create durable handoff interactions tied to an owned tab.

## Adaptive pacing

Pacing considers urgency, deadlines, recent volume, repeated actions, concurrency, errors, warnings, challenges, uncertainty, quiet hours, and opportunities to batch locally. Persisted controls include resource concurrency, burst caps, cooldowns, bounded jitter, exponential backoff, warning-triggered slowdown, challenge suspension, and time-period budgets.

Pacing exists for safe, low-impact operation—not human simulation or protection bypass.

## Pause, revocation, and shutdown

Pause prevents future wakes and consequential dispatch. Stop also requests active workers and generated services to terminate. Revocation is checked before every dispatch and releases affected resource and browser leases.

Pending interactions may be cancelled. Uncertain actions require explicit reconciliation. Gateway restart recovery reconciles workers, interactions, triggers, sessions, browser leases, actions, services, and policy hashes before work resumes.

No consequential recovered work may continue unless its recorded policy hash remains approved.

Deletion is explicitly destructive and separate from non-destructive removal from gateway configuration. Audit export is redacted by default.

## Stable-kernel boundary

Personal Agents may create and operate external capabilities within approved envelopes, but they do not autonomously modify the Memcode executable or source checkout. New environment backends, including native desktop control, may be added later without introducing objective-specific concepts into the Personal core.
