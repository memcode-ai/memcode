// Package agent holds the agent runtime and its supporting subsystems.
//
// The hub is the runtime package (internal/agent/runtime), which owns the
// Session: the agent loop, tool dispatch, plan-mode lifecycle, cost ledger,
// and introspection. Supporting sub-packages are leaves below it:
//
//   - acceptance — reconciles prior agent work against git (did edits survive?).
//   - compaction — summarizes old turns when the context window fills.
//   - edit       — anchored search/replace edit application + hashing.
//   - focus      — tracks the active subsystem focus for context scoping.
//   - input      — parses user input routing (steer/queue/interrupt/shell).
//   - jobs       — detached background agent jobs.
//   - mood       — the session's mood (display state derived from turn history).
//   - permissions — the bash-command risk classifier and permission modes.
//   - protocol   — the stream-json machine control protocol adapter.
//   - room       — the room reducer (aggregates events into display state).
//   - secrets    — redaction of secrets before model calls.
//   - tools      — tool definitions and dispatch table.
//
// The dependency graph is a clean DAG: runtime is the hub; the leaves depend
// on it (or on nothing), never the reverse.
package agent
