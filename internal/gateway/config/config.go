// Package config is the gateway's self-hosted configuration, split the way
// memcode already splits everything (and the way Hermes does): secrets — the bot
// tokens — live in the global .env (written by `memcode gateway setup`, never
// hand-set), and NON-secret settings live here in gateway.yaml. Both sit in the
// global memcode config dir (per machine, not per project). This file names the
// secret env keys and models the YAML so one place owns the whole shape.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yaml "go.yaml.in/yaml/v4"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// Secret env keys. These live in the global .env (provider.GlobalEnvPath), NOT
// in gateway.yaml — a bot token is a secret, and secrets belong in .env. The
// names are each platform's OWN conventional variable (no memcode prefix), so a
// user can paste the value straight from the platform's docs and so a config
// imported from another gateway (Hermes, OpenClaw) drops in unchanged. Only
// memcode's own infra (MEMCODE_API_TOKEN, …) carries the project prefix.
const (
	EnvTelegramToken  = "TELEGRAM_BOT_TOKEN"
	EnvDiscordToken   = "DISCORD_BOT_TOKEN"
	EnvSlackAppToken  = "SLACK_APP_TOKEN"
	EnvSlackBotToken  = "SLACK_BOT_TOKEN"
	EnvGitHubSecret   = "GITHUB_WEBHOOK_SECRET"
	EnvWhatsAppToken  = "WHATSAPP_ACCESS_TOKEN"
	EnvWhatsAppVerify = "WHATSAPP_VERIFY_TOKEN"
	EnvWhatsAppSecret = "WHATSAPP_APP_SECRET" // Meta app secret — signs inbound POSTs
)

// Settings is the NON-secret gateway configuration (gateway.yaml). A channel's
// presence is decided by its secret in .env (see EnabledChannels); the per-channel
// blocks under Channels carry the non-secret knobs and the access list. The shape
// mirrors what Hermes and OpenClaw use (a channels.<name> object), so a config can
// be imported from either with a direct field mapping.
type Settings struct {
	// AllowAll disables the per-channel allow-list entirely — anyone who can reach
	// a channel may drive the agent. Defaults false: the gateway is default-deny,
	// so an unconfigured channel answers no one until you add yourself.
	AllowAll  bool               `yaml:"allow_all,omitempty"`
	Webhook   Webhook            `yaml:"webhook,omitempty"`
	Channels  map[string]Channel `yaml:"channels,omitempty"`
	Schedules []Schedule         `yaml:"schedules,omitempty"`
	// Projects is the registry of working directories the gateway may execute
	// against (added with `memcode project add`). A remote message may select
	// among these; it can never manufacture an arbitrary filesystem root.
	Projects map[string]Project `yaml:"projects,omitempty"`
	// DefaultProject is the project id the gateway executes against when a task
	// carries no explicit project (all of them, until conversations land).
	DefaultProject string `yaml:"default_project,omitempty"`
}

// Project is a registered working directory. Path is the configured location;
// the AUTHORITY is its canonicalized form (see ResolveProject), resolved at use
// time so a symlink swap can't redirect execution. Registration (is this path
// runnable at all?) is deliberately distinct from authorization (may THIS
// principal/agent run against it?) — the initial trust model is that every
// allow-listed gateway principal may execute against every enabled project; a
// principal→agent→projects policy is a later primitive.
type Project struct {
	Path    string `yaml:"path"`
	Enabled bool   `yaml:"enabled"`
}

// ResolveProject resolves a registered project id to its canonical filesystem
// root, enforcing the registration boundary: only a registered + enabled project
// resolves, and the returned root — not the raw config string — is the authority
// a task executes against.
func (s Settings) ResolveProject(id string) (string, error) {
	p, ok := s.Projects[id]
	if !ok {
		return "", fmt.Errorf("unknown project %q — register it with `memcode project add`", id)
	}
	if !p.Enabled {
		return "", fmt.Errorf("project %q is disabled", id)
	}
	root, err := CanonicalRoot(p.Path)
	if err != nil {
		return "", fmt.Errorf("project %q: %w", id, err)
	}
	return root, nil
}

// CanonicalRoot expands a leading ~ and resolves path to an absolute,
// symlink-free directory. The resolved directory is the execution authority — a
// task's root must equal it, so registration alone can't be tricked by a symlink
// into executing elsewhere.
func CanonicalRoot(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return resolved, nil
}

// Schedule is a time-triggered task: the gateway runs Task on the given cadence
// and posts the result to DeliverTo ("<channel>:<conversation>", e.g.
// "telegram:123456"). Set exactly one of Every (a Go duration like "24h" or
// "30m") or Cron (a 5-field cron expression like "0 9 * * 1-5"). This is what
// turns the gateway from purely reactive into autonomous.
type Schedule struct {
	Name      string `yaml:"name"`
	Every     string `yaml:"every,omitempty"`
	Cron      string `yaml:"cron,omitempty"`
	Task      string `yaml:"task"`
	DeliverTo string `yaml:"deliver_to"`
}

// Webhook is the inbound HTTP listener shared by GitHub/WhatsApp. Defaults to
// ":8787" when a webhook-using channel is enabled but no address is set.
type Webhook struct {
	Addr string `yaml:"addr,omitempty"`
}

// Channel is a channel's non-secret configuration.
type Channel struct {
	// AllowFrom is the set of stable user ids permitted to drive the agent through
	// this channel; "*" allows anyone on the channel. Empty means no one is
	// allowed (unless the global AllowAll is set). Use stable ids, not @handles —
	// authorization is on ids. Secrets never live here; bot tokens are in the .env.
	AllowFrom []string `yaml:"allow_from,omitempty"`
	// RespondToAll makes the bot act on every message in a group/channel it can
	// see. Default false: in a group the bot only acts when it is mentioned, so it
	// doesn't spawn a paid agent job for ordinary chatter. Direct messages always
	// trigger regardless of this setting.
	RespondToAll bool `yaml:"respond_to_all,omitempty"`
	// Tier routes this channel's agent runs to a stronger model tier: "strong"
	// (the strong vendor's balanced tier) or "frontier" (top). Empty is automatic
	// routing (cheap for routine work). Lets a code-review channel run strong while
	// a status channel stays cheap.
	Tier string `yaml:"tier,omitempty"`
	// ReplyTo (GitHub) routes an autonomous result to a chat conversation, e.g.
	// "telegram:123456".
	ReplyTo string `yaml:"reply_to,omitempty"`
	// PhoneNumberID (WhatsApp) is the non-secret Cloud API sender id.
	PhoneNumberID string `yaml:"phone_number_id,omitempty"`
	// Active (WhatsApp) gates the adapter: it stays inert (built but not mounted)
	// until the Meta business is verified and the operator flips this to true —
	// verification is an external account state the gateway can't detect.
	Active bool `yaml:"active,omitempty"`
}

// Get returns the settings for a channel (a zero Channel if unset), so callers
// don't repeat nil-map/missing-key handling.
func (s Settings) Get(name string) Channel {
	return s.Channels[name]
}

// Allowed reports whether principal may drive the agent through channel. It is
// default-deny: only the global AllowAll, an explicit "*", or an exact principal
// match grants access.
func (s Settings) Allowed(channel, principal string) bool {
	if s.AllowAll {
		return true
	}
	for _, p := range s.Channels[channel].AllowFrom {
		if p == "*" || p == principal {
			return true
		}
	}
	return false
}

// Dir returns the global memcode config directory: $XDG_CONFIG_HOME/memcode or
// ~/.config/memcode. Per machine, not per project — the home for gateway.yaml,
// the global .env, and the gateway's OWN operational state (durable inbox,
// singleton lock, event log). A gateway therefore never writes its operational
// state into a repo's .memcode.
func Dir() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("no home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "memcode"), nil
}

// Path returns the gateway settings file inside Dir():
// $XDG_CONFIG_HOME/memcode/gateway.yaml or ~/.config/memcode/gateway.yaml.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gateway.yaml"), nil
}

// Load reads gateway.yaml, returning zero Settings if the file does not exist.
func Load() (Settings, error) {
	p, err := Path()
	if err != nil {
		return Settings{}, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	var s Settings
	if err := yaml.Unmarshal(b, &s); err != nil {
		return Settings{}, fmt.Errorf("parsing %s: %w", p, err)
	}
	return s, nil
}

// Save writes gateway.yaml atomically. 0600 — it holds no secrets, but the
// allow-list of user ids is sensitive on a shared host, so keep it owner-only.
func Save(s Settings) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(p, b, 0o600)
}

// EnabledChannels lists channels whose required secret(s) are present in the
// environment. The global .env must be loaded first (provider.LoadDotEnv).
func EnabledChannels() []string {
	var names []string
	if os.Getenv(EnvTelegramToken) != "" {
		names = append(names, "telegram")
	}
	if os.Getenv(EnvDiscordToken) != "" {
		names = append(names, "discord")
	}
	if os.Getenv(EnvSlackAppToken) != "" && os.Getenv(EnvSlackBotToken) != "" {
		names = append(names, "slack")
	}
	if os.Getenv(EnvGitHubSecret) != "" {
		names = append(names, "github")
	}
	if os.Getenv(EnvWhatsAppToken) != "" {
		names = append(names, "whatsapp")
	}
	return names
}
