// Admin toolset — the `memcode admin` session's ONLY tools (plus ask_user).
// Deterministic, typed operations over the gateway configuration and state:
// the model converses, these tools read and write. No shell, no file editor,
// and secrets are structurally out of reach (nothing here touches the .env).
package tools

import "github.com/memcode-ai/memcode/internal/wire"

const (
	GwOverview = "gw_overview" // full current state: channels, projects, agents, schedules, pending pairings
	GwChannel  = "gw_channel"  // per-channel settings: allow list, agent, tier, pairing, voice, group behavior
	GwPairing  = "gw_pairing"  // approve/deny a pending pairing code
	GwProject  = "gw_project"  // register/remove working directories
	GwAgent    = "gw_agent"    // create/remove agents
	GwSchedule = "gw_schedule" // recurring tasks (cron)
	GwService  = "gw_service"  // the background daemon: status, install, uninstall
)

// AdminDefs returns the admin session's tool registry.
func AdminDefs() []wire.ToolDef {
	return []wire.ToolDef{
		{
			Name:        GwOverview,
			Description: "Read the full current configuration and live state: every channel (configured or not, allow list, agent, tier, pairing, voice replies), registered projects, agents, schedules, and pending pairing requests. Call this before answering questions about current state.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        GwChannel,
			Description: "Change one channel's settings. Fields: allow_add / allow_remove (a stable user id, or \"*\" for anyone), agent (agent name), tier (\"\", \"strong\", \"frontier\"), pairing (\"true\"/\"false\" — offer codes to unknown DM senders), respond_to_all (\"true\"/\"false\" — act on every group message), voice_replies (\"off\", \"in_kind\", \"always\"), poll (email only, a duration like \"30s\"), projects (comma-separated project ids this channel is limited to; empty clears the limit).",
			InputSchema: obj(map[string]any{
				"channel": str("channel name: telegram, discord, slack, email, signal, matrix, mattermost, msteams, googlechat, sms, github, whatsapp"),
				"field":   str("one of: allow_add, allow_remove, agent, tier, pairing, respond_to_all, voice_replies, poll, projects"),
				"value":   str("the new value (see field descriptions)"),
			}, "channel", "field", "value"),
		},
		{
			Name:        GwPairing,
			Description: "Approve or deny a pending pairing request by its code (gw_overview lists pending ones). Approving adds the sender to that channel's allow list; the gateway picks it up within seconds.",
			InputSchema: obj(map[string]any{
				"action": str("approve or deny"),
				"code":   str("the pairing code, e.g. K3QP7M"),
			}, "action", "code"),
		},
		{
			Name:        GwProject,
			Description: "Register or remove a project (a working directory agent tasks may run in). Messages can only ever run against registered projects.",
			InputSchema: obj(map[string]any{
				"action": str("add, remove, or set_default"),
				"path":   str("absolute directory path (add only)"),
				"id":     str("project id (remove/set_default; optional on add, defaults to the directory name)"),
			}, "action"),
		},
		{
			Name:        GwAgent,
			Description: "Create or remove an agent: a lasting assistant identity with its own memory and skills (identity file: ~/.memcode/agents/<name>/SOUL.md). Bind a channel to one with gw_channel field=agent. action=model pins/clears its model; action=reasoning pins/clears its thinking effort.",
			InputSchema: obj(map[string]any{
				"action":    str("add, remove, model, or reasoning"),
				"name":      str("agent name, e.g. personal, coder, researcher"),
				"model":     str("add/model: pin the model that drives this agent everywhere (catalog id, e.g. \"claude-sonnet-5\"); empty = automatic routing"),
				"reasoning": str("add/reasoning: pin thinking effort — off, medium, or high; empty = per-turn automatic"),
			}, "action", "name"),
		},
		{
			Name:        GwSchedule,
			Description: "Manage scheduled tasks. add creates one (recurring via cron/every, or a one-shot via at); remove deletes; disable pauses without deleting; enable resumes. deliver_to routes the result to a conversation, e.g. \"telegram:123456789\".",
			InputSchema: obj(map[string]any{
				"action":     str("add, remove, enable, or disable"),
				"name":       str("schedule name"),
				"cron":       str("add only: cron expression, e.g. \"0 9 * * 1-5\""),
				"every":      str("add only: interval as a Go duration, e.g. \"30m\""),
				"at":         str("add only: one-shot RFC3339 time, e.g. \"2026-03-01T09:00:00Z\""),
				"task":       str("add only: the task to run, in plain language"),
				"deliver_to": str("add only: where the result goes, channel:conversation"),
				"agent":      str("add only: run as this agent (its pinned model and instructions apply)"),
			}, "action", "name"),
		},
		{
			Name:        GwService,
			Description: "The gateway daemon. status: whether a gateway is running now and whether the background service (launchd/systemd) is installed. install: register it as a background service that survives logout and reboot. uninstall: remove the service unit.",
			InputSchema: obj(map[string]any{
				"action": str("status, install, or uninstall"),
			}, "action"),
		},
	}
}
