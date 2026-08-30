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
	case tools.GwBrowser:
		return gwBrowser(ctx) // gateway-wide, not per-agent
	case tools.GwPolicy, tools.GwGrant, tools.GwWake, tools.GwInbox, tools.GwAnswer, tools.GwJournal, tools.GwDoctor:
		return adminAutonomy(ctx, name, input)
	}
	return "", fmt.Errorf("unknown admin tool %q", name)
}

// adminAutonomy dispatches the per-agent autonomy tools. They all need the
// agent's store and its configuration, so opening those is done once here.
func adminAutonomy(ctx context.Context, name string, input json.RawMessage) (string, error) {
	var in struct {
		Agent    string `json:"agent"`
		Action   string `json:"action"`
		Document string `json:"document"`
		Hash     string `json:"hash"`
		Type     string `json:"type"`
		Locator  string `json:"locator"`
		Mode     string `json:"mode"`
		ID       string `json:"id"`
		Answer   string `json:"answer"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	agent := strings.TrimSpace(in.Agent)
	if agent == "" {
		return "", fmt.Errorf("an agent name is required")
	}
	st, home, cfg, err := agentStore(ctx, agent)
	if err != nil {
		return "", err
	}
	defer st.Close()
	switch name {
	case tools.GwPolicy:
		return gwPolicy(ctx, st, home, agent, in.Action, in.Document, in.Hash)
	case tools.GwGrant:
		return gwGrant(ctx, st, home, in.Action, in.Type, in.Locator, in.Mode, in.ID)
	case tools.GwWake:
		return gwWake(ctx, st, home, agent, cfg)
	case tools.GwInbox:
		return gwInbox(ctx, st, agent)
	case tools.GwAnswer:
		return gwAnswer(ctx, st, home, agent, in.ID, in.Answer, cfg)
	case tools.GwJournal:
		return gwJournal(ctx, st)
	case tools.GwDoctor:
		return gwDoctor(ctx, st, home, agent, cfg)
	}
	return "", fmt.Errorf("unknown autonomy tool %q", name)
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
		a := settings.Agents[name]
		extra := ""
		if a.Autonomous {
			extra += " autonomous"
			if a.Paused {
				extra += "(paused)"
			}
		}
		if a.Objective != "" {
			extra += fmt.Sprintf(" objective=%q", trunc(a.Objective, 60))
		}
		if a.Browser != "" {
			extra += " browser=" + a.Browser
		}
		if a.Model != "" {
			extra += " model=" + a.Model
		}
		if a.Reasoning != "" {
			extra += " reasoning=" + a.Reasoning
		}
		if len(a.Toolsets) > 0 {
			extra += fmt.Sprintf(" toolsets=%v", a.Toolsets)
		}
		if len(a.DisabledToolsets) > 0 {
			extra += fmt.Sprintf(" disabled_toolsets=%v", a.DisabledToolsets)
		}
		fmt.Fprintf(&b, "- %s:%s (home: ~/.memcode/agents/%s)\n", name, extra, name)
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
	already, err := approvePairing(p)
	if err != nil {
		return "", err
	}
	if already {
		return fmt.Sprintf("%s user %s was already allowed.", p.Channel, p.Principal), nil
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
		// The SAME registration the CLI uses: canonical (symlink-resolved) root,
		// and an id collision with a different path is refused, never overwritten.
		id, root, err := settings.RegisterProject(in.ID, path)
		if err != nil {
			return "", err
		}
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		return fmt.Sprintf("Registered project %s at %s.", id, root), nil
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
		Action           string `json:"action"`
		Name             string `json:"name"`
		Model            string `json:"model"`
		Reasoning        string `json:"reasoning"`
		Toolsets         string `json:"toolsets"`
		DisabledToolsets string `json:"disabled_toolsets"`
		Objective        string `json:"objective"`
		Autonomous       string `json:"autonomous"`
		Browser          string `json:"browser"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", fmt.Errorf("an agent needs a name")
	}
	settings, err := gwconfig.Load()
	if err != nil {
		return "", err
	}
	switch action {
	case "add":
		if settings.Agents == nil {
			settings.Agents = map[string]gwconfig.Agent{}
		}
		if r := strings.TrimSpace(in.Reasoning); r != "" && r != "off" && r != "medium" && r != "high" {
			return "", fmt.Errorf("reasoning must be off, medium, or high")
		}
		if _, ok := settings.Agents[name]; ok {
			return "", fmt.Errorf("agent %q already exists", name)
		}
		br, err := parseBrowser(in.Browser)
		if err != nil {
			return "", err
		}
		settings.Agents[name] = gwconfig.Agent{
			Model: strings.TrimSpace(in.Model), Reasoning: strings.TrimSpace(in.Reasoning),
			Objective: strings.TrimSpace(in.Objective), Browser: br,
		}
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		msg := fmt.Sprintf("Created agent %s. Bind a channel to it with gw_channel field=agent; its identity lives at ~/.memcode/agents/%s/SOUL.md.", name, name)
		if strings.TrimSpace(in.Objective) != "" {
			// Deliberately NOT autonomous yet: holding an objective and being
			// allowed to act on it unprompted are separate grants, and the second
			// one deserves its own explicit confirmation.
			msg = fmt.Sprintf("Created agent %s with an objective. It is NOT yet autonomous — it will only run when you ask (gw_wake). To let it run on its own: gw_agent action=autonomous, then approve a policy with gw_policy, then give it a cadence with gw_schedule.", name)
		}
		return msg, nil
	case "objective":
		p, ok := settings.Agents[name]
		if !ok {
			return "", fmt.Errorf("no agent %q", name)
		}
		p.Objective = strings.TrimSpace(in.Objective)
		settings.Agents[name] = p
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		if p.Objective == "" {
			return fmt.Sprintf("Cleared %s's objective; it stays an ordinary agent.", name), nil
		}
		return fmt.Sprintf("Objective for %s: %s", name, p.Objective), nil
	case "autonomous":
		p, ok := settings.Agents[name]
		if !ok {
			return "", fmt.Errorf("no agent %q", name)
		}
		on := isTrue(in.Autonomous)
		p.Autonomous = on
		settings.Agents[name] = p
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		if !on {
			return fmt.Sprintf("%s will no longer run unattended. Scheduled wakes stop; it still answers on demand.", name), nil
		}
		return fmt.Sprintf("%s may now run unattended: every run is policy-gated, journals consequential actions, and suspends durably on a question instead of prompting. It still needs an approved policy (gw_policy) before it can do anything consequential.", name), nil
	case "browser":
		p, ok := settings.Agents[name]
		if !ok {
			return "", fmt.Errorf("no agent %q", name)
		}
		br, err := parseBrowser(in.Browser)
		if err != nil {
			return "", err
		}
		p.Browser = br
		settings.Agents[name] = p
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		if br == gwconfig.BrowserExistingChrome {
			return fmt.Sprintf("%s will drive your OWN running Chrome, inheriting your signed-in sessions. Check it works with gw_browser; if the broker isn't reachable, browser work fails closed rather than falling back to a logged-out profile.", name), nil
		}
		return fmt.Sprintf("%s uses a fresh, logged-out browser profile per run.", name), nil
	case "pause", "resume":
		p, ok := settings.Agents[name]
		if !ok {
			return "", fmt.Errorf("no agent %q", name)
		}
		p.Paused = action == "pause"
		settings.Agents[name] = p
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		if p.Paused {
			return fmt.Sprintf("%s paused — no further unattended wakes. Nothing deleted; resume any time.", name), nil
		}
		return fmt.Sprintf("%s resumed.", name), nil
	case "tools":
		p, ok := settings.Agents[name]
		if !ok {
			return "", fmt.Errorf("no agent %q", name)
		}
		allow, deny := splitPolicyList(in.Toolsets), splitPolicyList(in.DisabledToolsets)
		var bad []string
		for _, e := range append(append([]string{}, allow...), deny...) {
			if !tools.ValidPolicyEntry(e) {
				bad = append(bad, e)
			}
		}
		if len(bad) > 0 {
			return "", fmt.Errorf("unknown toolsets/tools: %s (valid toolsets: %s)", strings.Join(bad, ", "), strings.Join(tools.ToolsetNames(), ", "))
		}
		p.Toolsets, p.DisabledToolsets = allow, deny
		settings.Agents[name] = p
		if err := gwconfig.Save(settings); err != nil {
			return "", err
		}
		if len(allow) == 0 && len(deny) == 0 {
			return fmt.Sprintf("Agent %s back to the full toolbox.", name), nil
		}
		return fmt.Sprintf("Agent %s tool policy set (allow: %v, disabled: %v).", name, allow, deny), nil
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
	return "", fmt.Errorf("action must be add, objective, autonomous, browser, pause, resume, tools, reasoning, model, or remove")
}

// parseBrowser validates the browser backend name, defaulting to ephemeral.
func parseBrowser(s string) (string, error) {
	switch v := strings.TrimSpace(s); v {
	case "", gwconfig.BrowserEphemeral:
		return "", nil // empty == ephemeral; don't write the default into config
	case gwconfig.BrowserExistingChrome:
		return v, nil
	default:
		return "", fmt.Errorf("browser must be %s or %s", gwconfig.BrowserEphemeral, gwconfig.BrowserExistingChrome)
	}
}

// isTrue reads a boolean carried as a string through a tool call. Anything but
// an explicit yes is false — granting unattended authority must never happen by
// typo.
func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1":
		return true
	}
	return false
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
		// A schedule aimed at an agent with no explicit destination delivers to
		// the agent itself: its report is journaled in its home rather than sent
		// to a chat. This is what lets ONE scheduler drive both channel replies
		// and unattended agent wakes, instead of a second cron implementation
		// just for autonomous agents.
		deliverTo := strings.TrimSpace(in.DeliverTo)
		if deliverTo == "" && strings.TrimSpace(in.Agent) != "" {
			if a, ok := settings.Agents[strings.TrimSpace(in.Agent)]; ok && a.Autonomous {
				deliverTo = "agent:" + strings.TrimSpace(in.Agent)
			}
		}
		// The SAME validated construction the CLI uses (cron/every/at parsing,
		// deliver_to shape, duplicate names) — the surfaces cannot drift.
		sc, err := gwconfig.BuildSchedule(name, in.Cron, in.Every, in.At, "", in.Task, deliverTo, in.Agent, time.Now())
		if err != nil {
			return "", err
		}
		if err := settings.AddSchedule(sc); err != nil {
			return "", err
		}
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

// splitPolicyList parses a comma-separated policy list, trimming blanks.
func splitPolicyList(v string) []string {
	var out []string
	for _, e := range strings.Split(v, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
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
