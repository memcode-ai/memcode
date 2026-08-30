package tools

import "github.com/memcode-ai/memcode/internal/wire"

// Personal cockpit toolset — the `memcode personal` interactive session's ONLY
// typed operations (plus ask_user). Deterministic management of Personal Agents:
// objectives, policies, resources, triggers, wakes, and pending interactions.
const (
	PaOverview  = "pa_overview"  // list all Personal Agents with status
	PaObjective = "pa_objective" // show/set an agent's objective
	PaPolicy    = "pa_policy"    // stage/show/approve delegation policies
	PaResource  = "pa_resource"  // grant/list/revoke resources
	PaTrigger   = "pa_trigger"   // add/list/pause/resume wake triggers
	PaWake      = "pa_wake"      // run one bounded wake now
	PaInbox     = "pa_inbox"     // list pending human interactions
	PaAnswer    = "pa_answer"    // answer a pending interaction
	PaHistory   = "pa_history"   // recent runs + journaled actions
	PaLifecycle = "pa_lifecycle" // pause/resume/stop/delete an agent
)

// PersonalDefs returns the personal-cockpit tool registry.
func PersonalDefs() []wire.ToolDef {
	return []wire.ToolDef{
		{
			Name:        PaOverview,
			Description: "List all Personal Agents: objective, status, approved policy version, pending questions, next wake. Call this first to answer questions about current state.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        PaObjective,
			Description: "Show or change an agent's objective (the durable desired outcome + success criteria). action=show or action=set with text.",
			InputSchema: obj(map[string]any{
				"agent":  str("agent name"),
				"action": str("show or set"),
				"text":   str("set only: the objective text"),
			}, "agent", "action"),
		},
		{
			Name:        PaPolicy,
			Description: "Manage an agent's delegation policy. action=show (approved), action=stage (write a draft from a JSON policy doc in 'document'), action=approve (by hash). Consequential work is blocked until a policy is approved.",
			InputSchema: obj(map[string]any{
				"agent":    str("agent name"),
				"action":   str("show, stage, or approve"),
				"document": str("stage only: the DelegationPolicy JSON"),
				"hash":     str("approve only: the policy hash or prefix"),
			}, "agent", "action"),
		},
		{
			Name:        PaResource,
			Description: "Grant or revoke a resource. action=grant (type, locator, mode), action=list, action=revoke (id). Filesystem paths are canonicalized and symlink-resolved.",
			InputSchema: obj(map[string]any{
				"agent":   str("agent name"),
				"action":  str("grant, list, or revoke"),
				"type":    str("grant only: filesystem, mcp, command, channel, repository"),
				"locator": str("grant only: the path or identifier"),
				"mode":    str("grant only: read, write, or admin"),
				"id":      str("revoke only: the resource id"),
			}, "agent", "action"),
		},
		{
			Name:        PaTrigger,
			Description: "Manage wake triggers. action=add (kind: interval|cron|one-shot, spec), action=list, action=pause/resume (id).",
			InputSchema: obj(map[string]any{
				"agent":  str("agent name"),
				"action": str("add, list, pause, or resume"),
				"kind":   str("add only: interval, cron, or one-shot"),
				"spec":   str("add only: e.g. 30m, '0 * * * *', or RFC3339"),
				"id":     str("pause/resume only: the trigger id"),
			}, "agent", "action"),
		},
		{
			Name:        PaWake,
			Description: "Run one bounded wake for an agent right now. Fails closed if no policy is approved. Returns the run's status and report.",
			InputSchema: obj(map[string]any{
				"agent": str("agent name"),
			}, "agent"),
		},
		{
			Name:        PaInbox,
			Description: "List an agent's pending human interactions (questions it is suspended waiting on).",
			InputSchema: obj(map[string]any{
				"agent": str("agent name"),
			}, "agent"),
		},
		{
			Name:        PaAnswer,
			Description: "Answer a pending interaction, resuming the suspended run with the exact continuation.",
			InputSchema: obj(map[string]any{
				"agent":  str("agent name"),
				"id":     str("the interaction id"),
				"answer": str("the human's answer"),
			}, "agent", "id", "answer"),
		},
		{
			Name:        PaHistory,
			Description: "Show an agent's recent runs and journaled consequential actions.",
			InputSchema: obj(map[string]any{
				"agent": str("agent name"),
			}, "agent"),
		},
		{
			Name:        PaLifecycle,
			Description: "Change an agent's lifecycle. action=pause, resume, stop, or delete. delete removes the config entry but keeps the agent home unless delete_home=true.",
			InputSchema: obj(map[string]any{
				"agent":       str("agent name"),
				"action":      str("pause, resume, stop, or delete"),
				"delete_home": str("delete only: 'true' to also permanently delete the agent home"),
			}, "agent", "action"),
		},
	}
}
