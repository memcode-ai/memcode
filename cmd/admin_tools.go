package cmd

// The admin session's executor: typed operations over the gateway
// configuration (gwconfig — Save validates, and the running gateway
// hot-reloads within seconds) and the pairing table in the gateway state
// store. Lives in cmd because the engine must not import the gateway layer;
// the runtime injects it (runtime.AdminExecutor) and gates every mutation
// through the user-visible approval card before it runs. Secrets are
// structurally out of reach: nothing here opens the .env.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	gwstate "github.com/memcode-ai/memcode/internal/gateway/state"
)

// adminChannels is the closed set of channel names the admin tools accept.
var adminChannels = map[string]bool{
	"telegram": true, "discord": true, "slack": true, "email": true,
	"signal": true, "matrix": true, "mattermost": true, "msteams": true,
	"googlechat": true, "sms": true, "github": true, "whatsapp": true,
}

// adminExecute is the runtime.AdminExecutor for `memcode admin`.
func adminExecute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	switch name {
	case tools.GwOverview:
		return adminOverview(ctx)
	case tools.GwChannel:
		return adminChannel(input)
	case tools.GwPairing:
		return adminPairing(ctx, input)
	case tools.GwProject:
		return adminProject(input)
	case tools.GwAgent:
		return adminAgent(input)
	case tools.GwSchedule:
		return adminSchedule(input)
	case tools.GwService:
		var in struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", err
		}
		return adminServiceAction(ctx, strings.ToLower(strings.TrimSpace(in.Action)))
	}
	return "", fmt.Errorf("unknown admin tool %q", name)
}

func adminOverview(ctx context.Context) (string, error) {
	settings, err := gwconfig.Load()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	enabled := map[string]bool{}
	for _, name := range gwconfig.EnabledChannels() {
		enabled[name] = true
	}
	b.WriteString("CHANNELS (configured = credentials present in the global .env):\n")
	names := make([]string, 0, len(adminChannels))
	for name := range adminChannels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ch := settings.Get(name)
		state := "not configured"
		if enabled[name] {
			state = "configured"
		}
		fmt.Fprintf(&b, "- %s: %s; allow_from=%v", name, state, ch.AllowFrom)
		if ch.Agent != "" {
			fmt.Fprintf(&b, "; agent=%s", ch.Agent)
		}
		if ch.Tier != "" {
			fmt.Fprintf(&b, "; tier=%s", ch.Tier)
		}
		if ch.Pairing != nil {
			fmt.Fprintf(&b, "; pairing=%v", *ch.Pairing)
		}
		if ch.RespondToAll {
			b.WriteString("; respond_to_all=true")
		}
		if ch.VoiceReplies != "" {
			fmt.Fprintf(&b, "; voice_replies=%s", ch.VoiceReplies)
		}
		if len(ch.Projects) > 0 {
			fmt.Fprintf(&b, "; projects=%v", ch.Projects)
		}
		b.WriteString("\n")
	}
	if settings.AllowAll {
		b.WriteString("WARNING: allow_all=true — every channel answers anyone.\n")
	}
	b.WriteString("\nPROJECTS:\n")
	if len(settings.Projects) == 0 {
		b.WriteString("- none registered\n")
	}
	ids := make([]string, 0, len(settings.Projects))
	for id := range settings.Projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := settings.Projects[id]
		def := ""
		if id == settings.DefaultProject {
			def = " (default)"
		}
		fmt.Fprintf(&b, "- %s: %s enabled=%v%s\n", id, p.Path, p.Enabled, def)
	}
	b.WriteString("\nAGENTS (agents):\n")
	if len(settings.Agents) == 0 {
		b.WriteString("- none\n")
	}
	agentNames := make([]string, 0, len(settings.Agents))
	for name := range settings.Agents {
		agentNames = append(agentNames, name)
	}
	sort.Strings(agentNames)
	for _, name := range agentNames {
		fmt.Fprintf(&b, "- %s: type=%s (home: ~/.memcode/agents/%s)\n", name, settings.Agents[name].Type, name)
	}
	b.WriteString("\nSCHEDULES:\n")
	if len(settings.Schedules) == 0 {
		b.WriteString("- none\n")
	}
	for _, sc := range settings.Schedules {
		when := sc.Cron
		if when == "" {
			when = "every " + sc.Every
		}
		fmt.Fprintf(&b, "- %s: %s -> %q -> %s\n", sc.Name, when, sc.Task, sc.DeliverTo)
	}
	b.WriteString("\nPENDING PAIRINGS:\n")
	pending, err := adminPendingPairings(ctx)
	if err != nil {
		fmt.Fprintf(&b, "- unavailable: %v\n", err)
	} else if len(pending) == 0 {
		b.WriteString("- none\n")
	} else {
		for _, p := range pending {
			fmt.Fprintf(&b, "- code %s: %s user %q\n", p.Code, p.Channel, p.Principal)
		}
	}
	return b.String(), nil
}

func adminPendingPairings(ctx context.Context) ([]gwstate.Pairing, error) {
	dir, err := gwconfig.Dir()
	if err != nil {
		return nil, err
	}
	gw, err := gwstate.OpenShared(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer gw.Close()
	return gw.PendingPairings(ctx, time.Now())
}

func adminChannel(input json.RawMessage) (string, error) {
	var in struct {
		Channel string `json:"channel"`
		Field   string `json:"field"`
		Value   string `json:"value"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	in.Channel = strings.ToLower(strings.TrimSpace(in.Channel))
	in.Field = strings.ToLower(strings.TrimSpace(in.Field))
	val := strings.TrimSpace(in.Value)
	if !adminChannels[in.Channel] {
		return "", fmt.Errorf("unknown channel %q", in.Channel)
	}
	settings, err := gwconfig.Load()
	if err != nil {
		return "", err
	}
	if settings.Channels == nil {
		settings.Channels = map[string]gwconfig.Channel{}
	}
	ch := settings.Channels[in.Channel]
	switch in.Field {
	case "allow_add":
		if val == "" {
			return "", fmt.Errorf("allow_add needs a user id")
		}
		for _, id := range ch.AllowFrom {
			if id == val {
				return fmt.Sprintf("%s already allows %s.", in.Channel, val), nil
			}
		}
		ch.AllowFrom = append(ch.AllowFrom, val)
	case "allow_remove":
		kept := ch.AllowFrom[:0]
		found := false
		for _, id := range ch.AllowFrom {
			if id == val {
				found = true
				continue
			}
			kept = append(kept, id)
		}
		if !found {
			return fmt.Sprintf("%s does not list %s.", in.Channel, val), nil
		}
		ch.AllowFrom = kept
	case "agent":
		if val != "" {
			if _, ok := settings.Agents[val]; !ok {
				return "", fmt.Errorf("no agent %q — create it first with gw_agent", val)
			}
		}
		ch.Agent = val
	case "tier":
		if val != "" && val != "strong" && val != "frontier" {
			return "", fmt.Errorf("tier must be empty, strong, or frontier")
		}
		ch.Tier = val
	case "pairing":
		on, err := parseAdminBool(val)
		if err != nil {
			return "", err
		}
		ch.Pairing = &on
	case "respond_to_all":
		on, err := parseAdminBool(val)
		if err != nil {
			return "", err
		}
		ch.RespondToAll = on
	case "voice_replies":
		if val != "off" && val != "in_kind" && val != "always" && val != "" {
			return "", fmt.Errorf("voice_replies must be off, in_kind, or always")
		}
		ch.VoiceReplies = val
	case "poll":
		if val != "" {
			if _, err := time.ParseDuration(val); err != nil {
				return "", fmt.Errorf("poll must be a duration like 30s: %w", err)
			}
		}
		ch.Poll = val
	case "projects":
		ch.Projects = nil
		for _, id := range strings.Split(val, ",") {
			if id = strings.TrimSpace(id); id != "" {
				if _, ok := settings.Projects[id]; !ok {
					return "", fmt.Errorf("unknown project %q — register it first with gw_project", id)
				}
				ch.Projects = append(ch.Projects, id)
			}
		}
	default:
		return "", fmt.Errorf("unknown field %q", in.Field)
	}
	settings.Channels[in.Channel] = ch
	if err := gwconfig.Save(settings); err != nil {
		return "", err
	}
	return fmt.Sprintf("Done: %s.%s = %q. The gateway picks this up within seconds.", in.Channel, in.Field, val), nil
}

func adminPairing(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Action string `json:"action"`
		Code   string `json:"code"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	if action != "approve" && action != "deny" {
		return "", fmt.Errorf("action must be approve or deny")
	}
	dir, err := gwconfig.Dir()
	if err != nil {
		return "", err
	}
	gw, err := gwstate.OpenShared(ctx, dir)
	if err != nil {
		return "", err
	}
	defer gw.Close()
	p, err := gw.TakePairing(ctx, code, time.Now())
	if err != nil {
		return "", err
	}
	if action == "deny" {
		return fmt.Sprintf("Denied %s user %s.", p.Channel, p.Principal), nil
	}
	settings, err := gwconfig.Load()
	if err != nil {
		return "", err
	}
	if settings.Channels == nil {
		settings.Channels = map[string]gwconfig.Channel{}
	}
	ch := settings.Channels[p.Channel]
	for _, id := range ch.AllowFrom {
		if id == p.Principal {
			return fmt.Sprintf("%s user %s was already allowed.", p.Channel, p.Principal), nil
		}
	}
	ch.AllowFrom = append(ch.AllowFrom, p.Principal)
	settings.Channels[p.Channel] = ch
	if err := gwconfig.Save(settings); err != nil {
		return "", err
	}
	return fmt.Sprintf("Approved %s user %s. The gateway picks this up within seconds.", p.Channel, p.Principal), nil
}

func adminProject(input json.RawMessage) (string, error) {
	var in struct {
		Action string `json:"action"`
		Path   string `json:"path"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	settings, err := gwconfig.Load()
	if err != nil {
		return "", err
	}
	switch action {
	case "add":
		path := strings.TrimSpace(in.Path)
		if path == "" {
			return "", fmt.Errorf("add needs a path")
		}
		if strings.HasPrefix(path, "~/") {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, path[2:])
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", abs)
		}
		id := strings.TrimSpace(in.ID)
		if id == "" {
			id = filepath.Base(abs)
		}
		if settings.Projects == nil {
			settings.Projects = map[string]gwconfig.Project{}
		}
		settings.Projects[id] = gwconfig.Project{Path: abs, Enabled: true}
		if settings.DefaultProject == "" {
			settings.DefaultProject = id
		}
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		return fmt.Sprintf("Registered project %s at %s.", id, abs), nil
	case "remove":
		id := strings.TrimSpace(in.ID)
		if _, ok := settings.Projects[id]; !ok {
			return "", fmt.Errorf("unknown project %q", id)
		}
		delete(settings.Projects, id)
		if settings.DefaultProject == id {
			settings.DefaultProject = ""
		}
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		return fmt.Sprintf("Removed project %s.", id), nil
	case "set_default":
		id := strings.TrimSpace(in.ID)
		if _, ok := settings.Projects[id]; !ok {
			return "", fmt.Errorf("unknown project %q", id)
		}
		settings.DefaultProject = id
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		return fmt.Sprintf("Default project is now %s.", id), nil
	}
	return "", fmt.Errorf("action must be add, remove, or set_default")
}

func adminAgent(input json.RawMessage) (string, error) {
	var in struct {
		Action    string `json:"action"`
		Name      string `json:"name"`
		Type      string `json:"type"`
		Model     string `json:"model"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", fmt.Errorf("a agent needs a name")
	}
	settings, err := gwconfig.Load()
	if err != nil {
		return "", err
	}
	switch action {
	case "add":
		typ := strings.TrimSpace(in.Type)
		if typ == "" {
			typ = "assistant"
		}
		if typ != "assistant" && typ != "coding" && typ != "research" {
			return "", fmt.Errorf("type must be assistant, coding, or research")
		}
		if settings.Agents == nil {
			settings.Agents = map[string]gwconfig.Agent{}
		}
		if r := strings.TrimSpace(in.Reasoning); r != "" && r != "off" && r != "medium" && r != "high" {
			return "", fmt.Errorf("reasoning must be off, medium, or high")
		}
		settings.Agents[name] = gwconfig.Agent{Type: typ, Model: strings.TrimSpace(in.Model), Reasoning: strings.TrimSpace(in.Reasoning)}
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		return fmt.Sprintf("Created agent %s (type %s). Bind a channel to it with gw_channel field=agent; its standing instructions live at ~/.memcode/agents/%s/MEMCODE.md.", name, typ, name), nil
	case "reasoning":
		p, ok := settings.Agents[name]
		if !ok {
			return "", fmt.Errorf("no agent %q", name)
		}
		r := strings.TrimSpace(in.Reasoning)
		if r != "" && r != "off" && r != "medium" && r != "high" {
			return "", fmt.Errorf("reasoning must be off, medium, or high (empty = automatic)")
		}
		p.Reasoning = r
		settings.Agents[name] = p
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		if r == "" {
			return fmt.Sprintf("Agent %s back on automatic per-turn reasoning.", name), nil
		}
		return fmt.Sprintf("Agent %s now thinks at %s effort everywhere it answers.", name, r), nil
	case "model":
		p, ok := settings.Agents[name]
		if !ok {
			return "", fmt.Errorf("no agent %q", name)
		}
		p.Model = strings.TrimSpace(in.Model)
		settings.Agents[name] = p
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		if p.Model == "" {
			return fmt.Sprintf("Agent %s back on automatic model routing.", name), nil
		}
		return fmt.Sprintf("Agent %s now runs on %s everywhere it answers.", name, p.Model), nil
	case "remove":
		if _, ok := settings.Agents[name]; !ok {
			return "", fmt.Errorf("no agent %q", name)
		}
		for chName, ch := range settings.Channels {
			if ch.Agent == name {
				return "", fmt.Errorf("agent %q is bound to channel %s — unbind it first", name, chName)
			}
		}
		delete(settings.Agents, name)
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		return fmt.Sprintf("Removed agent %s. Its home under ~/.memcode/agents is kept; delete it yourself if you want the memory gone.", name), nil
	}
	return "", fmt.Errorf("action must be add, remove, enable, or disable")
}

func adminSchedule(input json.RawMessage) (string, error) {
	var in struct {
		Action    string `json:"action"`
		Name      string `json:"name"`
		Cron      string `json:"cron"`
		Every     string `json:"every"`
		At        string `json:"at"`
		Task      string `json:"task"`
		DeliverTo string `json:"deliver_to"`
		Agent     string `json:"agent"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", fmt.Errorf("a schedule needs a name")
	}
	settings, err := gwconfig.Load()
	if err != nil {
		return "", err
	}
	switch action {
	case "add":
		spec := 0
		for _, v := range []string{in.Cron, in.Every, in.At} {
			if strings.TrimSpace(v) != "" {
				spec++
			}
		}
		if spec != 1 || strings.TrimSpace(in.Task) == "" {
			return "", fmt.Errorf("add needs a task and exactly one of cron, every, or at")
		}
		for _, sc := range settings.Schedules {
			if sc.Name == name {
				return "", fmt.Errorf("schedule %q already exists — remove it first to replace it", name)
			}
		}
		settings.Schedules = append(settings.Schedules, gwconfig.Schedule{
			Name: name, Cron: strings.TrimSpace(in.Cron), Every: strings.TrimSpace(in.Every),
			At:   strings.TrimSpace(in.At),
			Task: strings.TrimSpace(in.Task), DeliverTo: strings.TrimSpace(in.DeliverTo),
		})
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		return fmt.Sprintf("Scheduled %s.", name), nil
	case "enable", "disable":
		for i, sc := range settings.Schedules {
			if sc.Name == name {
				sc.Disabled = action == "disable"
				settings.Schedules[i] = sc
				if err := gwconfig.Save(settings); err != nil {
					return "", err
				}
				return fmt.Sprintf("Schedule %s %sd.", name, action), nil
			}
		}
		return "", fmt.Errorf("no schedule %q", name)
	case "remove":
		kept := settings.Schedules[:0]
		found := false
		for _, sc := range settings.Schedules {
			if sc.Name == name {
				found = true
				continue
			}
			kept = append(kept, sc)
		}
		if !found {
			return "", fmt.Errorf("no schedule %q", name)
		}
		settings.Schedules = kept
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		return fmt.Sprintf("Removed schedule %s.", name), nil
	}
	return "", fmt.Errorf("action must be add, remove, enable, or disable")
}

func parseAdminBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "on", "yes":
		return true, nil
	case "false", "off", "no":
		return false, nil
	}
	return false, fmt.Errorf("value must be true or false")
}
