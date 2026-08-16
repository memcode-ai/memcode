package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/authflow"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/gateway/importer"
	"github.com/memcode-ai/memcode/internal/mcp"
	"github.com/memcode-ai/memcode/internal/provider"
)

// The migration commands are deliberately source-specific — `memcode claw` for
// OpenClaw, `memcode hermes` for Hermes — each a namespace holding a `migrate`
// subcommand (mirroring Hermes's own `hermes claw migrate`), rather than one
// command that guesses. A user who moved from OpenClaw to Hermes still has
// ~/.openclaw lying around; auto-detecting between two installs would silently
// import the stale one. Naming the source is the whole point: no cleverness, no
// collisions. The namespace also leaves room for future per-source subcommands.
//
// migrate moves the full install, not just channels: gateway channels (tokens +
// allow-lists), provider API keys, skills, and long-term memory (read from the
// source's markdown stores into memcode's global ~/.memcode/memory.md, loaded
// every session).

var clawCmd = &cobra.Command{
	Use:   "claw",
	Short: "OpenClaw migration tools (see `memcode claw migrate`)",
}

var clawMigrateCmd = &cobra.Command{
	Use:   "migrate [path]",
	Short: "Migrate an OpenClaw install into memcode (channels, keys, skills, memory)",
	Long: `Migrate an existing OpenClaw install into memcode.

Run it with no arguments and it reads OpenClaw's own default location:

  memcode claw migrate

It brings over your gateway channels, provider API keys, skills, and long-term
memory. Point it elsewhere only if your OpenClaw state lives in a non-standard
directory:

  memcode claw migrate /path/to/.openclaw`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		provider.LoadDotEnv() // so env-referenced channel credentials resolve
		dir, searched := openClawDir(arg0(args))
		if dir == "" {
			return fmt.Errorf("no OpenClaw install found (looked in %s)", strings.Join(searched, ", "))
		}
		return runMigration(cmd, migrationSource{
			display:    "OpenClaw",
			slug:       "openclaw",
			dir:        dir,
			channels:   openClawChannels,
			memory:     openClawMemory,
			schedules:  importer.ImportOpenClawSchedules,
			mcpServers: openClawMCP,
			identity:   openClawIdentity,
		})
	},
}

var hermesCmd = &cobra.Command{
	Use:   "hermes",
	Short: "Hermes migration tools (see `memcode hermes migrate`)",
}

var hermesMigrateCmd = &cobra.Command{
	Use:   "migrate [path]",
	Short: "Migrate a Hermes install into memcode (channels, keys, skills, memory)",
	Long: `Migrate an existing Hermes install into memcode.

Run it with no arguments and it reads Hermes's own default location:

  memcode hermes migrate

It brings over your gateway channels, provider API keys, skills, and long-term
memory. Point it elsewhere only if your Hermes state lives in a non-standard
directory:

  memcode hermes migrate /path/to/.hermes`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		provider.LoadDotEnv()
		dir := hermesDir(arg0(args))
		if dir == "" {
			home, _ := os.UserHomeDir()
			return fmt.Errorf("no Hermes install found (looked in %s)", filepath.Join(home, ".hermes"))
		}
		return runMigration(cmd, migrationSource{
			display:    "Hermes",
			slug:       "hermes",
			dir:        dir,
			channels:   hermesChannels,
			memory:     hermesMemory,
			schedules:  importer.ImportHermesSchedules,
			mcpServers: hermesMCP,
			identity:   hermesIdentity,
		})
	},
}

// migrationSource describes one source assistant. channels reads that tool's
// config (with the .env beside it resolving credential references) into the
// channel/secret mapping; the rest is common across sources.
type migrationSource struct {
	display  string
	slug     string
	dir      string
	channels func(dir string, env map[string]string) (importer.Result, error)
	memory   func(dir string) []string // extracts the source's memory as discrete entries
	// schedules reads the source's cron jobs into memcode schedules plus notes
	// for anything that couldn't be carried.
	schedules func(dir string) ([]gwconfig.Schedule, []string)
	// mcpServers reads the source's MCP server config into memcode's shape.
	mcpServers func(dir string) (map[string]mcp.ServerConfig, []string)
	// identity reads the source agent's identity/agent files (SOUL.md and
	// friends) verbatim — they seed a memcode agent, not global memory.
	identity func(dir string) string
}

// runMigration performs the full migration for a source: channels, provider API
// keys, skills, and memory extraction — reporting exactly what moved and what
// could not, never dropping anything silently.
func runMigration(cmd *cobra.Command, src migrationSource) error {
	// The .env beside the install is the canonical home for both channel bot
	// tokens (resolved by the channel importer) and provider API keys.
	env := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(src.dir, ".env")); err == nil {
		env = importer.ParseEnv(b)
	}

	res, err := src.channels(src.dir, env)
	if err != nil {
		return err
	}
	if res.Secrets == nil {
		res.Secrets = map[string]string{}
	}

	// Provider API keys ride the same names into memcode's global .env.
	keys := importer.ProviderKeys(env)
	for k, v := range keys {
		res.Secrets[k] = v
	}

	// 1. Channels → gateway.yaml (merge, preserving any per-channel settings).
	cur, err := gwconfig.Load()
	if err != nil {
		return err
	}
	if cur.Channels == nil {
		cur.Channels = map[string]gwconfig.Channel{}
	}
	var channels []string
	for name, ch := range res.Settings.Channels {
		existing := cur.Channels[name]
		existing.AllowFrom = ch.AllowFrom
		cur.Channels[name] = existing
		channels = append(channels, name)
	}
	sort.Strings(channels)

	// Cron jobs → schedules (skip name collisions rather than clobbering).
	var schedCount int
	if src.schedules != nil {
		scheds, schedNotes := src.schedules(src.dir)
		res.Notes = append(res.Notes, schedNotes...)
		existing := map[string]bool{}
		for _, sc := range cur.Schedules {
			existing[sc.Name] = true
		}
		for _, sc := range scheds {
			if existing[sc.Name] {
				res.Notes = append(res.Notes, fmt.Sprintf("cron: schedule %q already exists in gateway.yaml — kept yours, skipped the import", sc.Name))
				continue
			}
			cur.Schedules = append(cur.Schedules, sc)
			schedCount++
		}
	}
	if err := gwconfig.Save(cur); err != nil {
		return err
	}

	// 2. Secrets (channel tokens + provider keys) → global .env.
	if len(res.Secrets) > 0 {
		if err := authflow.SetGlobalEnv(res.Secrets); err != nil {
			return err
		}
	}

	// 3. Skills → ~/.memcode/skills (agentskills.io standard, shared with memcode).
	skills, skillNotes := copySkills(filepath.Join(src.dir, "skills"))
	res.Notes = append(res.Notes, skillNotes...)

	// 4. MCP servers → user scope (~/.memcode/mcp.json), shared by all projects.
	var mcpCount int
	if src.mcpServers != nil {
		servers, mcpNotes := src.mcpServers(src.dir)
		res.Notes = append(res.Notes, mcpNotes...)
		existing := mcp.UserServers()
		for name, sc := range servers {
			if _, ok := existing[name]; ok {
				res.Notes = append(res.Notes, fmt.Sprintf("mcp: server %q already configured — kept yours, skipped the import", name))
				continue
			}
			if err := mcp.AddServer("", mcp.ScopeUser, name, sc); err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("mcp: server %q could not be written: %v", name, err))
				continue
			}
			mcpCount++
		}
	}

	// 5. Identity → a agent. SOUL.md is the source AGENT's identity, so it
	// becomes a memcode agent (its MEMCODE.md), not global memory: bind a
	// channel to it and you're talking to the same assistant you migrated.
	personaID := ""
	if src.identity != nil {
		if content := strings.TrimSpace(src.identity(src.dir)); content != "" {
			id := src.slug
			home, herr := gwconfig.AgentHome(id)
			mcPath := filepath.Join(home, "SOUL.md")
			switch {
			case herr != nil:
				res.Notes = append(res.Notes, fmt.Sprintf("identity: could not resolve the agent home: %v", herr))
			case fileExists(mcPath):
				res.Notes = append(res.Notes, fmt.Sprintf("identity: agent %q already has a SOUL.md — left yours in place; the source's was NOT copied", id))
			default:
				if err := os.MkdirAll(home, 0o755); err != nil {
					res.Notes = append(res.Notes, fmt.Sprintf("identity: %v", err))
					break
				}
				if err := os.WriteFile(mcPath, []byte(content+"\n"), 0o644); err != nil {
					res.Notes = append(res.Notes, fmt.Sprintf("identity: %v", err))
					break
				}
				if cur.Agents == nil {
					cur.Agents = map[string]gwconfig.Agent{}
				}
				if _, ok := cur.Agents[id]; !ok {
					cur.Agents[id] = gwconfig.Agent{Type: "assistant"}
					if err := gwconfig.Save(cur); err != nil {
						res.Notes = append(res.Notes, fmt.Sprintf("identity: agent %q written but not registered: %v", id, err))
					}
				}
				personaID = id
			}
		}
	}

	// 6. Memory → extracted from the source's markdown stores into global memory.md.
	memCount, err := migrateMemories(src)
	if err != nil {
		return err
	}

	cmd.Printf("Migrated from %s (%s)\n", src.display, src.dir)
	if len(channels) > 0 {
		cmd.Printf("  channels:  %s\n", strings.Join(channels, ", "))
	}
	if schedCount > 0 {
		cmd.Printf("  schedules: %d cron job(s) → gateway.yaml (see `memcode gateway schedule list`)\n", schedCount)
	}
	cmd.Printf("  API keys:  %d provider key(s) → global .env\n", len(keys))
	cmd.Printf("  secrets:   %d credential(s) written\n", len(res.Secrets))
	cmd.Printf("  skills:    %d imported → ~/.memcode/skills\n", len(skills))
	if mcpCount > 0 {
		cmd.Printf("  mcp:       %d server(s) → ~/.memcode/mcp.json (user scope, all projects)\n", mcpCount)
	}
	if personaID != "" {
		cmd.Printf("  identity:  SOUL.md → agent %q (~/.memcode/agents/%s/SOUL.md) — bind a channel with `channels.<name>.agent: %s`\n", personaID, personaID, personaID)
	}
	if memCount > 0 {
		cmd.Printf("  memory:    %d entries → ~/.memcode/memory.md (global, loaded every session)\n", memCount)
	}
	for _, note := range res.Notes {
		cmd.Printf("  note: %s\n", note)
	}
	cmd.Println("Review with `memcode gateway setup`, then run `memcode gateway`.")
	return nil
}

// arg0 returns the optional path argument, or "" for the zero-arg default.
func arg0(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return ""
}

// openClawChannels reads <dir>/openclaw.json into the channel/secret mapping,
// resolving env-referenced credentials from the .env beside it.
func openClawChannels(dir string, env map[string]string) (importer.Result, error) {
	data, err := os.ReadFile(filepath.Join(dir, "openclaw.json"))
	if err != nil {
		return importer.Result{}, err
	}
	return importer.FromOpenClaw(data, func(k string) string { return env[k] })
}

// hermesChannels reads <dir>/config.yaml into the channel/secret mapping.
func hermesChannels(dir string, env map[string]string) (importer.Result, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return importer.Result{}, err
	}
	return importer.FromHermes(data, env)
}

// openClawMCP / hermesMCP read each source's MCP server block.
func openClawMCP(dir string) (map[string]mcp.ServerConfig, []string) {
	data, err := os.ReadFile(filepath.Join(dir, "openclaw.json"))
	if err != nil {
		return nil, nil
	}
	return importer.OpenClawMCPServers(data)
}

func hermesMCP(dir string) (map[string]mcp.ServerConfig, []string) {
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return nil, nil
	}
	return importer.HermesMCPServers(data)
}

// hermesIdentity reads the profile's SOUL.md (one agent per Hermes profile).
func hermesIdentity(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		return ""
	}
	return string(data)
}

// openClawIdentity reads SOUL.md + IDENTITY.md from the first workspace that
// has them — the workspace is the OpenClaw agent's home, so these are that
// agent's identity, not user memory.
func openClawIdentity(dir string) string {
	var parts []string
	for _, rel := range []string{"SOUL.md", "IDENTITY.md"} {
		for _, r := range openClawWorkspaceRoots(dir) {
			if data, err := os.ReadFile(filepath.Join(r, rel)); err == nil {
				parts = append(parts, strings.TrimSpace(string(data)))
				break
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// openClawDir resolves the OpenClaw state directory: an explicit arg, then
// OpenClaw's own default locations (honoring OPENCLAW_STATE_DIR and the legacy
// ~/.clawdbot). Returns the found dir (or "") and the locations searched.
func openClawDir(arg string) (string, []string) {
	if arg != "" {
		return arg, []string{arg}
	}
	var candidates []string
	if d := os.Getenv("OPENCLAW_STATE_DIR"); d != "" {
		candidates = append(candidates, d)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".openclaw"),
			filepath.Join(home, ".clawdbot"), // legacy
		)
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c, candidates
		}
	}
	return "", candidates
}

// hermesDir resolves the Hermes state directory: an explicit arg, then ~/.hermes.
func hermesDir(arg string) string {
	if arg != "" {
		return arg
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".hermes")
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p
	}
	return ""
}

// copySkills copies each skill directory (one holding a SKILL.md) under srcDir
// into ~/.memcode/skills, skipping any that already exist. Imported skills are
// third-party code, so a note flags them for review rather than trusting them
// blindly. Returns the names imported and any notes.
func copySkills(srcDir string) (imported []string, notes []string) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, nil // no skills dir → nothing to do
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	dstRoot := filepath.Join(home, ".memcode", "skills")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src := filepath.Join(srcDir, e.Name())
		if !hasSkillManifest(src) {
			continue
		}
		dst := filepath.Join(dstRoot, e.Name())
		if _, err := os.Stat(dst); err == nil {
			notes = append(notes, fmt.Sprintf("skill %q already exists in ~/.memcode/skills — kept yours, skipped the import", e.Name()))
			continue
		}
		if err := copyTree(src, dst); err != nil {
			notes = append(notes, fmt.Sprintf("skill %q could not be copied: %v", e.Name(), err))
			continue
		}
		imported = append(imported, e.Name())
	}
	sort.Strings(imported)
	if len(imported) > 0 {
		notes = append(notes, "imported skills are third-party code — review them under ~/.memcode/skills before trusting them")
	}
	return imported, notes
}

// hasSkillManifest reports whether dir holds a SKILL.md (the Agent Skills marker).
func hasSkillManifest(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), "SKILL.md") {
			return true
		}
	}
	return false
}

// memoryImportBudget caps the total characters of imported memory written into
// global memory.md, so a large source store cannot bloat every session's
// context. Overflow entries are dropped with a note. Matches Hermes's own merge
// budget (agent_import.py's MEMORY_CHAR_LIMIT).
const memoryImportBudget = 20_000

// migrateMemories extracts the source's memory as discrete entries, dedups and
// caps them, and writes them into global memory.md (~/.memcode/memory.md) where
// they are loaded into every session. This mirrors how Hermes imports OpenClaw
// memory: markdown stores are parsed into entries, not copied verbatim. Returns
// the number of entries written.
func migrateMemories(src migrationSource) (int, error) {
	if src.memory == nil {
		return 0, nil
	}
	entries := dedupEntries(src.memory(src.dir))
	if len(entries) == 0 {
		return 0, nil
	}
	entries, truncated := capEntries(entries, memoryImportBudget)
	if len(entries) == 0 {
		return 0, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, nil
	}
	path := filepath.Join(home, ".memcode", "memory.md")
	if err := upsertMemoryBlock(path, src.slug, buildMemoryBlock(src, entries, truncated)); err != nil {
		return 0, err
	}
	return len(entries), nil
}

// openClawMemory reads OpenClaw's workspace memory files — MEMORY.md, USER.md,
// SOUL.md, AGENTS.md, and the daily memory/*.md files — and parses each into
// entries. It searches the workspace directory the way OpenClaw itself lays it
// out: the configured agents.defaults.workspace, then the default workspace/ and
// its renamed variants (workspace-main, workspace-assistant).
func openClawMemory(dir string) []string {
	roots := openClawWorkspaceRoots(dir)
	var entries []string
	readInto := func(rel string) {
		for _, r := range roots {
			if data, err := os.ReadFile(filepath.Join(r, rel)); err == nil {
				entries = append(entries, extractMarkdownEntries(string(data))...)
				return // first workspace root that has the file wins
			}
		}
	}
	readInto("MEMORY.md")
	readInto("USER.md")
	readInto("AGENTS.md")
	// Daily memory files live under <workspace>/memory/.
	for _, r := range roots {
		md := filepath.Join(r, "memory")
		files, err := os.ReadDir(md)
		if err != nil {
			continue
		}
		var names []string
		for _, e := range files {
			if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".md") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			if data, err := os.ReadFile(filepath.Join(md, n)); err == nil {
				entries = append(entries, extractMarkdownEntries(string(data))...)
			}
		}
		break // first workspace root with a memory/ dir wins
	}
	return entries
}

// hermesMemory reads Hermes's own store, ~/.hermes/memories/*.md, whose entries
// are already discrete (context-prefixed when Hermes imported them) and
// separated by bare § lines. Split on that delimiter rather than re-parsing the
// markdown, matching Hermes's own destination parser.
func hermesMemory(dir string) []string {
	var entries []string
	memDir := filepath.Join(dir, "memories")
	files, err := os.ReadDir(memDir)
	if err != nil {
		return entries
	}
	var names []string
	for _, e := range files {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(memDir, n))
		if err != nil {
			continue
		}
		for _, part := range strings.Split(string(data), "\n§\n") {
			if part = strings.TrimSpace(part); part != "" {
				entries = append(entries, part)
			}
		}
	}
	return entries
}

// openClawWorkspaceRoots returns the existing directories to search for OpenClaw
// workspace files, in priority order: the workspace configured in openclaw.json,
// then the default workspace/ and OpenClaw's renamed variants.
func openClawWorkspaceRoots(dir string) []string {
	var candidates []string
	if ws := openClawConfiguredWorkspace(dir); ws != "" {
		candidates = append(candidates, ws)
	}
	for _, name := range []string{"workspace", "workspace-main", "workspace-assistant"} {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	// A workspace file may also sit at the install root itself.
	candidates = append(candidates, dir)
	var out []string
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			out = append(out, c)
		}
	}
	return out
}

// openClawConfiguredWorkspace reads agents.defaults.workspace from openclaw.json,
// expanding a leading ~. Returns "" when unset or unreadable.
func openClawConfiguredWorkspace(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "openclaw.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Agents struct {
			Defaults struct {
				Workspace string `json:"workspace"`
			} `json:"defaults"`
		} `json:"agents"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	ws := strings.TrimSpace(cfg.Agents.Defaults.Workspace)
	if ws == "" {
		return ""
	}
	if strings.HasPrefix(ws, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			ws = filepath.Join(home, strings.TrimPrefix(ws, "~"))
		}
	}
	return ws
}

// filenameHeadingRe matches a heading that is just a memory filename (MEMORY.md,
// USER.md, …). Such headings are structural, not context, so they are excluded
// from an entry's heading-context prefix.
var filenameHeadingRe = regexp.MustCompile(`(?i)\b(MEMORY|USER|SOUL|AGENTS|TOOLS|IDENTITY)\.md\b`)

// headingRe and bulletRe match markdown headings and list items.
var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*\S)\s*$`)
	bulletRe  = regexp.MustCompile(`^\s*(?:[-*]|\d+\.)\s+(.*\S)\s*$`)
)

// extractMarkdownEntries parses one markdown memory file into discrete entries,
// a faithful port of Hermes's OpenClaw importer (openclaw_to_hermes.py's
// extract_markdown_entries). Each bullet and each paragraph becomes an entry,
// prefixed with its heading context ("Heading > Subheading: text"). Fenced code
// blocks and table rows are dropped, and entries are deduped by
// whitespace-normalized text.
func extractMarkdownEntries(text string) []string {
	var entries []string
	var headings []string
	var paragraph []string

	contextPrefix := func() string {
		var filtered []string
		for _, h := range headings {
			if h != "" && !filenameHeadingRe.MatchString(h) {
				filtered = append(filtered, h)
			}
		}
		return strings.Join(filtered, " > ")
	}
	emit := func(content string) {
		if prefix := contextPrefix(); prefix != "" {
			entries = append(entries, prefix+": "+content)
		} else {
			entries = append(entries, content)
		}
	}
	flush := func() {
		if len(paragraph) == 0 {
			return
		}
		var parts []string
		for _, l := range paragraph {
			parts = append(parts, strings.TrimSpace(l))
		}
		paragraph = nil
		if block := strings.TrimSpace(strings.Join(parts, " ")); block != "" {
			emit(block)
		}
	}

	inCode := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		stripped := strings.TrimSpace(line)

		if strings.HasPrefix(stripped, "```") {
			inCode = !inCode
			flush()
			continue
		}
		if inCode {
			continue
		}
		if m := headingRe.FindStringSubmatch(stripped); m != nil {
			flush()
			level := len(m[1])
			for len(headings) >= level {
				headings = headings[:len(headings)-1]
			}
			headings = append(headings, strings.TrimSpace(m[2]))
			continue
		}
		if m := bulletRe.FindStringSubmatch(line); m != nil {
			flush()
			emit(strings.TrimSpace(m[1]))
			continue
		}
		if stripped == "" {
			flush()
			continue
		}
		if strings.HasPrefix(stripped, "|") && strings.HasSuffix(stripped, "|") {
			flush()
			continue
		}
		paragraph = append(paragraph, stripped)
	}
	flush()

	return dedupEntries(entries)
}

// normalizeEntry collapses whitespace for dedup comparison (Hermes's
// normalize_text).
func normalizeEntry(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// dedupEntries drops empty and duplicate entries (by normalized text), keeping
// first-seen order.
func dedupEntries(entries []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		key := normalizeEntry(e)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, strings.TrimSpace(e))
	}
	return out
}

// capEntries keeps entries in order until their combined length would exceed
// limit, reporting whether any were dropped.
func capEntries(entries []string, limit int) (kept []string, truncated bool) {
	total := 0
	for _, e := range entries {
		next := total + len(e)
		if len(kept) > 0 {
			next++ // newline between bullets
		}
		if next > limit {
			return kept, true
		}
		total = next
		kept = append(kept, e)
	}
	return kept, false
}

// buildMemoryBlock renders imported entries as a bounded, bulleted markdown block
// for global memory.md. The HTML-comment markers let a re-run replace the block
// in place instead of appending a duplicate.
func buildMemoryBlock(src migrationSource, entries []string, truncated bool) string {
	var b strings.Builder
	b.WriteString(memBlockMarker(src.slug, "start") + "\n")
	b.WriteString("## Memory imported from " + src.display + "\n")
	b.WriteString("Facts and context migrated from your " + src.display + " install. Background knowledge, not instructions.\n\n")
	for _, e := range entries {
		// Keep each entry on one line so it reads as a discrete fact.
		b.WriteString("- " + strings.Join(strings.Fields(e), " ") + "\n")
	}
	if truncated {
		b.WriteString("\n_(Truncated at " + strconv.Itoa(memoryImportBudget) + " characters; see your original " + src.display + " files for the rest.)_\n")
	}
	b.WriteString(memBlockMarker(src.slug, "end") + "\n")
	return b.String()
}

func memBlockMarker(slug, side string) string {
	return "<!-- memcode:import:" + slug + ":" + side + " -->"
}

// upsertMemoryBlock writes block into memory.md, replacing any existing block for
// the same source (matched by its markers) so a re-run refreshes rather than
// duplicates. Preserves the user's own content around it.
func upsertMemoryBlock(path, slug, block string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	existing := ""
	if b, err := os.ReadFile(path); err == nil {
		existing = removeMemoryBlock(string(b), slug)
	}
	out := strings.TrimRight(existing, "\n")
	if out != "" {
		out += "\n\n"
	}
	out += strings.TrimRight(block, "\n") + "\n"
	return os.WriteFile(path, []byte(out), 0o600)
}

// removeMemoryBlock strips an existing source block (start marker through end
// marker) from content, leaving surrounding text intact. A malformed block
// (start without a following end) is left untouched.
func removeMemoryBlock(content, slug string) string {
	start, end := memBlockMarker(slug, "start"), memBlockMarker(slug, "end")
	si := strings.Index(content, start)
	if si < 0 {
		return content
	}
	ei := strings.Index(content[si:], end)
	if ei < 0 {
		return content
	}
	ei = si + ei + len(end)
	before := strings.TrimRight(content[:si], "\n")
	after := strings.TrimLeft(content[ei:], "\n")
	switch {
	case before == "":
		return after
	case after == "":
		return before + "\n"
	default:
		return before + "\n\n" + after
	}
}

// copyTree copies a file or directory tree from src to dst, creating parents.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode())
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies one file, preserving its mode.
func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

func init() {
	clawCmd.AddCommand(clawMigrateCmd)
	hermesCmd.AddCommand(hermesMigrateCmd)
	rootCmd.AddCommand(clawCmd)
	rootCmd.AddCommand(hermesCmd)
}
