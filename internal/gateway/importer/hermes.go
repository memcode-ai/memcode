package importer

import (
	"fmt"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v4"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// hermesConfig is the subset of a Hermes config.yaml we read. Hermes nests
// messaging under platforms.<name>; tokens live in ~/.hermes/.env under each
// platform's conventional variable name (the same names memcode uses), and
// allowed_users is the allow-list.
type hermesConfig struct {
	Platforms map[string]hermesPlatform `yaml:"platforms"`
}

type hermesPlatform struct {
	Token             any   `yaml:"token"` // usually resolved from .env; a literal is also honored
	AllowedUsers      []any `yaml:"allowed_users"`
	GroupAllowedUsers []any `yaml:"group_allowed_users"`
}

// FromHermes maps a Hermes config.yaml plus its .env (parsed to env) into memcode's
// gateway config. Hermes and memcode share the same credential variable names, so
// tokens carry over directly; allowed_users becomes channels.<name>.allow_from.
func FromHermes(configYAML []byte, env map[string]string) (Result, error) {
	var hc hermesConfig
	if err := yaml.Unmarshal(configYAML, &hc); err != nil {
		return Result{}, fmt.Errorf("parsing Hermes config: %w", err)
	}

	res := Result{
		Settings: gwconfig.Settings{Channels: map[string]gwconfig.Channel{}},
		Secrets:  map[string]string{},
	}

	names := make([]string, 0, len(hc.Platforms))
	for name := range hc.Platforms {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		p := hc.Platforms[name]
		allow := stripWildcard(name, mergeAllow(p.AllowedUsers, p.GroupAllowedUsers), &res.Notes)

		record := func() { res.Settings.Channels[name] = gwconfig.Channel{AllowFrom: allow} }
		// token resolves from the Hermes .env first (its canonical home), then a
		// literal in config.yaml.
		token := func(envKey string) string {
			if v := strings.TrimSpace(env[envKey]); v != "" {
				return v
			}
			if lit, ok := p.Token.(string); ok && !strings.HasPrefix(strings.TrimSpace(lit), "$") {
				return strings.TrimSpace(lit)
			}
			return ""
		}
		note := func(msg string) { res.Notes = append(res.Notes, msg) }

		switch name {
		case "telegram":
			if t := token(gwconfig.EnvTelegramToken); t != "" {
				res.Secrets[gwconfig.EnvTelegramToken] = t
			} else {
				note("telegram: no token found in the Hermes .env — set " + gwconfig.EnvTelegramToken + " or run `memcode gateway setup`")
			}
			record()
		case "discord":
			if t := token(gwconfig.EnvDiscordToken); t != "" {
				res.Secrets[gwconfig.EnvDiscordToken] = t
			} else {
				note("discord: no token found in the Hermes .env — set " + gwconfig.EnvDiscordToken)
			}
			record()
		case "slack":
			if t := strings.TrimSpace(env[gwconfig.EnvSlackBotToken]); t != "" {
				res.Secrets[gwconfig.EnvSlackBotToken] = t
			}
			if t := strings.TrimSpace(env[gwconfig.EnvSlackAppToken]); t != "" {
				res.Secrets[gwconfig.EnvSlackAppToken] = t
			} else {
				note("slack: SLACK_APP_TOKEN (Socket Mode) not found in the Hermes .env — add it with `memcode gateway setup`")
			}
			record()
		case "whatsapp", "signal", "matrix", "irc", "whatsapp_cloud":
			record()
			note(name + ": allow-list imported, but its credentials don't transfer to memcode — configure it with `memcode gateway setup`")
		default:
			note(fmt.Sprintf("%s: channel not supported by memcode — skipped", name))
		}
	}

	return res, nil
}

// ParseEnv reads a .env file's KEY=VALUE lines into a map, ignoring blanks,
// comments, and an optional "export " prefix. Used to lift tokens out of a
// Hermes ~/.hermes/.env for import.
func ParseEnv(data []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}
