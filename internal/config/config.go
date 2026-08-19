// Package config loads and persists per-project memcode configuration,
// stored under a .memcode directory at the project root.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/provider"
)

// DirName is the per-project directory that holds memcode state.
const DirName = ".memcode"

// ConfigFile is the config filename inside DirName.
const ConfigFile = "config.json"

// ModelTiers maps the engine's model roles to concrete model ids. Values may be
// aliases ("opus"|"sonnet"|"haiku") or full model ids; resolve with
// provider.ResolveAlias.
type ModelTiers struct {
	Planner    string `json:"planner"`    // hard reasoning / planning
	Coder      string `json:"coder"`      // the everyday default / agent
	Classifier string `json:"classifier"` // the reducer's cheap router
	Explorer   string `json:"explorer"`   // read-only scout sub-agents (cheap; Haiku by default)
}

// SyncTarget names an AI-editor context file that memcode can keep in sync.
type SyncTarget string

const (
	SyncTargetClaude   SyncTarget = "claude"   // CLAUDE.md
	SyncTargetCodex    SyncTarget = "codex"    // AGENTS.md (the cross-tool standard)
	SyncTargetCopilot  SyncTarget = "copilot"  // .github/copilot-instructions.md
	SyncTargetCursor   SyncTarget = "cursor"   // .cursor/rules
	SyncTargetGemini   SyncTarget = "gemini"   // GEMINI.md
	SyncTargetWindsurf SyncTarget = "windsurf" // .windsurfrules
)

// SyncTargetMeta is display metadata for a sync target. Name (lowercased) must
// equal the SyncTarget constant — that's how stored targets resolve back to a file.
type SyncTargetMeta struct {
	Name string
	Path string // relative to project root
}

// SyncTargetAll is the ordered list of all supported targets. AGENTS.md (Codex) is
// the emerging cross-editor standard; the rest are tool-specific. Add a row here +
// a SyncTarget constant to support a new editor — nothing else needs to change.
var SyncTargetAll = []SyncTargetMeta{
	{Name: "Claude", Path: "CLAUDE.md"},
	{Name: "Codex", Path: "AGENTS.md"},
	{Name: "Copilot", Path: ".github/copilot-instructions.md"},
	{Name: "Cursor", Path: ".cursor/rules"},
	{Name: "Gemini", Path: "GEMINI.md"},
	{Name: "Windsurf", Path: ".windsurfrules"},
}

// Config is the per-project configuration.
//
// NOTE: API keys are NEVER stored here. They are read from the environment
// only — the CLI holds no provider keys at all; the gateway holds them.
type Config struct {
	// Root is the absolute path to the project root (the dir holding .memcode).
	Root string `json:"-"`

	// Models selects the model per tier (see ModelTiers).
	Models ModelTiers `json:"models"`

	// Exclude is a list of glob patterns skipped during indexing.
	Exclude []string `json:"exclude"`

	// Sync controls which AI-editor context files memcode keeps in sync.
	// "everything" means all detected files plus any added in the future.
	// Otherwise it's a list of target names (see SyncTarget constants).
	Sync SyncConfig `json:"sync,omitempty"`

	// Mode is the remembered permission mode (ask|auto|allow-all) — persisted when
	// the user cycles it (Shift+Tab) or sets it via /mode, so the choice survives
	// across sessions. Empty = use the default. Explicit `--auto`/`--allow-all`
	// flags are one-offs and do NOT overwrite this.
	Mode  string `json:"mode,omitempty"`  // ask|auto|allow-all — persisted when cycled or /mode
	Theme string `json:"theme,omitempty"` // color theme name; empty means aurora (default)

	// Vendor is the remembered strong-tier vendor (set by /model): "openai" |
	// "anthropic" | "gemini" | "grok". Empty = the configured default (BYOK
	// steering may prefer a keyed vendor). Persisted so the choice survives
	// across sessions. The catalog's tier triples decide which MODEL within
	// the vendor.
	Vendor string `json:"vendor,omitempty"`

	// PinnedModel is the remembered /model pin: a catalog model label ("sonnet",
	// "glm-5p2") the whole session runs on — main loop, planner, reviewer, agents
	// (invisible plumbing stays on the utility lanes). Empty = Automatic (the
	// ladder decides). A stale label is harmless: the resolver falls through to
	// Automatic for labels it doesn't recognize.
	PinnedModel string `json:"pinned_model,omitempty"`

	// PinnedWindow caches the pin's context window (tokens) from the picker list,
	// so the ctx meter is sized right on launch before the first serve reports one.
	PinnedWindow int `json:"pinned_window,omitempty"`

	// Personality is the chosen agent voice (a built-in key like "joker"/"mirror" or a
	// free-text custom voice). It travels to the gateway as a fact and is realized there
	// as a tone-only prose envelope; empty means the default neutral voice. Set via
	// /personality. Voice ONLY — it never affects behavior or the permission floor.
	Personality string `json:"personality,omitempty"`

	// ExtraMile, when true, asks the agent to go above and beyond the literal request —
	// checking edge cases and feature completeness — on every plan and execution. It travels
	// to the gateway as a fact and is realized there as an extra prompt rule for the
	// planner/executor. Off by default; set via /extramile. Consumes extra tokens.
	ExtraMile bool `json:"extra_mile,omitempty"`

	// ServingDefault caches the gateway's everyday (cheap-lane) model id — learned from
	// /v1/models at startup and persisted, so the banner + footer show what actually runs
	// (e.g. glm-5p2) immediately on the NEXT launch instead of the CLI's bootstrap identity
	// (sonnet) until the async fetch returns. Refreshed each launch; cosmetic only.
	ServingDefault string `json:"serving_default,omitempty"`

	// CommitBeforeWork is the remembered answer to the "you have uncommitted
	// changes — commit before starting a large work block?" prompt: "" = ask each
	// time (default), "commit" = always checkpoint-commit the tree silently and
	// continue, "skip" = never prompt, just proceed on the dirty tree. Persisted
	// when the user picks either "don't ask again" option on the card.
	CommitBeforeWork string `json:"commit_before_work,omitempty"`

	// Endpoints is the named custom-endpoint list (one-wire Phase C): arbitrary
	// OpenAI-compatible backends (Ollama, LM Studio, vLLM, a provider cloud)
	// the CLI can run against without a memcode account. Selection: a memcode
	// login always wins; else MEMCODE_ENDPOINT_URL wins; else the entry named
	// by Endpoint below ("" = the first). Keys are NEVER stored here (see the
	// package rule above) — KeyEnv names the env var that holds one.
	Endpoints []Endpoint `json:"endpoints,omitempty"`

	// Endpoint selects the active Endpoints entry by name ("" = the first).
	Endpoint string `json:"endpoint,omitempty"`
}

// Endpoint is one named OpenAI-compatible endpoint in the config list.
type Endpoint struct {
	// Name is the entry's id — what Config.Endpoint selects and the UI shows.
	Name string `json:"name"`
	// BaseURL is the FULL compat base incl. any path prefix, e.g.
	// http://localhost:11434/v1 ({base}/chat/completions is the turn endpoint).
	BaseURL string `json:"base_url"`
	// KeyEnv names the ENV VAR holding this endpoint's API key (dotenv chain
	// applies). The key itself never lives in config.json.
	KeyEnv string `json:"key_env,omitempty"`
	// Models is an optional curated model list: when set, the /model picker
	// offers exactly these (plus free-text) instead of GET {base}/models.
	Models []string `json:"models,omitempty"`
	// LastModel is the remembered /model choice for this endpoint — the
	// session model on the next launch.
	LastModel string `json:"last_model,omitempty"`
}

// SyncConfig stores the user's /sync preferences.
type SyncConfig struct {
	// Everything syncs all detected targets plus any new ones automatically.
	Everything bool `json:"everything,omitempty"`
	// Targets is the explicit list when Everything is false.
	Targets []SyncTarget `json:"targets,omitempty"`
}

// Save persists the config back to disk (root must be set).
func (c *Config) Save() error {
	if c.Root == "" {
		return fmt.Errorf("config has no root set")
	}
	path := filepath.Join(c.Root, DirName, ConfigFile)
	return write(path, *c)
}

// ErrNotInitialized is returned when no .memcode directory is found.
var ErrNotInitialized = errors.New("memcode is not initialized in this project")

// defaultConfig returns the configuration written by `memcode init`.
func defaultConfig() Config {
	return Config{
		Models: ModelTiers{
			Planner:    provider.DefaultModel(provider.TierPlanner),
			Coder:      provider.DefaultModel(provider.TierCoder),
			Classifier: provider.DefaultModel(provider.TierClassifier),
			Explorer:   "haiku", // legacy alias — resolves to the cheap tier; gateway ignores the CLI's model field anyway
		},
		Exclude: []string{
			"node_modules", ".git", "dist", "build", ".next", "vendor",
		},
	}
}

// Init creates the .memcode directory and config file at root. It returns
// whether new configuration was written. Passing force overwrites an existing
// config file.
func Init(root string, force bool) (created bool, err error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}

	dir := filepath.Join(abs, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	EnsureGitignore(abs) // make .memcode self-ignoring

	path := filepath.Join(dir, ConfigFile)
	if _, statErr := os.Stat(path); statErr == nil && !force {
		return false, nil
	}

	cfg := defaultConfig()
	if err := write(path, cfg); err != nil {
		return false, err
	}
	return true, nil
}

// EnsureGitignore makes .memcode self-ignoring — a `*` .gitignore INSIDE the
// directory — so it fully disappears from the user's `git status` and never lands in
// their repo, WITHOUT touching the user's own root .gitignore (we don't edit files
// they curate). .memcode is memcode's local metadata, like .git: synced its own way,
// not committed. The rule is bare `*` (which ignores the .gitignore itself too) — an
// `!.gitignore` exception would leave a non-ignored file inside and the directory
// would still show in `git status`; memcode re-creates this file every run, so it
// never needs to be committed.
//
// PUBLIC and called on EVERY launch (openProject), not just first-time Init — an
// already-initialized project still needs the self-ignore. Idempotent; no-op if
// .memcode doesn't exist yet; never clobbers a user-edited file; and migrates our own
// earlier `*`+`!.gitignore` template that didn't hide the dir.
func EnsureGitignore(root string) {
	gi := filepath.Join(root, DirName, ".gitignore")
	const content = "# memcode's local state — not part of your repo\n*\n"
	const priorTemplate = "# memcode's local state — not part of your repo\n*\n!.gitignore\n"
	if b, err := os.ReadFile(gi); err == nil && string(b) != priorTemplate {
		return // exists and isn't our migratable template — leave it alone
	}
	_ = os.WriteFile(gi, []byte(content), 0o644)
}

// Source describes how the project root was resolved (for `memcode doctor`).
type Source string

const (
	SourceGit      Source = "git toplevel"
	SourceExisting Source = "existing .memcode"
	SourceCWD      Source = "working directory"
)

// Resolve determines the canonical project root for memcode state. To avoid
// creating divergent state depending on the working directory, it prefers the
// git repository root; only outside a repo does it fall back to an existing
// .memcode ancestor, then the working directory itself.
func Resolve(start string) (root string, src Source, err error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", "", err
	}
	if top, ok := gitToplevel(abs); ok {
		return top, SourceGit, nil
	}
	if dir, err := findRoot(abs); err == nil {
		return dir, SourceExisting, nil
	}
	return abs, SourceCWD, nil
}

// Load reads the configuration for the project rooted at the canonical root for
// start (see Resolve). It is an error if the project hasn't been initialized.
func Load(start string) (*Config, error) {
	root, _, err := Resolve(start)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(root, DirName, ConfigFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	cfg.Root = root
	cfg.applyDefaults()
	// Self-heal the v0.25.x subscription-as-endpoint artifact: prune
	// subscription-named endpoint entries and persist the cleaned config so
	// the artifact disappears for good (best-effort — a read-only checkout
	// still works via ResolveEndpoint's runtime guard).
	if pruned := cfg.pruneSubscriptionEndpoints(); pruned {
		_ = cfg.Save()
	}
	if err := cfg.Validate(path); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// pruneSubscriptionEndpoints drops endpoint entries that are really
// subscription lanes (persisted by the v0.25.x exclusive mode), reporting
// whether anything changed.
func (c *Config) pruneSubscriptionEndpoints() bool {
	var kept []Endpoint
	for _, e := range c.Endpoints {
		if provider.SubscriptionEndpointName(e.Name) {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == len(c.Endpoints) {
		return false
	}
	c.Endpoints = kept
	return true
}

// applyDefaults fills model tiers that have a documented fallback, so a config written
// before a tier existed (or left blank) still loads instead of hard-failing. Explorer —
// the read-only scout lane — defaults to Haiku, exactly as its field doc promises. Planner
// and Coder have no default and stay required (see Validate).
func (c *Config) applyDefaults() {
	if c.Models.Explorer == "" {
		c.Models.Explorer = "haiku" // resolves via provider.ResolveAlias
	}
	if c.Vendor == "" {
		c.Vendor = "openai" // the gateway default strong tier in hybrid mode
	}
}

// Validate checks the config for values that would fail downstream — currently
// that every required model tier has a non-empty value. path is the config file
// path (passed in so the error names the file the user needs to edit). Empty
// tiers are sent to the gateway as empty model ids, which produce opaque errors;
// this catches them locally with an actionable message instead.
func (c *Config) Validate(path string) error {
	var missing []string
	if c.Models.Coder == "" {
		missing = append(missing, "models.coder")
	}
	if c.Models.Planner == "" {
		missing = append(missing, "models.planner")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s: model tier(s) must not be empty — set %s in %s",
		strings.Join(missing, ", "), strings.Join(missing, ", "), path)
}

// ── custom endpoints (one-wire Phase C) ─────────────────────────────────────

// ActiveEndpoint resolves the config-selected endpoint entry: the one named by
// Config.Endpoint, else the first. ok=false for an empty list or a dangling
// name (a typo must not silently select a different backend).
func (c *Config) ActiveEndpoint() (Endpoint, bool) {
	if len(c.Endpoints) == 0 {
		return Endpoint{}, false
	}
	if c.Endpoint == "" {
		return c.Endpoints[0], true
	}
	for _, e := range c.Endpoints {
		if e.Name == c.Endpoint {
			return e, true
		}
	}
	return Endpoint{}, false
}

// ResolveEndpoint resolves the ACTIVE custom endpoint for this project as the
// provider dials it, env first: MEMCODE_ENDPOINT_URL names the endpoint (key /
// initial model from MEMCODE_ENDPOINT_KEY/_MODEL), enriched from a matching
// config entry (same base URL) — its remembered LastModel, curated Models, and
// KeyEnv fill whatever the env leaves unset; else the config-selected entry
// stands alone. The session model resolves remembered-last-model → env/initial
// → the first curated model ("" = the picker/boot autodetect decides). ok=false
// when neither env nor config configures an endpoint. A memcode login outranks
// both — that selection lives in provider.NewFromEnv/NewFromEnvLazy.
func (c *Config) ResolveEndpoint() (provider.Endpoint, bool) {
	if ep, ok := provider.EndpointFromEnv(); ok {
		if entry, found := c.endpointByBase(ep.BaseURL); found {
			if entry.Name != "" {
				ep.Name = entry.Name
			}
			ep.Models = entry.Models
			if entry.LastModel != "" {
				ep.Model = entry.LastModel // a remembered /model choice beats the env INITIAL
			}
			if ep.Key == "" && entry.KeyEnv != "" {
				ep.Key = strings.TrimSpace(os.Getenv(entry.KeyEnv))
			}
		}
		if ep.Model == "" && len(ep.Models) > 0 {
			ep.Model = ep.Models[0]
		}
		return ep, true
	}
	entry, ok := c.ActiveEndpoint()
	if !ok || strings.TrimSpace(entry.BaseURL) == "" {
		return provider.Endpoint{}, false
	}
	// v0.25.x persisted subscription sessions as config ENDPOINTS
	// (name "claude-sub" → api.anthropic.com). Subscriptions are family
	// lanes now — treating the artifact as an exclusive endpoint would put
	// the whole session back on one vendor (the kimi-k3-404-at-Anthropic
	// bug). Ignore it; Load prunes it from disk.
	if provider.SubscriptionEndpointName(entry.Name) {
		return provider.Endpoint{}, false
	}
	ep := provider.Endpoint{
		Name:    entry.Name,
		BaseURL: strings.TrimSpace(entry.BaseURL),
		Models:  entry.Models,
		Model:   entry.LastModel,
	}
	if ep.Name == "" {
		ep.Name = provider.EndpointName(ep.BaseURL)
	}
	if entry.KeyEnv != "" {
		ep.Key = strings.TrimSpace(os.Getenv(entry.KeyEnv))
	}
	if ep.Model == "" && len(entry.Models) > 0 {
		ep.Model = entry.Models[0]
	}
	return ep, true
}

// RememberEndpointModel records a /model choice for an endpoint in this
// project's config: the matching entry (by base URL) gets LastModel set, and
// an env-defined endpoint with no entry yet gets one appended so the choice
// survives relaunches. The caller Saves.
func (c *Config) RememberEndpointModel(ep provider.Endpoint, model string) {
	base := strings.TrimRight(strings.TrimSpace(ep.BaseURL), "/")
	for i := range c.Endpoints {
		if strings.TrimRight(strings.TrimSpace(c.Endpoints[i].BaseURL), "/") == base {
			c.Endpoints[i].LastModel = model
			return
		}
	}
	name := ep.Name
	if name == "" {
		name = provider.EndpointName(ep.BaseURL)
	}
	c.Endpoints = append(c.Endpoints, Endpoint{Name: name, BaseURL: ep.BaseURL, LastModel: model})
}

// endpointByBase finds the config entry for a base URL (trailing-slash
// insensitive).
func (c *Config) endpointByBase(base string) (Endpoint, bool) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	for _, e := range c.Endpoints {
		if strings.TrimRight(strings.TrimSpace(e.BaseURL), "/") == base {
			return e, true
		}
	}
	return Endpoint{}, false
}

// gitToplevel returns the git working-tree root containing start, if any.
func gitToplevel(start string) (string, bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", false
	}
	cmd := exec.Command("git", "-C", start, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	top := strings.TrimSpace(string(out))
	return top, top != ""
}

// findRoot walks up from start looking for a directory containing .memcode.
func findRoot(start string) (string, error) {
	dir := start
	for {
		if info, err := os.Stat(filepath.Join(dir, DirName)); err == nil && info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotInitialized
		}
		dir = parent
	}
}

func write(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Atomic: a crash mid-write must not truncate config.json — Load hard-fails on invalid
	// JSON, which would block the CLI from starting in this project.
	return atomicfile.WriteFile(path, append(data, '\n'), 0o644)
}
