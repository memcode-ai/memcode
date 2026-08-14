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
)

// Settings is the NON-secret gateway configuration (gateway.yaml). A channel's
// presence is decided by its secret in .env (see EnabledChannels); the blocks
// here only carry the non-secret knobs a channel needs.
type Settings struct {
	Webhook  Webhook  `yaml:"webhook,omitempty"`
	GitHub   GitHub   `yaml:"github,omitempty"`
	WhatsApp WhatsApp `yaml:"whatsapp,omitempty"`
}

// Webhook is the inbound HTTP listener shared by GitHub/WhatsApp. Defaults to
// ":8787" when a webhook-using channel is enabled but no address is set.
type Webhook struct {
	Addr string `yaml:"addr,omitempty"`
}

// GitHub: ReplyTo routes an autonomous result to a chat conversation, e.g.
// "telegram:123456". The webhook secret is a secret and lives in .env.
type GitHub struct {
	ReplyTo string `yaml:"reply_to,omitempty"`
}

// WhatsApp: the non-secret phone number ID. Access + verify tokens live in .env.
// Active gates the adapter: it stays inert (built but not mounted) until the
// Meta business is verified and the operator flips this to true — verification
// is an external account state with no programmatic signal, so it's a manual
// switch, not something the gateway can detect.
type WhatsApp struct {
	PhoneNumberID string `yaml:"phone_number_id,omitempty"`
	Active        bool   `yaml:"active,omitempty"`
}

// Path returns the gateway settings file: $XDG_CONFIG_HOME/memcode/gateway.yaml
// or ~/.config/memcode/gateway.yaml.
func Path() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("no home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "memcode", "gateway.yaml"), nil
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

// Save writes gateway.yaml atomically. 0644 — it holds no secrets.
func Save(s Settings) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(p, b, 0o644)
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
