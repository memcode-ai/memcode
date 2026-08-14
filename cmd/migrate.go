package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/authflow"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/gateway/importer"
	"github.com/memcode-ai/memcode/internal/provider"
)

// The migration commands are deliberately source-specific — `memcode claw` for
// OpenClaw, `memcode hermes` for Hermes — rather than one command that guesses.
// A user who moved from OpenClaw to Hermes still has ~/.openclaw lying around;
// auto-detecting between two installs would silently import the stale one. Naming
// the source is the whole point: no cleverness, no collisions.
//
// Each migrates the full install, not just channels: gateway channels (tokens +
// allow-lists), provider API keys, skills, and the conversation/memory store.
// memcode's memory is per-repository, so an assistant's global memory has no
// native home; it is preserved under ~/.memcode/imported/<source>/ and pointed at
// from global memory.md rather than dropped.

var clawCmd = &cobra.Command{
	Use:   "claw",
	Short: "Migrate an OpenClaw install into memcode (channels, keys, skills, memory)",
	Long: `Migrate an existing OpenClaw install into memcode.

Run it with no arguments — it reads OpenClaw's own default location:

  memcode claw

It brings over your gateway channels, provider API keys, and skills, and
preserves your conversation history. Point it elsewhere only if your OpenClaw
state lives in a non-standard directory:

  memcode claw /path/to/.openclaw`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		provider.LoadDotEnv() // so env-referenced channel credentials resolve
		dir, searched := openClawDir(arg0(args))
		if dir == "" {
			return fmt.Errorf("no OpenClaw install found (looked in %s)", strings.Join(searched, ", "))
		}
		return runMigration(cmd, migrationSource{
			display:  "OpenClaw",
			slug:     "openclaw",
			dir:      dir,
			channels: openClawChannels,
			// OpenClaw keeps its conversation/memory store under state/.
			memoryArtifacts: []string{"state", "openclaw.sqlite", "memory"},
		})
	},
}

var hermesCmd = &cobra.Command{
	Use:   "hermes",
	Short: "Migrate a Hermes install into memcode (channels, keys, skills, memory)",
	Long: `Migrate an existing Hermes install into memcode.

Run it with no arguments — it reads Hermes's own default location:

  memcode hermes

It brings over your gateway channels, provider API keys, and skills, and
preserves your conversation history. Point it elsewhere only if your Hermes
state lives in a non-standard directory:

  memcode hermes /path/to/.hermes`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		provider.LoadDotEnv()
		dir := hermesDir(arg0(args))
		if dir == "" {
			home, _ := os.UserHomeDir()
			return fmt.Errorf("no Hermes install found (looked in %s)", filepath.Join(home, ".hermes"))
		}
		return runMigration(cmd, migrationSource{
			display:  "Hermes",
			slug:     "hermes",
			dir:      dir,
			channels: hermesChannels,
			// Hermes keeps sessions/history in state.db and sessions/.
			memoryArtifacts: []string{"state.db", "sessions", "memory"},
		})
	},
}

// migrationSource describes one source assistant. channels reads that tool's
// config (with the .env beside it resolving credential references) into the
// channel/secret mapping; the rest is common across sources.
type migrationSource struct {
	display         string
	slug            string
	dir             string
	channels        func(dir string, env map[string]string) (importer.Result, error)
	memoryArtifacts []string // paths under dir holding the memory/history store
}

// runMigration performs the full migration for a source: channels, provider API
// keys, skills, and memory preservation — reporting exactly what moved and what
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

	// 4. Memory/history → preserved, pointed at from global memory.md.
	memNote, err := preserveMemories(src)
	if err != nil {
		return err
	}

	cmd.Printf("Migrated from %s (%s)\n", src.display, src.dir)
	if len(channels) > 0 {
		cmd.Printf("  channels:  %s\n", strings.Join(channels, ", "))
	}
	cmd.Printf("  API keys:  %d provider key(s) → global .env\n", len(keys))
	cmd.Printf("  secrets:   %d credential(s) written\n", len(res.Secrets))
	cmd.Printf("  skills:    %d imported → ~/.memcode/skills\n", len(skills))
	if memNote != "" {
		cmd.Printf("  memory:    %s\n", memNote)
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

// preserveMemories copies the source's memory/history artifacts under
// ~/.memcode/imported/<slug>/ and records a pointer in global memory.md. memcode
// has no global conversation store to load them into (its memory is
// per-repository), so they are preserved for reference, not auto-loaded. Returns
// a short human-readable note, or "" when the source has no memory store.
func preserveMemories(src migrationSource) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil
	}
	var found []string
	for _, rel := range src.memoryArtifacts {
		if _, err := os.Stat(filepath.Join(src.dir, rel)); err == nil {
			found = append(found, rel)
		}
	}
	if len(found) == 0 {
		return "", nil
	}
	dstRoot := filepath.Join(home, ".memcode", "imported", src.slug)
	for _, rel := range found {
		if err := copyTree(filepath.Join(src.dir, rel), filepath.Join(dstRoot, rel)); err != nil {
			return "", fmt.Errorf("preserving %s memory: %w", src.display, err)
		}
	}
	if err := appendMemoryPointer(home, src, dstRoot); err != nil {
		return "", err
	}
	return fmt.Sprintf("history preserved at ~/.memcode/imported/%s/ (reference — memcode memory is per-repo, so it is not auto-loaded)", src.slug), nil
}

// appendMemoryPointer records, once, a pointer in global memory.md so the running
// agent knows the imported history exists and where to find it. Idempotent: a
// second migration from the same source does not duplicate the note.
func appendMemoryPointer(home string, src migrationSource, importedDir string) error {
	path := filepath.Join(home, ".memcode", "memory.md")
	marker := "<!-- imported:" + src.slug + " -->"
	if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), marker) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	entry := fmt.Sprintf("\n## Imported from %s %s\nYour %s conversation history and memory were preserved at `%s`. "+
		"memcode's memory is per-repository, so this is reference material, not auto-loaded facts — "+
		"open those files to review, or copy specific facts into this file to keep them.\n",
		src.display, marker, src.display, importedDir)
	_, err = f.WriteString(entry)
	return err
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
	rootCmd.AddCommand(clawCmd)
	rootCmd.AddCommand(hermesCmd)
}
