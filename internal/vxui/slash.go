package vxui

import "strings"

// slashCmd is one entry in the `/` autocomplete menu. Mirrors the catalog in internal/tui;
// kept here so the vaxis renderer is self-contained until the two are unified at cutover.
// local marks commands that work SIGNED OUT (mandatory login): everything that
// never needs the gateway — display, session bookkeeping, local job control,
// and the login flow itself. Gateway/LLM-backed commands are gated in runSlash.
type slashCmd struct {
	name, desc string
	local      bool
}

var slashCommands = []slashCmd{
	{"/help", "show commands", true},
	{"/login", "sign in to memcode.ai", true},
	{"/logout", "sign out of this machine", true},
	{"/advisor", "outside second opinion (Claude Opus)", false},
	{"/next", "highest-value next move", false},
	{"/recap", "recap this session", false},
	{"/overview", "what this project is", false},
	{"/arch", "architecture / flow diagrams", true}, // renders the stored doc — no model call
	{"/doctor", "runtime health check", true},       // signed-out: local checks only
	{"/jobs", "background jobs", true},
	{"/tail", "tail a background job", true},
	{"/kill", "kill a background job", true},
	{"/status", "session status line", true},
	{"/plan", "plan mode", false},
	{"/yolo", "plan + auto-execute", false},
	{"/dispatch", "dispatch a hands-off sub-agent", false},
	{"/agents", "list dispatched sub-agents (or /agents stop <id>)", true},
	{"/sync", "sync memory to assistants", false},
	{"/mode", "change permission mode", true},
	{"/theme", "change display theme", true},
	{"/personality", "set the agent's voice", true},
	{"/extramile", "go above and beyond (edge cases + completeness)", true},
	{"/effort", "force thinking effort: off / medium / high / auto", true},
	{"/goal", "set an objective", false}, // objective intake runs a model call
	{"/model", "pick the model for this session", false},
	{"/policy", "how you've customized memcode, and where each setting came from", false},

	{"/apikeys", "bring your own provider API keys", false},
	{"/websites", "your AI-built websites (pull with `memcode websites`)", false},
	{"/artifacts", "your published artifact pages", false},
	{"/cost", "session spend", true},
	{"/costp", "spend by purpose", true},
	{"/costby", "spend by purpose", true},
	{"/todos", "show active task checklist", true},
	{"/debug", "runtime debug summary", true},
	{"/compact", "summarize older turns", false},
	{"/clear", "start a fresh session", true},
	{"/resume", "re-enter a previous session (latest, or /resume <id>)", true},
	{"/fork", "fork this conversation into a new session (or /fork <id>)", true},
	{"/rewind", "undo agent edits — pick a turn, then confirm", true},
	{"/quit", "exit", true},
}

// isLocalSlash reports whether a CANONICAL command name works signed out.
// Unknown commands count as local so the "unknown command" message (not a
// login nudge) is what the user sees.
func isLocalSlash(cmd string) bool {
	for _, c := range slashCommands {
		if c.name == cmd {
			return c.local
		}
	}
	return true
}

// slashAliases maps pure synonyms to their canonical command — the alias resolves to
// the same handler with identical behavior (unlike /costp /costby, which are distinct
// commands sharing a handler). Both the catalog (above) and this map are the ONLY
// sources of truth for "is this a slash command": recognition, autocomplete, and
// dispatch all flow from them, so the two can never drift again.
var slashAliases = map[string]string{
	"/?":      "/help",
	"/exit":   "/quit",
	"/new":    "/clear",
	"/todo":   "/todos",
	"/themes": "/theme",
	"/models": "/model", // both open the model picker
	"/keys":   "/apikeys",
	"/sites":  "/websites",
}

// adminSlash is the admin session's slash whitelist: display, session
// bookkeeping, auth, and model choice. Coding-session commands (plan, jobs,
// dispatch, rewind, …) don't exist there.
var adminSlash = map[string]bool{
	"/help": true, "/login": true, "/logout": true, "/doctor": true,
	"/status": true, "/mode": true, "/theme": true, "/personality": true,
	"/effort": true, "/model": true, "/apikeys": true, "/cost": true,
	"/costp": true, "/costby": true, "/debug": true, "/compact": true,
	"/clear": true, "/resume": true, "/fork": true, "/quit": true,
}

// catalogFor returns the slash catalog for the session kind.
func catalogFor(admin bool) []slashCmd {
	if !admin {
		return slashCommands
	}
	out := make([]slashCmd, 0, len(adminSlash))
	for _, c := range slashCommands {
		if adminSlash[c.name] {
			out = append(out, c)
		}
	}
	return out
}

// matchSlash returns commands whose name starts with prefix (e.g. "/mo"), for the menu.
func matchSlash(prefix string, admin bool) []slashCmd {
	prefix = strings.ToLower(prefix)
	var out []slashCmd
	for _, c := range catalogFor(admin) {
		if strings.HasPrefix(c.name, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// isKnownSlash reports whether the first token is a real command — so a pasted "/cost …"
// routes as a command while a pasted path ("/var/log/x") routes as text. The catalog and
// the alias map are the only sources of truth (see slashCommands / slashAliases), so
// recognition and autocomplete can't drift: add a command once and it's recognized.
func isKnownSlash(line string, admin bool) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	first := strings.ToLower(fields[0])
	for _, c := range catalogFor(admin) {
		if c.name == first {
			return true
		}
	}
	if canon, ok := slashAliases[first]; ok {
		return !admin || adminSlash[canon]
	}
	return false
}

// slashHelp is the compact command reference printed by /help.
func slashHelp(admin bool) string {
	var b strings.Builder
	b.WriteString("Commands\n")
	for _, c := range catalogFor(admin) {
		b.WriteString("  " + c.name)
		for i := len(c.name); i < 14; i++ {
			b.WriteByte(' ')
		}
		b.WriteString(c.desc + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
