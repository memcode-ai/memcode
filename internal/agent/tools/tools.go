// Package tools declares the typed tool registry exposed to the model. The
// definitions (names + JSON schemas) live here; the runtime owns execution so
// that permission gating, event capture and post-command diff detection stay in
// one place. v0 registry: read_file, ripgrep, bash, git_diff, edit_file.
package tools

import (
	"bytes"
	"encoding/json"

	"github.com/memcode-ai/memcode/internal/wire"
)

// Tool names (stable identifiers shared by the model and the runtime).
const (
	ReadFile         = "read_file"
	ListDir          = "list_dir"
	Glob             = "glob" // recursive file discovery by name pattern (repofiles-backed; excludes hidden/ignored)
	Ripgrep          = "ripgrep"
	CodeQuery        = "code_query" // deterministic "where does X live" — ranked evidence in one call (no model loop)
	Bash             = "bash"
	MCPCodeExec      = "mcp_code_exec" // orchestrate the connected MCP servers from a Python script ("code execution with MCP"); only the printed result returns
	GitDiff          = "git_diff"
	EditFile         = "edit_file"
	ApplyPatch       = "apply_patch"       // multi-file, all-or-nothing edits (coherent refactor lands whole or rolls back)
	MCP              = "mcp"               // the MCP meta-tool: search/schema/call over connected servers (schemas disclosed on demand, never inlined)
	MCPResource      = "mcp_resource"      // list/read MCP resources (external context: docs, schemas, runbooks)
	MCPPrompt        = "mcp_prompt"        // list/get MCP prompt templates
	GitHub           = "github"            // typed PR/issue/CI surface over gh (structured, not shell-string fiddling)
	RunTests         = "run_tests"         // run the repo's tests and return a STRUCTURED pass/fail summary (not raw log)
	Diagnostics      = "diagnostics"       // type/compile diagnostics for a file or the repo (gopls/go build, tsc) — see errors without guessing
	CodeNav          = "code_nav"          // semantic navigation via LSP: go-to-definition, find-references, hover-for-type
	RepoMap          = "repo_map"          // ranked, token-budgeted symbol map (personalized PageRank over the reference graph)
	Memcode          = "memcode"           // dispatcher into memcode's own intelligence (read-only)
	Todo             = "todo"              // dispatcher for the agent's own work-tracker checklist
	Explore          = "explore"           // spawn a read-only sub-agent to investigate one facet (map/reduce)
	AskUser          = "ask_user"          // human-in-the-loop: ask the user a clarifying question on a critical fork
	WebSearch        = "web_search"        // search the web for a query
	Fetch            = "fetch"             // fetch a specific URL and return its text
	Skill            = "skill"             // recruit an installed skill's expert guidance on demand (gated)
	Script           = "script"            // save/find/run/delete a reusable multi-step command sequence (each gated ONCE at the script level, not per inner command)
	Artifact         = "artifact"          // publish/update/list/delete a self-contained HTML page hosted at memcode.ai (publish gated)
	Knowledge        = "knowledge"         // consult memcode's baseline facts/idioms for a stack (ungated reference)
	Trace            = "trace"             // trace an artifact across pipeline stages to locate where data is lost (alias: wiretap)
	EnterPlan        = "enter_plan"        // switch into research-and-approve plan mode (offered in normal chat only)
	CancelPlan       = "cancel_plan"       // CANCEL/ABANDON plan mode on the user's behalf (plan mode only; never executes, never presents). Renamed from exit_plan: models carry a Claude Code prior where "exit plan mode" means "present the plan" — that name got plans silently abandoned.
	ExecutePlan      = "execute_plan"      // APPROVE+execute the plan when the user says so (plan mode only) → apply phase
	Dispatch         = "dispatch"          // offload a discrete block of work to a hands-off background sub-agent
	Agent            = "agent"             // first-class sub-agent: run a task on a chosen tier (fast/strong), report the result back
	RecallPlan       = "recall_plan"       // list / retrieve a previously saved plan from the user-level plans store
	Reasoning        = "reasoning"         // adaptive reasoning: inspect/adjust OWN thinking depth mid-turn, or delegate a hard sub-problem to the strong reasoning model
	PreferenceSignal = "preference_signal" // capture a durable user taste/constraint to remember (LLM-captured, reducer-promoted)

	// Browser tools — only advertised when --chrome is set (gated in toolDefs).
	BrowserNavigate   = "browser_navigate"   // load a URL in the current Chrome tab
	BrowserClick      = "browser_click"      // click an element by CSS selector
	BrowserType       = "browser_type"       // type text into an element by CSS selector
	BrowserScreenshot = "browser_screenshot" // capture the current page as an image (vision)
	BrowserEval       = "browser_eval"       // run JavaScript in the page and return the result
	BrowserText       = "browser_text"       // get the visible text content of the current page
	BrowserScroll     = "browser_scroll"     // scroll the page by x/y pixel deltas
	BrowserPressKey   = "browser_press_key"  // send a keyboard event (Enter, Escape, Tab, arrows)
	BrowserHover      = "browser_hover"      // hover over an element (trigger hover states/menus)
	BrowserSelect     = "browser_select"     // pick an option in a <select> dropdown
	BrowserBack       = "browser_back"       // navigate to the previous page in history
	BrowserForward    = "browser_forward"    // navigate to the next page in history
	BrowserConsole    = "browser_console"    // read captured console.log/error messages
	BrowserNewTab     = "browser_new_tab"    // open a new tab and switch to it
	BrowserSwitchTab  = "browser_switch_tab" // switch focus to a different tab
	BrowserCloseTab   = "browser_close_tab"  // close a tab
	BrowserListTabs   = "browser_list_tabs"  // list all open tabs
	BrowserWait       = "browser_wait"       // wait for a selector to become visible/hidden/ready (settles SPA navigation)
	BrowserUpload     = "browser_upload"     // set a file input's file (project files only, symlink-safe)
	BrowserResize     = "browser_resize"     // set the viewport dimensions (responsive testing)
)

// MemcodeCommands are the introspection subcommands the `memcode` tool dispatches
// to. One tool, one schema — cheap context, structured calls (no shell strings).
var MemcodeCommands = []string{
	"overview", "map", "context", "why", "recall", "next", "recap", "memories", "claims", "sources", "session", "acceptance", "doctor", "jobs", "preferences",
}

// PreferenceAxes is the vocabulary for preference_signal's `axis` field. The axis
// drives clustering in the reducer — same-axis signals are candidates to merge.
var PreferenceAxes = []string{"workflow", "gating", "verbosity", "style", "tooling"}

// TodoActions are the actions the `todo` tool dispatches to.
var TodoActions = []string{"create", "add", "start", "done", "block", "skip", "update", "show"}

// Input payloads, one per tool.
type (
	ReadFileInput struct {
		Path string `json:"path"`
		// Optional 1-based inclusive line range — a re-verification read fetches
		// the region it needs instead of re-paying the whole file. 0 = unset.
		StartLine int `json:"start_line,omitempty"`
		EndLine   int `json:"end_line,omitempty"`
		// Attach (PDFs only): send the file ITSELF to the model as a native
		// document instead of locally extracted text — for layout/charts/scans.
		// Costs far more tokens (per-page image billing), so it's opt-in.
		Attach bool `json:"attach,omitempty"`
	}
	// EnterPlanInput optionally requests yolo planning (auto-resolve questions + auto-execute on
	// approval). cancel_plan takes no input — it's cancel-only.
	EnterPlanInput struct {
		Yolo bool `json:"yolo,omitempty"`
	}
	// ReasoningInput drives the adaptive-reasoning tool: bare effort = self-adjust
	// (or report when everything is empty); task (+context, +effort) = delegate to
	// the strong reasoning model.
	ReasoningInput struct {
		Task    string `json:"task,omitempty"`
		Context string `json:"context,omitempty"`
		Effort  string `json:"effort,omitempty"` // off | medium | high | auto
	}
	// PreferenceSignalInput captures a durable user taste/constraint the model wants
	// memcode to remember. The reducer clusters these by axis + lexical similarity
	// and promotes a cluster to a standing preference once it crosses the evidence
	// bar (≥3 signals, ≥2 sessions, weighted score ≥ 2.0). Call this when the user
	// states a FORCEFUL, REPEATED directive ("always X", "never Y", "stop doing Z")
	// — NOT for one-off tasks, explorations, or ordinary preferences.
	PreferenceSignalInput struct {
		Text  string `json:"text"`
		Axis  string `json:"axis"`
		Scope string `json:"scope,omitempty"`
	}
	// AgentInput drives the first-class agent tool: run a self-contained task on a chosen tier,
	// read-only or mutating, and get the result back.
	AgentInput struct {
		Task       string `json:"task"`                 // the self-contained instruction for the sub-agent
		Context    string `json:"context,omitempty"`    // optional background the sub-agent needs (it starts fresh)
		ReadOnly   bool   `json:"readonly,omitempty"`   // true = investigate/generate only (no edits); false = a full mutating agent
		Background bool   `json:"background,omitempty"` // true = run detached and report the result back when done (don't block this turn)
	}
	// DispatchInput offloads a discrete block of work to a hands-off background sub-agent.
	// The sub-agent runs the full mutating agent loop (edits, bash, tests) with NO prompts
	// or clarifying questions — 100% autonomous. It reports back a summary when done.
	DispatchInput struct {
		Task string `json:"task"`
		Mode string `json:"mode,omitempty"` // "auto" (default) | "allow-all"
	}
	// RecallPlanInput retrieves a saved plan. Empty slug → the most recent plan (plus a list of
	// older ones); a slug → that specific plan's markdown.
	RecallPlanInput struct {
		Slug string `json:"slug,omitempty"`
	}
	ListDirInput struct {
		Path string `json:"path,omitempty"`
	}
	GlobInput struct {
		Pattern       string `json:"pattern"`
		IncludeHidden bool   `json:"include_hidden,omitempty"`
		MaxResults    int    `json:"max_results,omitempty"`
	}
	RipgrepInput struct {
		Query string `json:"query"`
		Path  string `json:"path,omitempty"`
	}
	BashInput struct {
		Command    string `json:"command"`
		Cwd        string `json:"cwd,omitempty"`
		Background bool   `json:"background,omitempty"` // long-running (dev server/watcher): run detached, don't block
	}
	// MCPMCPCodeExecInput drives the mcp_code_exec tool: a Python 3 script that calls a
	// whitelisted set of read-only memcode tools as functions and prints ONLY the
	// distilled answer. save_skill (+ description) optionally persists the script
	// as a reusable skill under .memcode/skills/ — only when the user asked for it.
	MCPCodeExecInput struct {
		Script               string `json:"script"`
		SaveSkill            string `json:"save_skill,omitempty"`             // slug (lowercase-hyphen) to save the script as a skill
		SaveSkillDescription string `json:"save_skill_description,omitempty"` // one-line description for the saved skill
	}
	GitDiffInput struct {
		Path string `json:"path,omitempty"`
	}
	EditFileInput struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string,omitempty"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all,omitempty"`
	}
	// ApplyPatchInput is a set of edits applied ATOMICALLY: validate all, apply all, or
	// roll back all. For a coherent multi-file change (rename + all call sites) that must
	// not half-land. Each edit is the same shape as edit_file.
	ApplyPatchInput struct {
		Edits []EditFileInput `json:"edits"`
	}
	// MCPInput drives the mcp meta-tool: action "search" (find tools by query), "schema"
	// (one tool's input schema on demand), or "call" (invoke — gated per call).
	MCPInput struct {
		Action string          `json:"action"`          // search | schema | call
		Query  string          `json:"query,omitempty"` // search filter (empty = list all)
		Tool   string          `json:"tool,omitempty"`  // required for schema/call
		Args   json.RawMessage `json:"args,omitempty"`  // call arguments per the tool's schema
	}
	// MCPResourceInput drives the mcp_resource tool: action "list" (catalog) or "read"
	// (fetch a resource's contents by uri).
	MCPResourceInput struct {
		Action string `json:"action"`        // list | read
		URI    string `json:"uri,omitempty"` // required for read
	}
	// MCPPromptInput drives the mcp_prompt tool: action "list" (catalog) or "get" (render a
	// template with args).
	MCPPromptInput struct {
		Action string            `json:"action"`         // list | get
		Name   string            `json:"name,omitempty"` // required for get (namespaced)
		Args   map[string]string `json:"args,omitempty"` // template arguments for get
	}
	// RunTestsInput drives the run_tests tool: optional path (a package/dir to scope to)
	// and run (a name pattern to filter). Empty = the whole repo's tests.
	RunTestsInput struct {
		Path string `json:"path,omitempty"` // package/dir to scope the run (default: whole repo)
		Run  string `json:"run,omitempty"`  // test-name pattern to filter (go -run / pytest -k / jest -t)
	}
	// DiagnosticsInput drives the diagnostics tool: optional path (a file or dir) to scope
	// the check. Empty = the whole repo.
	DiagnosticsInput struct {
		Path string `json:"path,omitempty"`
	}
	// CodeNavInput drives the code_nav tool: a semantic query at a 1-based line/col in a
	// file. action = definition | references | hover | impact. depth applies to impact.
	CodeNavInput struct {
		Action string `json:"action"`
		Path   string `json:"path"`
		Line   int    `json:"line"`
		Col    int    `json:"col"`
		Depth  int    `json:"depth,omitempty"`
	}
	// GitHubInput drives the github tool — a typed surface over `gh`. Read actions
	// (pr_view/pr_list/issue_view/issue_list/checks) run directly; write actions
	// (pr_create/comment) go through the permission gate.
	GitHubInput struct {
		Action string `json:"action"`           // pr_view | pr_list | issue_view | issue_list | checks | pr_create | comment
		Number int    `json:"number,omitempty"` // PR/issue number (pr_view/issue_view/comment; checks uses current branch if 0)
		Title  string `json:"title,omitempty"`  // pr_create
		Body   string `json:"body,omitempty"`   // pr_create / comment
		Base   string `json:"base,omitempty"`   // pr_create base branch (default the repo default)
	}

	// ArtifactInput drives the artifact tool: publish a LOCAL self-contained HTML
	// file as a shareable page at memcode.ai/code/artifact/<id>.
	ArtifactInput struct {
		Action string `json:"action"`          // publish | update | list | delete
		Path   string `json:"path,omitempty"`  // repo-relative path to the HTML file (publish/update)
		Title  string `json:"title,omitempty"` // display title (publish; optional on update)
		ID     string `json:"id,omitempty"`    // artifact id from a prior publish (update/delete)
	}
	MemcodeInput struct {
		Command string `json:"command"`
		Target  string `json:"target,omitempty"` // path/subsystem for context/why
		Query   string `json:"query,omitempty"`  // question for recall
		Limit   int    `json:"limit,omitempty"`
	}
	// TodoInput drives the work-tracker. `items` carries the (re)written list for
	// create/update; `index` (1-based) targets a single item for done/block.
	TodoInput struct {
		Action  string         `json:"action"`
		Items   []TodoItemWire `json:"items,omitempty"`
		Index   int            `json:"index,omitempty"`
		Indices []int          `json:"indices,omitempty"` // done/skip several at once (one sweep → one call)
	}
	TodoItemWire struct {
		Title  string `json:"title"`
		Detail string `json:"detail,omitempty"`
		Status string `json:"status,omitempty"`
	}
	// CodeQueryInput locates code by a natural-language question, deterministically.
	CodeQueryInput struct {
		Query string `json:"query"`
		Scope string `json:"scope,omitempty"` // optional path prefix to focus on
	}
	// RepoMapInput drives the repo_map tool: optional focus terms (paths or symbol
	// names, space-separated) to center the map on, and a token budget.
	RepoMapInput struct {
		Focus        string `json:"focus,omitempty"`
		BudgetTokens int    `json:"budget_tokens,omitempty"`
	}
	// ExploreInput dispatches a read-only sub-agent investigation of one facet.
	ExploreInput struct {
		Question string `json:"question"`
		Scope    string `json:"scope,omitempty"` // optional subsystem/path to focus on
		Focus    string `json:"focus,omitempty"` // optional angle (e.g. "entrypoints, schema, tests")
	}
	// AskUserInput poses a clarifying question to the user (human-in-the-loop).
	AskUserInput struct {
		Question string      `json:"question"`
		Options  []AskOption `json:"options,omitempty"` // 2-4 choices; the user may also type their own
	}
	// AskOption is one candidate answer: a concise Label (the choice the user picks)
	// and an optional Description (a muted clarifying line shown beneath it, à la a
	// Claude-Code selector). It decodes tolerantly from EITHER a plain JSON string
	// (→ Label, no description) or an object {label, description}, because models are
	// inconsistent about which they emit — see UnmarshalJSON.
	AskOption struct {
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
	}
	// SkillInput drives the skill tool: FIND/LOAD work on already-installed skills; DISCOVER/INSTALL
	// reach the skills.sh catalog (search any agent's published skills, then install one on demand).
	SkillInput struct {
		Find     string `json:"find,omitempty"`     // search INSTALLED skills (local) → matching names
		Load     string `json:"load,omitempty"`     // exact installed skill name → pull its guidance in (gated)
		Discover string `json:"discover,omitempty"` // search the skills.sh CATALOG (remote, no install) → packages
		Install  string `json:"install,omitempty"`  // install "owner/repo@skill" from the catalog (gated) → then loadable
	}
	// KnowledgeInput drives the knowledge tool: FIND packs by topic, or get one by name/topic.
	KnowledgeInput struct {
		Find  string `json:"find,omitempty"`  // search query → matching pack names
		Topic string `json:"topic,omitempty"` // exact pack name → its full Facts + Idioms (ungated)
	}
	// ScriptInput drives the script tool: SAVE/DELETE/RUN each get exactly ONE coarse
	// permission decision (never a deep look inside the script — its commands were already
	// approved, per-command, the moment they first ran live); LIST/FIND are read-only.
	ScriptInput struct {
		Save        string `json:"save,omitempty"`        // slug (lowercase-hyphen) to save/update — pairs with description + command (gated)
		Description string `json:"description,omitempty"` // one-line description for `save`
		Command     string `json:"command,omitempty"`     // the command body for `save`
		Run         string `json:"run,omitempty"`         // exact saved slug to execute (gated once, at the script level — its contents are NOT re-classified)
		Background  bool   `json:"background,omitempty"`  // `run`: start detached instead of blocking (long-running scripts)
		List        bool   `json:"list,omitempty"`        // list every saved script (read-only)
		Find        string `json:"find,omitempty"`        // search saved scripts by topic (read-only)
		Delete      string `json:"delete,omitempty"`      // exact saved slug to remove — soft-deleted to .trash (gated)
	}
	// WebSearchInput searches the web for a query.
	WebSearchInput struct {
		Query string `json:"query"`
	}
	// FetchInput fetches a specific URL and returns its (text) content.
	FetchInput struct {
		URL string `json:"url"`
	}
	// TraceInput traces an artifact across pipeline stages to locate data loss.
	TraceInput struct {
		Target string `json:"target"` // a URL (traces the fetch pipeline) or a file path
	}
	// BrowserNavigateInput loads a URL in the Chrome tab.
	BrowserNavigateInput struct {
		URL string `json:"url"`
	}
	// BrowserClickInput clicks an element by CSS selector.
	BrowserClickInput struct {
		Selector string `json:"selector"`
	}
	// BrowserTypeInput types text into an element by CSS selector.
	BrowserTypeInput struct {
		Selector string `json:"selector"`
		Text     string `json:"text"`
		// Append keeps the field's existing value; default clears it first.
		Append bool `json:"append,omitempty"`
	}
	// BrowserWaitInput waits for an element to reach a state.
	BrowserWaitInput struct {
		Selector       string `json:"selector"`
		State          string `json:"state,omitempty"`           // visible (default) | hidden | ready
		TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // default 30, max 60
	}
	// BrowserUploadInput sets a file input's file.
	BrowserUploadInput struct {
		Selector string `json:"selector"`
		Path     string `json:"path"` // inside the project root (resolved through symlinks)
	}
	// BrowserResizeInput sets the viewport size.
	BrowserResizeInput struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	// BrowserScreenshotInput captures the current page as a PNG image (vision).
	// Default is the viewport; full_page captures the entire scrollable page (larger — more tokens).
	BrowserScreenshotInput struct {
		FullPage bool `json:"full_page,omitempty"`
	}
	// BrowserEvalInput runs a JavaScript expression in the page and returns the result.
	BrowserEvalInput struct {
		Script string `json:"script"`
	}
	// BrowserScrollInput scrolls the page by pixel deltas.
	BrowserScrollInput struct {
		DX int `json:"dx,omitempty"` // horizontal pixels (positive = right)
		DY int `json:"dy,omitempty"` // vertical pixels (positive = down)
	}
	// BrowserPressKeyInput sends a keyboard event to the page.
	BrowserPressKeyInput struct {
		Key string `json:"key"` // "Enter", "Escape", "Tab", "Backspace", "Space", "ArrowUp/Down/Left/Right", or a single char
	}
	// BrowserHoverInput hovers over an element by CSS selector.
	BrowserHoverInput struct {
		Selector string `json:"selector"`
	}
	// BrowserSelectInput picks an option in a <select> by value.
	BrowserSelectInput struct {
		Selector string `json:"selector"`
		Value    string `json:"value"`
	}
	// BrowserConsoleInput reads captured console messages.
	BrowserConsoleInput struct {
		Level string `json:"level,omitempty"` // filter by level: log, error, warn, info, debug, exception
	}
	// BrowserSwitchTabInput switches to a tab by 1-based index.
	BrowserSwitchTabInput struct {
		Index int `json:"index"` // 1-based tab number
	}
	// BrowserCloseTabInput closes a tab by 1-based index.
	BrowserCloseTabInput struct {
		Index int `json:"index"` // 1-based tab number
	}
)

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func intSchema(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// Defs returns the tool schemas advertised to the model.
func Defs() []wire.ToolDef {
	return []wire.ToolDef{
		{
			Name:        ReadFile,
			Description: "Read a UTF-8 text file from the project. Returns its contents (truncated if very large). For RE-VERIFICATION of a file you've already read — or when you know the region you need — pass start_line/end_line to fetch just that slice (a one-line header names the range): far cheaper, and it stays in context longer. Whole-file reads are for first contact. PDFs work too: by default the text is extracted locally (cheap); pass attach:true ONLY when you need the document itself — layout, charts, images, or a scan with no text layer (much more expensive: per-page image billing).",
			InputSchema: obj(map[string]any{
				"path":       str("path relative to the repo root"),
				"start_line": intSchema("first line of an optional range (1-based, inclusive)"),
				"end_line":   intSchema("last line of the range (inclusive; defaults to end of file)"),
				"attach":     boolean("PDFs only: attach the document itself for native reading (layout/charts/scans) instead of extracted text — costlier, opt-in"),
			}, "path"),
		},
		{
			Name:        ListDir,
			Description: "List the entries (files and subdirectories) of a directory. A read-only way to explore structure — prefer this over `bash ls`/`find`. Hidden/ignored entries (.git, .memcode, dotfiles) are skipped unless you pass a hidden path explicitly.",
			InputSchema: obj(map[string]any{
				"path": str("directory path relative to the repo root (default: the repo root)"),
			}),
		},
		{
			Name:        Glob,
			Description: "Find files by name/path GLOB, recursively — the DEFAULT way to discover files by name (e.g. `**/*.md`, `internal/**/*_test.go`, `*.go`). Use this instead of `bash find`. Respects .gitignore and excludes hidden/tooling/state dirs (.git, .memcode, .claude, node_modules, build output) — set include_hidden, or name a hidden dir in the pattern (`.github/**`), to include them.",
			InputSchema: obj(map[string]any{
				"pattern":        str("a glob like **/*.md, *.go, internal/**/*_test.go"),
				"include_hidden": boolean("include hidden/tooling dirs (.git, .memcode, .github); default false"),
				"max_results":    map[string]any{"type": "integer", "description": "cap on results (default 200)"},
			}, "pattern"),
		},
		{
			Name:        Ripgrep,
			Description: "Search file contents for a string/regex. Prefer this over `bash` for searching.",
			InputSchema: obj(map[string]any{
				"query": str("text or regular expression to search for"),
				"path":  str("optional path to scope the search"),
			}, "query"),
		},
		{
			Name:        CodeQuery,
			Description: "LOCATE code by a natural-language question — the first move when you don't know where something lives. Deterministic: returns the likely files ranked with evidence lines and confidence, favoring definitions over mentions. One call replaces a grep→read→grep loop; then read the top hits. Read-only, cheap.",
			InputSchema: obj(map[string]any{
				"query": str("a natural-language question about where something is or how it works"),
				"scope": str("optional path prefix to focus on (e.g. internal/tui)"),
			}, "query"),
		},
		{
			Name:        RepoMap,
			Description: "Ranked, token-budgeted map of the repo's important symbols and how heavily they're referenced (personalized PageRank; Go parsed natively, TS/JS/Python via language servers — a header note names any missing server). Your FIRST move to orient in an unfamiliar repo or subsystem, before targeted reads; complements code_query (locates one thing) and code_nav impact (one symbol's callers). `focus` (paths or symbol names, space-separated) zooms the ranking; dirty files boost automatically. Read-only, fast.",
			InputSchema: obj(map[string]any{
				"focus":         str("optional paths or symbol names (space-separated) to center the map on, e.g. `internal/tui composer`"),
				"budget_tokens": map[string]any{"type": "integer", "description": "digest size in tokens (default 1200, max 1800)"},
			}),
		},
		{
			Name:        Bash,
			Description: "Run a shell command (sh -c) for ACTIONS: builds, tests, installs, git, and live/external lookups (cloud CLIs, DB queries). Not for reading or searching repo files — use read_file/glob/ripgrep/git_diff for those; and for database inspection prefer one SQL query (psql -c, supabase db query) over probing a REST API table-by-table. Commands that don't exit (dev servers, watchers, log tails) MUST set background:true — they run as a managed job (/jobs, /tail, /kill); never foreground them, and when the user asks you to start a dev server, do it this way rather than refusing. When scanning files in bash, exclude hidden/ignored dirs (.git, .memcode, node_modules) unless asked. Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"command":    str("the shell command to run"),
				"cwd":        str("optional working directory relative to the repo root"),
				"background": boolean("run as a detached background job (anything long-running); returns immediately, and the job's EXIT hands its result back to you as a new turn — so a poll loop must break on success and bound its total wait"),
			}, "command"),
		},
		{
			Name:        MCPCodeExec,
			Description: "Run a short Python 3 script that orchestrates the connected MCP servers' tools and returns ONLY what it prints — intermediates never enter the conversation. This is CODE EXECUTION WITH MCP: use it when an external-tool workflow needs several MCP calls whose raw outputs you don't need to see (query-filter-join across services, pagination loops, cross-referencing two servers). It is NOT for repo work — the standard tools (read_file, ripgrep, glob, …) are deliberately NOT available in the script; call them directly. Available functions: search_tools(query) and tool_schema(name) for progressive discovery, mcp(tool, **args) to call an MCP tool (each tool's first use prompts the user once per run; a denial is an answer, adjust rather than retry), plus call(tool, **args) and gather((tool, kwargs), ...) which runs MCP calls in parallel, results in request order. The script's own file I/O uses a persistent workspace (.memcode/codeexec/) that survives across calls. Sandboxed with NO network beyond gated mcp calls (don't import urllib/requests) and no repo tools (read/edit/bash directly yourself). Print a distilled result — the reply truncates at 64KB. save_skill (+ save_skill_description) persists the script as a reusable skill, ONLY when the user asked.",
			InputSchema: obj(map[string]any{
				"script":                 str("the Python 3 script to run (top-level statements; print the distilled result)"),
				"save_skill":             str("optional: persist this script as a reusable skill with this slug — only when the user asked"),
				"save_skill_description": str("one-line description for the saved skill (required with save_skill)"),
			}, "script"),
		},
		{
			Name:        GitDiff,
			Description: "Show the current git diff (optionally for one path).",
			InputSchema: obj(map[string]any{
				"path": str("optional path to limit the diff"),
			}),
		},
		{
			Name:        EditFile,
			Description: "Edit a file via anchored search/replace. old_string must occur exactly once (or set replace_all). Empty old_string creates a new file with new_string.",
			InputSchema: obj(map[string]any{
				"path":        str("path relative to the repo root"),
				"old_string":  str("exact text to replace (empty to create a new file)"),
				"new_string":  str("replacement text"),
				"replace_all": boolean("replace every occurrence instead of requiring uniqueness"),
			}, "path", "new_string"),
		},
		{
			Name:        ApplyPatch,
			Description: "Apply MULTIPLE edits ATOMICALLY — all succeed or none do (the tree is rolled back on any failure). Use for a coherent change that spans files (rename a symbol + update every call site, change a signature + all callers) so it can't half-land. Each edit is like edit_file: {path, old_string, new_string, replace_all}. Empty old_string creates a file.",
			InputSchema: obj(map[string]any{
				"edits": map[string]any{
					"type":        "array",
					"description": "the edits to apply together, in order",
					"items": obj(map[string]any{
						"path":        str("path relative to the repo root"),
						"old_string":  str("exact text to replace (empty to create a new file)"),
						"new_string":  str("replacement text"),
						"replace_all": boolean("replace every occurrence instead of requiring uniqueness"),
					}, "path", "new_string"),
				},
			}, "edits"),
		},
		{
			Name:        Diagnostics,
			Description: "Get type/compile diagnostics (errors + warnings) for a file or the whole repo — see what's actually broken instead of guessing from the source or re-running a full build. Auto-detects the language (Go: gopls/go build; TypeScript: tsc --noEmit). Optional `path` scopes to a file or dir. Use after an edit to confirm it type-checks.",
			InputSchema: obj(map[string]any{
				"path": str("optional file or dir to scope the check (default: the whole repo)"),
			}),
		},
		{
			Name:        CodeNav,
			Description: "Semantic navigation via the language server (Go/TS/Python): definition, references, hover (type), and impact — the outward call graph up to `depth` levels with test callers flagged; run impact before changing a signature or deleting a symbol. Use instead of grep when you need the real definition or every caller. 1-based line/col on the symbol name; needs the language's server on PATH.",
			InputSchema: obj(map[string]any{
				"action": enum("definition | references | hover | impact", []string{"definition", "references", "hover", "impact"}),
				"path":   str("file containing the symbol"),
				"line":   map[string]any{"type": "integer", "description": "1-based line of the symbol"},
				"col":    map[string]any{"type": "integer", "description": "1-based column of the symbol"},
				"depth":  map[string]any{"type": "integer", "description": "impact only: call-graph levels to walk outward (default 2, max 3)"},
			}, "action", "path", "line", "col"),
		},
		{
			Name:        RunTests,
			Description: "Run the repo's tests and get a STRUCTURED summary — counts plus the FAILING test names and their output — instead of scrolling a raw log. Auto-detects the runner (go test / pytest / jest/vitest). Optional `path` scopes to a package/dir; optional `run` filters by test-name pattern. Prefer this over `bash go test` when you want to know what failed and why.",
			InputSchema: obj(map[string]any{
				"path": str("optional package/dir to scope the run (default: the whole repo)"),
				"run":  str("optional test-name pattern to filter"),
			}),
		},
		{
			Name:        Artifact,
			Description: "Publish a LOCAL self-contained HTML file as a shareable page at memcode.ai/code/artifact/<id> — anyone with the link can view. Build and iterate on the file with edit_file FIRST, then publish. The page must be fully self-contained: inline all CSS/JS, data: URIs for images, NO external scripts/styles/fetches (the hosting CSP blocks all external network), ~1.5MB max. Actions: publish (path+title) → stable URL; update (id+path[+title]) — replaces content in place, same URL; list — your published artifacts; delete (id). publish/update/delete are gated.",
			InputSchema: obj(map[string]any{
				"action": enum("which artifact operation", []string{"publish", "update", "list", "delete"}),
				"path":   str("repo-relative path to the HTML file to publish/update"),
				"title":  str("display title shown in the page's tab (publish; optional on update)"),
				"id":     str("artifact id returned by publish (update/delete)"),
			}, "action"),
		},
		{
			Name:        GitHub,
			Description: "GitHub PRs, issues, and CI via a typed surface (over gh). Actions: pr_view (number), pr_list, issue_view (number), issue_list, checks (CI status for a PR number or the current branch), pr_create (title+body[+base]), comment (number+body). Read actions run directly; pr_create/comment are gated.",
			InputSchema: obj(map[string]any{
				"action": enum("which GitHub operation", []string{"pr_view", "pr_list", "issue_view", "issue_list", "checks", "pr_create", "comment"}),
				"number": map[string]any{"type": "integer", "description": "PR or issue number (pr_view/issue_view/comment; checks uses the current branch if omitted)"},
				"title":  str("PR title (pr_create)"),
				"body":   str("PR/comment body (pr_create/comment)"),
				"base":   str("base branch for pr_create (default: the repo default branch)"),
			}, "action"),
		},
		{
			Name: Memcode,
			Description: "Query memcode's persistent model of THIS repo — prefer it over re-deriving by hand. Read-only, fast. Commands:\n" +
				"  overview   — what the project is + what's being worked on now (\"what is this project / where are we\")\n" +
				"  map        — subsystems, ownership, change hotspots\n" +
				"  context    — compiled ContextPack for `target`\n" +
				"  why        — `target`'s provenance: introduced, evolved, deps, tests\n" +
				"  recall     — where something was decided/documented (`query`)\n" +
				"  next       — highest-value next move\n" +
				"  recap      — what happened this session (or the last)\n" +
				"  memories   — governing claims, doctrine sources, recent decisions\n" +
				"  claims     — adjudicated claims governing the repo\n" +
				"  sources    — tracked instruction/doc files\n" +
				"  session    — target = recent (last N prior sessions) | current | previous | recap | sidequests | commits | search (`query`) | shell (last $ command+output, for explain/fix-last)\n" +
				"  acceptance — was recent agent work accepted/corrected/rejected (via git)\n" +
				"  doctor     — health check of memcode's setup\n" +
				"  jobs       — background jobs: target = list | tail <id> | kill <id>. Stop a background server HERE, never with shell `kill` (it won't touch the managed job).",
			InputSchema: obj(map[string]any{
				"command": enum("which memcode capability to invoke", MemcodeCommands),
				"target":  str("path/subsystem (context/why), or session subcommand: recent|current|previous|recap|sidequests|commits|search|shell"),
				"query":   str("the question (recall) or search term (session search)"),
				"limit":   map[string]any{"type": "integer", "description": "max results (recall/session)"},
			}, "command"),
		},
		{
			Name: Todo,
			Description: "Track your own multi-step work so you don't lose your place — use without asking whenever a request has 3+ steps or multiple deliverables. Concise scratchpad, not a plan (planning is a separate mode). Actions:\n" +
				"  create — start a checklist from `items`; the first becomes active\n" +
				"  add    — push newly-discovered items (the routine growth path; don't resend the list)\n" +
				"  start  — focus `index` (1-based), or the next pending item\n" +
				"  done   — complete the active item (or `index`) and advance; the FINAL item is rejected unless a build/tests passed after your last edit\n" +
				"  block / skip — mark the active item (or `index`) blocked / intentionally skipped\n" +
				"  update — REPLACE the whole list (repair/reorder only; use add/start/done for normal flow)\n" +
				"  show   — return the checklist.",
			InputSchema: obj(map[string]any{
				"action": enum("which todo operation to perform", TodoActions),
				"items": map[string]any{
					"type":        "array",
					"description": "the checklist items (for create/update)",
					"items": obj(map[string]any{
						"title":  str("short imperative step"),
						"detail": str("optional context: what this step entails"),
						"status": enum("pending|active|done|blocked (update only; create defaults sensibly)", []string{"pending", "active", "done", "blocked"}),
					}, "title"),
				},
				"index":   map[string]any{"type": "integer", "description": "1-based item to target (done/block); omit for the active item"},
				"indices": map[string]any{"type": "array", "description": "1-based items to mark done/skip in ONE call — sync the list after finishing several items in one sweep", "items": map[string]any{"type": "integer"}},
			}, "action"),
		},
		{
			Name:        Explore,
			Description: "Spawn a focused read-only sub-agent to investigate ONE facet of the codebase (or verify ONE claim) in depth. For broad research, emit SEVERAL explore calls in a single turn — they run in parallel and you synthesize. One facet per explore: split distinct sub-questions into separate calls instead of joining them with 'also'/'and', so each scout gets a focused search space. Heavyweight — for genuine multi-area investigation or fan-out verification, not single-file questions you can read directly.",
			InputSchema: obj(map[string]any{
				"question": str("what this sub-agent should find out"),
				"scope":    str("optional subsystem or path to focus the investigation on"),
				"focus":    str("optional angle, e.g. 'entrypoints, schema, tests'"),
			}, "question"),
		},
		{
			Name:        AskUser,
			Description: "Ask the USER a clarifying question — only for a decision-blocking fork research can't resolve (a product/architecture choice only they can make). Give 2–4 concrete options {label, description}: label = the choice as a short noun phrase; description = one terse ≤20-word line with the real trade-off. Put the option you'd pick FIRST with ' (recommended)' appended (do this whenever you have a lean). No generic 'Other'/'None of these' option — the user can always type their own. Use sparingly; never ask what you could find by reading.",
			InputSchema: obj(map[string]any{
				"question": str("the clarifying question — specific and decision-relevant, kept to one line"),
				"options": map[string]any{
					"type":        "array",
					"description": "2-4 candidate answers",
					"items": obj(map[string]any{
						"label":       str("the choice in a few words — a concise noun phrase, not a sentence"),
						"description": str("ONE terse line, ≤20 words: the real meaning or trade-off, dense and high-signal — no rambling, no restating the label"),
					}, "label"),
				},
			}, "question"),
		},
		{
			Name:        WebSearch,
			Description: "Search the web for current external information (a library's current API, an error message, recent docs/news). Returns a concise synthesized answer with sources. Use when the answer isn't in this repo and you need up-to-date information. To read a specific URL you already have, use `fetch` instead.",
			InputSchema: obj(map[string]any{
				"query": str("a web search query"),
			}, "query"),
		},
		{
			Name:        Fetch,
			Description: "Fetch a specific URL over HTTP(S) and return its content as text. Use when you have a concrete link (docs page, raw file, API/JSON endpoint) to read. For an open-ended question without a URL, use `web_search`.",
			InputSchema: obj(map[string]any{
				"url": str("the http(s) URL to fetch"),
			}, "url"),
		},
		{
			Name:        Trace,
			Description: "TRACE BEFORE THEORY (alias: wiretap). Trace an artifact through memcode's pipeline stages, reporting the size at each stage and flagging the FIRST catastrophic drop — find WHERE a value dies instead of guessing why. Use the moment a fetch/read/extraction looks wrong (empty, truncated, mangled), before proposing a cause. `target` = a URL (fetch pipeline: raw bytes → extracted text → truncated) or a file path. Raw ground truth, never summarized.",
			InputSchema: obj(map[string]any{
				"target": str("a URL (traces the fetch pipeline) or a file path"),
			}, "target"),
		},
		{
			Name:        Skill,
			Description: "Find/load installed SKILLS, or discover/install new ones from the skills.sh catalog — expert guidance for a tool/library/service. Reach for one the moment you work with a third-party CLI, package, or service you lack strong guidance for, before improvising. `find` searches installed skills; `load` pulls one in by exact name (gated). Nothing installed for the topic? `discover` searches the public catalog; `install` adds \"owner/repo@skill\" (gated — writes files). Prefer find→load. For ordinary repo questions just read the code.",
			InputSchema: obj(map[string]any{
				"find":     str("search query: a technology/library/CLI/topic (e.g. \"supabase\", \"next.js\") — returns matching INSTALLED skills"),
				"load":     str("exact installed skill name to pull its full guidance into context (gated). Get it from `find` (e.g. \"claude-api\", \"vercel:ai-sdk\")"),
				"discover": str("search query for the REMOTE skills.sh catalog (e.g. \"react\", \"typescript\") — lists installable packages when nothing is installed for the topic"),
				"install":  str("package to install from the catalog, \"owner/repo@skill\" (gated). Get it from `discover` (e.g. \"vercel-labs/agent-skills@vercel-optimize\")"),
			}),
		},
		{
			Name:        Knowledge,
			Description: "Consult memcode's built-in knowledge packs — curated baseline facts and idioms for common stacks (vercel, next, react, node, supabase, …). Ungated reference, free to read: consult whenever you're changing code for one of these stacks and aren't certain of the platform's built-ins, conventions, or gotchas. `topic` = one pack's full guidance by name; `find` = list packs matching a query; the session pointer names the packs this repo uses. Facts override your priors; Idioms are defaults for NEW code only — match existing code.",
			InputSchema: obj(map[string]any{
				"find":  str("search query: a stack/framework/service you're working with (e.g. \"vercel\", \"react\") — lists matching knowledge packs"),
				"topic": str("exact pack name to read its full Facts + Idioms (e.g. \"vercel\", \"next\", \"supabase\")"),
			}),
		},
		{
			Name:        Script,
			Description: "Save, find, run, and delete reusable multi-step command SEQUENCES — a proven recipe (\"rebuild the cli\", \"commit, push, deploy\") replayed by name instead of re-derived every time. `list`/`find` are read-only. `run` asks ONCE — \"run script X?\" — and then executes; it does NOT re-classify or re-approve the individual commands inside (those were already vetted, live, the first time the sequence ran and earned its \"this is repeatable\" status). `save` after you've run a sequence and seen it work (your judgment, not a special ceremony) — one script per distinct operation, named for what it does. `delete` soft-removes one (recoverable, not gone for good).",
			InputSchema: obj(map[string]any{
				"save":        str("slug (lowercase-hyphen) to save or update — pairs with description + command (gated)"),
				"description": str("one-line description of what the script does, for `save`"),
				"command":     str("the command body to save, for `save`"),
				"run":         str("exact saved slug to execute — gated ONCE at the script level, not per inner command"),
				"background":  boolean("with `run`: start detached instead of blocking, for a long-running script (dev server/watcher)"),
				"list":        boolean("list every saved script (read-only)"),
				"find":        str("search query: saved scripts matching a topic (read-only)"),
				"delete":      str("exact saved slug to remove (gated, soft-deleted — recoverable)"),
			}),
		},
		{
			Name:        EnterPlan,
			Description: "Enter plan mode: research-only — investigate, ask clarifying questions, then propose a plan the user approves before anything runs. Use when asked to plan, or for a large/ambiguous task, instead of typing a plan inline. Not for small, clear changes.",
			InputSchema: obj(map[string]any{
				"yolo": boolean("auto-resolve questions and auto-execute on approval (default false)"),
			}),
		},
		{
			Name:        Reasoning,
			Description: "Match thinking depth to task difficulty. SELF: pass only `effort` (off | medium | high | auto) to set your own depth for the rest of this turn (no arguments = report current). Raise it when stuck or weighing real tradeoffs; drop it for mechanical batches; never switches models. DELEGATE: pass `task` (+ `context`) to hand ONE hard, self-contained sub-problem to the strong reasoning model — it starts fresh, reads the repo but can't edit, and returns its conclusion. For when raising your own effort wasn't enough; slower and costlier, never routine.",
			InputSchema: obj(map[string]any{
				"task":    str("a hard, self-contained sub-problem for the strong reasoning model (omit to adjust your own depth instead)"),
				"context": str("background the delegate needs — it does not see this conversation"),
				"effort":  enum("thinking depth: off | medium | high | auto (self-adjust default: report; delegate default: high)", []string{"off", "medium", "high", "auto"}),
			}),
		},
		{
			Name:        CancelPlan,
			Description: "ABANDON plan mode because the user said to stop (\"never mind\", \"cancel\") — the plan is discarded, nothing presented or executed. NEVER call this to finish planning: to finish, write the plan as your prose reply and the approve/execute selector appears automatically. To execute an approved plan, use execute_plan.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        ExecutePlan,
			Description: "Approve the current plan and start EXECUTING it (the plan becomes the binding contract). Call the moment the user, having seen the plan, says go (\"execute\", \"do it\", \"proceed\", or an info-then-go message). Never call it on your own right after proposing — the user approves, not you; if they ask for ANY change, revise and wait.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        Dispatch,
			Description: "Dispatch a discrete block of work to a hands-off background sub-agent (full mutating loop: edits, bash, tests; no prompts, no questions). FIRE-AND-FORGET: returns a job id and the result goes to the USER, not back to you — you will not learn the outcome, so dispatch only terminal, independent work you don't need to act on, as a complete self-contained task. Runs detached behind the repo writer lock, but it does NOT share this session's write lock — avoid files you're actively editing. Not for read-only research (explore) or work needing approval (plan mode). Status: /agents or `memcode jobs logs <id>`.",
			InputSchema: obj(map[string]any{
				"task": str("the complete, self-contained task for the sub-agent — it cannot ask you questions mid-run"),
				"mode": enum("permission mode for the sub-agent (default auto)", []string{"auto", "allow-all"}),
			}, "task"),
		},
		{
			Name:        Agent,
			Description: "Hand a self-contained sub-task to a fresh sub-agent and get its RESULT back as the tool result (it reports to you — unlike dispatch). Use it to run work on a stronger model (tier:\"strong\" for writing/docs/design reasoning/hard debugging/non-code) or to offload a chunk so your context stays focused. The sub-agent starts fresh — put everything it needs in `task`/`context`. readonly:true = investigate only; background:true = detached, result reported back when done. For parallel read-only repo investigation prefer `explore`.",
			InputSchema: obj(map[string]any{
				"task":       str("the complete, self-contained instruction for the sub-agent (it has no memory of this conversation)"),
				"context":    str("optional background the sub-agent needs to do the task well"),
				"readonly":   boolean("true = investigate/generate only (no edits); false (default) = a full agent that can edit files and run commands"),
				"background": boolean("true = run detached and have the result reported back to you when it finishes (keep working meanwhile); false (default) = run now and wait for the result"),
			}, "task"),
		},
		{
			Name:        RecallPlan,
			Description: "Retrieve a previously presented plan (every plan is auto-saved to ~/.memcode/plans). Call when the user asks to pick up / resume / revisit a plan. No slug = the most recent plan in full plus a list of older ones; a slug = that specific plan. Read-only recall — returns the plan text to act on.",
			InputSchema: obj(map[string]any{
				"slug": str("optional: a specific saved-plan slug (e.g. \"calm-cooking-otter\"); omit to get the most recent plan"),
			}),
		},
		{
			Name:        PreferenceSignal,
			Description: "Capture a DURABLE user preference/constraint for FUTURE sessions — call when the user states a forceful directive (\"always X\", \"never Y, use Z\", \"stop doing W\"). Not for one-off tasks or this-session-only concerns. Signals accumulate; a reducer silently promotes a recurring cluster (≥3 signals, ≥2 sessions) to a standing preference in .memcode/prefs/ — you won't see the promotion. `axis` categorizes; `scope` defaults to the whole repo.",
			InputSchema: obj(map[string]any{
				"text":  str("the preference/constraint in the user's words (e.g. \"always commit, deploy, rebuild after gateway changes\")"),
				"axis":  enum("which preference axis this belongs to — drives clustering", PreferenceAxes),
				"scope": str("optional scope this preference governs (default \".\" = whole repo)"),
			}, "text", "axis"),
		},
	}
}

func enum(desc string, values []string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

// BrowserDefs returns the browser tool schemas, advertised only when --chrome is
// set. They drive a real Chrome instance (via CDP) so the agent can fully
// interact with web pages as a user would: navigate, click, type, scroll, hover,
// keyboard, dropdowns, history, tabs, screenshots, console logs, and JS.
// Screenshots come back as image blocks (vision) through the tool_result content
// union. Actions that change page state (navigate/click/type/scroll/hover/press/
// select/back/forward/new_tab/close_tab) are Medium (they act on the world);
// screenshot/eval/text/console/list_tabs are Safe (read-only).
func BrowserDefs() []wire.ToolDef {
	return []wire.ToolDef{
		{
			Name:        BrowserNavigate,
			Description: "Open a URL in the current Chrome tab. Use this to load a web page you want to interact with or inspect. Waits for the page body to be ready before returning. Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"url": str("the http(s) URL to navigate to"),
			}, "url"),
		},
		{
			Name:        BrowserClick,
			Description: "Click an element on the current page by CSS selector. Waits for the element to be visible first. Use after browser_navigate to interact with buttons, links, etc. Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"selector": str("a CSS selector for the element to click (e.g. #btn, .submit, a[href='/login'])"),
			}, "selector"),
		},
		{
			Name:        BrowserType,
			Description: "Type into an input by CSS selector: clicks it, clears the current value, sends the keys (append:true keeps the value). Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"selector": str("a CSS selector for the input element"),
				"text":     str("the text to type into the element"),
				"append":   boolean("keep the field's existing value and append instead of clearing first (default false)"),
			}, "selector", "text"),
		},
		{
			Name:        BrowserWait,
			Description: "Wait until an element becomes visible (default), hidden, or ready. Use after clicks that trigger SPA navigation or async loading. Read-only.",
			InputSchema: obj(map[string]any{
				"selector":        str("a CSS selector to wait for"),
				"state":           str("visible (default), hidden, or ready"),
				"timeout_seconds": integer("seconds before giving up (default 30, max 60)"),
			}, "selector"),
		},
		{
			Name:        BrowserUpload,
			Description: "Attach a project file to a file input (<input type=file>). Files resolving outside the project root (through symlinks too) are refused. Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"selector": str("a CSS selector for the file input"),
				"path":     str("the file to upload, inside the project"),
			}, "selector", "path"),
		},
		{
			Name:        BrowserResize,
			Description: "Set the viewport size in CSS pixels (64-4096) — responsive testing, e.g. 390x844 for a phone. Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"width":  integer("viewport width in CSS pixels"),
				"height": integer("viewport height in CSS pixels"),
			}, "width", "height"),
		},
		{
			Name:        BrowserScreenshot,
			Description: "Take a screenshot of the current page and return it as an IMAGE — you SEE the page (vision). This is the primary way to understand a page's visual state after navigating or interacting. Default captures the viewport; set full_page:true for the entire scrollable page (larger image, more tokens). Read-only.",
			InputSchema: obj(map[string]any{
				"full_page": boolean("capture the entire scrollable page, not just the viewport (default false — viewport only, to limit token cost)"),
			}),
		},
		{
			Name:        BrowserEval,
			Description: "Run a JavaScript expression in the current page and return the result as a string. Use for ANYTHING the dedicated tools don't cover: extracting data (document.title, querySelectorAll results, computed styles), triggering mutations (dispatchEvent, setting values), reading state, etc. The expression is wrapped in a function call and its return value is stringified. This is the universal escape hatch.",
			InputSchema: obj(map[string]any{
				"script": str("a JavaScript expression to evaluate (e.g. \"document.title\", \"document.querySelectorAll('a').length\", \"document.querySelector('#btn').click()\")"),
			}, "script"),
		},
		{
			Name:        BrowserText,
			Description: "Get the visible text content of the current page (body.innerText). Read-only — use when you need the page's text without a screenshot (cheaper than vision, but no layout). Prefer browser_screenshot when visual state matters.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        BrowserScroll,
			Description: "Scroll the page by pixel deltas. Positive dy scrolls down, positive dx scrolls right. Use to see content below the fold before a screenshot. Common: dy=500 to scroll down one chunk, dy=-500 to go back up. Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"dx": map[string]any{"type": "integer", "description": "horizontal pixels to scroll (positive = right; default 0)"},
				"dy": map[string]any{"type": "integer", "description": "vertical pixels to scroll (positive = down; default 0)"},
			}),
		},
		{
			Name:        BrowserPressKey,
			Description: "Send a single keyboard event to the page (to whatever element has focus). Use after browser_click or browser_type to submit forms (Enter), close modals (Escape), move between fields (Tab), navigate lists (ArrowDown/Up), etc. Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"key": str("the key to press: Enter, Escape, Tab, Backspace, Space, ArrowUp, ArrowDown, ArrowLeft, ArrowRight — or a single printable character (e.g. \"a\")"),
			}, "key"),
		},
		{
			Name:        BrowserHover,
			Description: "Hover the mouse over an element by CSS selector, triggering hover states, dropdown menus, tooltips, etc. Scrolls the element into view first. Use before browser_screenshot to capture a hover-triggered UI state. Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"selector": str("a CSS selector for the element to hover over"),
			}, "selector"),
		},
		{
			Name:        BrowserSelect,
			Description: "Pick an option in a <select> dropdown by its value attribute. Dispatches a change event so the page reacts. Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"selector": str("a CSS selector for the <select> element"),
				"value":    str("the value attribute of the <option> to select"),
			}, "selector", "value"),
		},
		{
			Name:        BrowserBack,
			Description: "Navigate the current tab to the previous page in browser history. Subject to permission approval.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        BrowserForward,
			Description: "Navigate the current tab to the next page in browser history. Subject to permission approval.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        BrowserConsole,
			Description: "Read the browser console log — messages from console.log/error/warn/info and uncaught exceptions, captured since the page loaded. Optionally filter by level. Read-only. Use to debug page errors or see what a page logged.",
			InputSchema: obj(map[string]any{
				"level": str("optional: filter to a level — log, error, warn, info, debug, exception (omit for all)"),
			}),
		},
		{
			Name:        BrowserNewTab,
			Description: "Open a new browser tab (about:blank) and switch focus to it. Use when you need to work on multiple pages without losing the current one. Follow with browser_navigate to load a URL in the new tab. Subject to permission approval.",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        BrowserSwitchTab,
			Description: "Switch focus to a different open tab by its 1-based index. Use browser_list_tabs to see the indices. Read-only (just changes which tab subsequent actions target).",
			InputSchema: obj(map[string]any{
				"index": map[string]any{"type": "integer", "description": "the 1-based tab index to switch to (from browser_list_tabs)"},
			}, "index"),
		},
		{
			Name:        BrowserCloseTab,
			Description: "Close a browser tab by its 1-based index. Can't close the last tab. If you close the active tab, focus moves to the previous one. Subject to permission approval.",
			InputSchema: obj(map[string]any{
				"index": map[string]any{"type": "integer", "description": "the 1-based tab index to close"},
			}, "index"),
		},
		{
			Name:        BrowserListTabs,
			Description: "List all open browser tabs with their index, URL, and title. The active tab is marked with ▶. Read-only — use to get tab indices before browser_switch_tab or browser_close_tab.",
			InputSchema: obj(map[string]any{}),
		},
	}
}

// UnmarshalJSON accepts an ask option as EITHER a plain string (a bare label) or an
// object {label, description}. Models are inconsistent about which they emit, so we
// tolerate both rather than dropping options that arrived as strings.
func (o *AskOption) UnmarshalJSON(b []byte) error {
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '"' {
		return json.Unmarshal(b, &o.Label)
	}
	type raw AskOption // shed the custom method to avoid infinite recursion
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*o = AskOption(r)
	return nil
}
