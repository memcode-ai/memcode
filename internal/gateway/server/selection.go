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
		if _, ok := r.settings.Agents[id]; !ok {
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
		if _, err := r.settings.ResolveProject(id); err != nil { // registry is the authority
			reply = fmt.Sprintf("%v. %s", err, r.projectList())
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

// resolveSelection returns the persona and project id a new task should snapshot:
// the conversation's explicit choice if set, else the channel/gateway defaults.
func (r *runtime) resolveSelection(ctx context.Context, channel, conversation string) (agent, project string) {
	agent = r.settings.Get(channel).Agent
	project = r.settings.DefaultProject
	if a, p, err := r.gw.Conversation(ctx, channel, conversation); err == nil {
		if a != "" {
			agent = a
		}
		if p != "" {
			project = p
		}
	}
	return agent, project
}

func (r *runtime) agentList() string {
	ids := make([]string, 0, len(r.settings.Agents))
	for id := range r.settings.Agents {
		ids = append(ids, id)
	}
	return listOrNone("agents", ids)
}

func (r *runtime) projectList() string {
	ids := make([]string, 0, len(r.settings.Projects))
	for id := range r.settings.Projects {
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
