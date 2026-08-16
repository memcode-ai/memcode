package server

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/channels"
)

// handleCommand processes a /agent or /project control message, re-pointing the
// conversation for its SUBSEQUENT tasks. It returns true when the message was a
// recognized command — control, not a task, so it is never enqueued. Runs after
// authorization, so only allow-listed principals can switch selection.
func (r *runtime) handleCommand(ctx context.Context, inb channels.Inbound) bool {
	fields := strings.Fields(strings.TrimSpace(inb.Text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return false
	}
	var reply string
	switch fields[0] {
	case "/agent":
		if len(fields) < 2 {
			reply = "Usage: /agent <name>. " + r.agentList()
			break
		}
		id := fields[1]
		if _, ok := r.cfg().Agents[id]; !ok {
			reply = fmt.Sprintf("Unknown agent %q. %s", id, r.agentList())
			break
		}
		if err := r.gw.SetConversationAgent(ctx, inb.Channel, inb.Conversation, id); err != nil {
			reply = "Couldn't switch agent: " + err.Error()
			break
		}
		reply = "Agent set to " + id + " for your next message."
	case "/project":
		if len(fields) < 2 {
			reply = "Usage: /project <name>. " + r.projectList()
			break
		}
		id := fields[1]
		if _, err := r.cfg().ResolveProject(id); err != nil { // registry is the authority
			reply = fmt.Sprintf("%v. %s", err, r.projectList())
			break
		}
		if !r.cfg().ProjectAllowed(inb.Channel, id) { // channel policy narrows the registry
			reply = fmt.Sprintf("Project %q is not allowed on this channel. %s", id, r.channelProjectList(inb.Channel))
			break
		}
		if err := r.gw.SetConversationProject(ctx, inb.Channel, inb.Conversation, id); err != nil {
			reply = "Couldn't switch project: " + err.Error()
			break
		}
		reply = "Project set to " + id + " for your next message."
	default:
		return false // an unrecognized slash message is just a task that starts with "/"
	}
	if ch := r.byName[inb.Channel]; ch != nil && reply != "" {
		_ = ch.Send(ctx, inb.Conversation, channels.Outbound{Text: reply})
	}
	return true
}

// resolveSelection returns the agent and project id a new task should snapshot:
// the conversation's explicit choice if set, else the channel/gateway defaults.
// The project always satisfies the channel's project policy: a default (or a
// selection made before the policy tightened) that is no longer allowed falls to
// the first project the channel IS allowed, never through to the gateway default.
func (r *runtime) resolveSelection(ctx context.Context, channel, conversation string) (agent, project string) {
	cfg := r.cfg() // one snapshot: the allowed-check and the fallback index must agree
	agent = cfg.Get(channel).Agent
	project = cfg.DefaultProject
	if a, p, err := r.gw.Conversation(ctx, channel, conversation); err == nil {
		if a != "" {
			agent = a
		}
		if p != "" {
			project = p
		}
	}
	if !cfg.ProjectAllowed(channel, project) {
		project = cfg.Get(channel).Projects[0] // non-empty list, or Allowed would be true
	}
	return agent, project
}

func (r *runtime) agentList() string {
	ids := make([]string, 0, len(r.cfg().Agents))
	for id := range r.cfg().Agents {
		ids = append(ids, id)
	}
	return listOrNone("agents", ids)
}

// channelProjectList names the projects a channel may select: its configured
// subset when one is set, else every registered project.
func (r *runtime) channelProjectList(channel string) string {
	if allowed := r.cfg().Get(channel).Projects; len(allowed) > 0 {
		ids := append([]string(nil), allowed...)
		return listOrNone("projects", ids)
	}
	return r.projectList()
}

func (r *runtime) projectList() string {
	ids := make([]string, 0, len(r.cfg().Projects))
	for id := range r.cfg().Projects {
		ids = append(ids, id)
	}
	return listOrNone("projects", ids)
}

func listOrNone(kind string, ids []string) string {
	if len(ids) == 0 {
		return "No " + kind + " registered."
	}
	sort.Strings(ids)
	return "Available " + kind + ": " + strings.Join(ids, ", ")
}
