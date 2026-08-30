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
	GwAgent    = "gw_agent"    // create/remove agents; objective, autonomy, browser, pause
	GwSchedule = "gw_schedule" // recurring tasks (cron)
	GwService  = "gw_service"  // the background daemon: status, install, uninstall

	// Autonomy tools — these apply to an agent allowed to run unattended
	// (gw_agent action=autonomous). They are ordinary admin tools, not a
	// separate species of agent: an autonomous agent is an agent with an
	// objective, an approved policy, and permission to act on its own.
	GwPolicy  = "gw_policy"  // stage/show/approve the delegation policy (the authority ceremony)
	GwGrant   = "gw_grant"   // grant/list/revoke resources (filesystem paths, mcp tools, ...)
	GwWake    = "gw_wake"    // run one bounded wake now
	GwInbox   = "gw_inbox"   // questions an agent is suspended on
	GwAnswer  = "gw_answer"  // answer one, resuming the suspended run
	GwJournal = "gw_journal" // recent runs + the consequential-action journal
	GwDoctor  = "gw_doctor"  // health check an agent's home, objective, policy, wakes
	GwBrowser = "gw_browser" // check/connect the user's existing Chrome for browser work
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
			Description: "Create, configure, or remove a lasting agent identity with its own memory and skills; use gw_overview to inspect existing agents (identity file: ~/.memcode/agents/<name>/SOUL.md). Bind agents to channels with gw_channel field=agent. action=model pins/clears its model; action=reasoning pins/clears its thinking effort; action=tools sets its tool policy; action=objective sets the durable outcome it works toward; action=autonomous grants or revokes permission to run unattended; action=browser picks its browser backend; action=pause/resume stops or restarts unattended wakes without deleting anything.\n\nobjective and autonomous are SEPARATE grants and must be proposed separately: an objective says what the agent is for, autonomous says it may act on that without being asked. An agent can hold an objective you only ever work on together, and an agent can run unattended on a schedule with no standing objective at all.",
			InputSchema: obj(map[string]any{
				"action":            str("add, objective, autonomous, browser, pause, resume, tools, reasoning, model, or remove"),
				"name":              str("agent name, e.g. assistant, coder, researcher"),
				"model":             str("add/model: pin the model that drives this agent everywhere (catalog id, e.g. \"claude-sonnet-5\"); empty = automatic routing"),
				"reasoning":         str("add/reasoning: pin thinking effort — off, medium, or high; empty = per-turn automatic"),
				"toolsets":          str("tools: comma-separated allow-list of toolsets/tools; empty = all"),
				"disabled_toolsets": str("tools: comma-separated toolsets/tools to remove; deny wins over allow"),
				"objective":         str("add/objective: the durable outcome this agent works toward, e.g. \"Find backend roles and keep a shortlist\"; empty clears it"),
				"autonomous":        str("autonomous: \"true\" to let it run unattended (policy-gated, action-journaled, suspends durably on questions), anything else to revoke"),
				"browser":           str("add/browser: \"existing_chrome\" to drive the user's OWN running, signed-in Chrome; \"ephemeral\" (default) for a fresh logged-out profile"),
			}, "action", "name"),
		},
		{
			Name:        GwSchedule,
			Description: "Manage scheduled tasks. add creates one (recurring via cron/every, or a one-shot via at); remove deletes; disable pauses without deleting; enable resumes. deliver_to routes the result to a conversation, e.g. \"telegram:123456789\".\n\nThis is ALSO how an autonomous agent gets its recurring cadence: set agent=<name> and leave deliver_to empty, and the wake is delivered to the agent itself (its report is journaled in its home rather than sent to a chat). There is no separate scheduler for autonomous agents.",
			InputSchema: obj(map[string]any{
				"action":     str("add, remove, enable, or disable"),
				"name":       str("schedule name"),
				"cron":       str("add only: cron expression, e.g. \"0 9 * * 1-5\""),
				"every":      str("add only: interval as a Go duration, e.g. \"30m\""),
				"at":         str("add only: one-shot RFC3339 time, e.g. \"2026-03-01T09:00:00Z\""),
				"task":       str("add only: the task to run, in plain language"),
				"deliver_to": str("add only: where the result goes, channel:conversation. Omit it when agent= names an autonomous agent — the wake then goes to the agent itself."),
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
		{
			Name:        GwPolicy,
			Description: "The delegation policy that bounds what an agent may do while running unattended. action=show (the approved one), action=stage (write a draft from a DelegationPolicy JSON in 'document'), action=approve (by hash). Approval is deliberately a two-step ceremony pinned by hash: an unattended agent cannot ask permission mid-run, so the authority it will use has to be reviewed and fixed in advance. Consequential work stays blocked until a policy is approved.",
			InputSchema: obj(map[string]any{
				"agent":    str("agent name"),
				"action":   str("show, stage, or approve"),
				"document": str("stage only: the DelegationPolicy JSON (allowed_tools, consequence_classes, max_seconds, max_actions_per_period, max_delegation_depth, ...)"),
				"hash":     str("approve only: the policy hash or a unique prefix, as returned by stage"),
			}, "agent", "action"),
		},
		{
			Name:        GwGrant,
			Description: "Grant or revoke a resource an agent may reach. action=grant (locator, optionally type and mode), action=list, action=revoke (id). type defaults to filesystem and mode to read — the common case is just a path. Filesystem paths are canonicalized and symlink-resolved, and a grant may be a single file or a whole directory. Revoking takes effect at the agent's next dispatch.",
			InputSchema: obj(map[string]any{
				"agent":   str("agent name"),
				"action":  str("grant, list, or revoke"),
				"type":    str("grant only: filesystem (default), mcp, command, channel, repository"),
				"locator": str("grant only: the path or identifier, e.g. ~/resume.md"),
				"mode":    str("grant only: read (default), write, or admin"),
				"id":      str("revoke only: the resource id from list"),
			}, "agent", "action"),
		},
		{
			Name:        GwWake,
			Description: "Run one bounded wake for an agent right now, without waiting for its schedule. Fails closed if no policy is approved. Returns the run's status and report. Works on any agent with an objective — an agent does not have to be autonomous to be woken on demand; autonomy only governs whether it wakes on its own.",
			InputSchema: obj(map[string]any{
				"agent": str("agent name"),
			}, "agent"),
		},
		{
			Name:        GwInbox,
			Description: "List the questions an agent is suspended waiting on. An unattended run that needs a human does not prompt — it suspends durably and waits here.",
			InputSchema: obj(map[string]any{
				"agent": str("agent name"),
			}, "agent"),
		},
		{
			Name:        GwAnswer,
			Description: "Answer a pending question, resuming the suspended run from the exact point it paused — nothing already done is repeated.",
			InputSchema: obj(map[string]any{
				"agent":  str("agent name"),
				"id":     str("the interaction id from gw_inbox"),
				"answer": str("the human's answer"),
			}, "agent", "id", "answer"),
		},
		{
			Name:        GwJournal,
			Description: "Show an agent's recent runs and its journal of consequential actions — what it actually did, under which approved policy. This is the audit trail for work done while nobody was watching.",
			InputSchema: obj(map[string]any{
				"agent": str("agent name"),
			}, "agent"),
		},
		{
			Name:        GwDoctor,
			Description: "Health check an agent set up to run on its own: home directory layout, objective, approved policy, generated workspace, sandbox availability, scheduled wakes, and pending questions. Use it when something looks wrong, or before the first wake.",
			InputSchema: obj(map[string]any{
				"agent": str("agent name"),
			}, "agent"),
		},
		{
			Name:        GwBrowser,
			Description: "Check whether browser work against the user's OWN running Chrome is ready: verifies npx and the gateway's browser broker, then attempts a real, bounded connection. Call it when an agent's browser work fails closed, or when setting up an agent with browser=existing_chrome. It cannot click Chrome's own Allow dialog — only the user can do that; report what it needs instead.",
			InputSchema: obj(map[string]any{}),
		},
	}
}
