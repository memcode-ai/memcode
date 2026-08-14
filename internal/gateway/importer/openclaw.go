// Package importer migrates an existing OpenClaw configuration into memcode's
// gateway config. OpenClaw is the large incumbent multi-channel gateway; letting
// a user bring their channels over with one command is how you win a switch
// without making them reconfigure everything. It reads OpenClaw's JSON5
// openclaw.json, maps each supported channel's credentials to memcode's .env keys
// and its allow-list to our channels.<name>.allow_from, and reports (never
// silently drops) anything it can't carry.
package importer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// Result is what an import produced: the non-secret settings to merge into
// gateway.yaml, the secrets to write to the global .env, and human-readable notes
// about anything that couldn't be carried automatically.
type Result struct {
	Settings gwconfig.Settings
	Secrets  map[string]string
	Notes    []string
}

// ocConfig is the subset of an OpenClaw config we read.
type ocConfig struct {
	Channels map[string]ocChannel `json:"channels"`
}

// ocChannel covers the credential and policy fields across OpenClaw's channels.
// Credentials are SecretInput (string literal, "$ENV" shorthand, or a
// {source,provider,id} object), so they're decoded as any and resolved later.
type ocChannel struct {
	BotToken       any   `json:"botToken"` // telegram, slack
	Token          any   `json:"token"`    // discord
	AppToken       any   `json:"appToken"` // slack
	AllowFrom      []any `json:"allowFrom"`
	GroupAllowFrom []any `json:"groupAllowFrom"`
	DM             *struct {
		AllowFrom []any `json:"allowFrom"` // discord legacy: dm.allowFrom
	} `json:"dm"`
}

// FromOpenClaw parses an OpenClaw config and maps it to memcode's gateway config.
// getenv resolves env-backed secret references (OpenClaw stores a reference, not
// the value); pass os.Getenv in production.
func FromOpenClaw(data []byte, getenv func(string) string) (Result, error) {
	// OpenClaw writes plain JSON (it strips comments on save), so encoding/json
	// handles the configs it produces. A hand-edited config with JSON5 comments or
	// trailing commas will fail here — surface that clearly rather than pulling a
	// whole JSON5 (and its JS-VM test deps) into the CLI.
	var oc ocConfig
	if err := json.Unmarshal(data, &oc); err != nil {
		return Result{}, fmt.Errorf("parsing OpenClaw config (must be JSON; strip comments/trailing commas if hand-edited, or run `openclaw doctor --fix` first): %w", err)
	}

	res := Result{
		Settings: gwconfig.Settings{Channels: map[string]gwconfig.Channel{}},
		Secrets:  map[string]string{},
	}

	// Deterministic order so notes and output are stable.
	names := make([]string, 0, len(oc.Channels))
	for name := range oc.Channels {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		ch := oc.Channels[name]
		lists := [][]any{ch.AllowFrom, ch.GroupAllowFrom}
		if ch.DM != nil {
			lists = append(lists, ch.DM.AllowFrom) // discord legacy dm.allowFrom
		}
		allow := mergeAllow(lists...)

		record := func() {
			res.Settings.Channels[name] = gwconfig.Channel{AllowFrom: allow}
		}
		cred := func(field string, v any, envKey string) {
			val, note := resolveSecret(v, getenv)
			if note != "" {
				res.Notes = append(res.Notes, fmt.Sprintf("%s %s: %s — set %s or run `memcode gateway setup`", name, field, note, envKey))
			}
			if val != "" {
				res.Secrets[envKey] = val
			}
		}

		switch name {
		case "telegram":
			cred("botToken", ch.BotToken, gwconfig.EnvTelegramToken)
			record()
		case "discord":
			cred("token", ch.Token, gwconfig.EnvDiscordToken)
			record()
		case "slack":
			cred("botToken", ch.BotToken, gwconfig.EnvSlackBotToken)
			cred("appToken", ch.AppToken, gwconfig.EnvSlackAppToken)
			record()
		case "whatsapp":
			// OpenClaw's WhatsApp is a QR-linked Baileys session; memcode's is the
			// Meta Cloud API. The credentials don't transfer, but the allow-list of
			// phone numbers does.
			record()
			res.Notes = append(res.Notes, "whatsapp: allow-list imported, but WhatsApp Cloud API credentials (phone number id, access/verify tokens, app secret) don't transfer from OpenClaw — add them with `memcode gateway setup`")
		default:
			res.Notes = append(res.Notes, fmt.Sprintf("%s: channel not supported by memcode — skipped", name))
		}
	}

	return res, nil
}

// resolveSecret turns an OpenClaw SecretInput into a concrete value, or returns a
// note explaining why it couldn't. A plain string is a literal; "$NAME"/"${NAME}"
// and {source:"env",id:"NAME"} reference an env var we read via getenv; other
// sources (file/exec/store) can't be resolved here.
func resolveSecret(v any, getenv func(string) string) (value, note string) {
	switch t := v.(type) {
	case nil:
		return "", ""
	case string:
		if name, ok := envShorthand(t); ok {
			if val := getenv(name); val != "" {
				return val, ""
			}
			return "", "references env var " + name + " which isn't set"
		}
		return t, "" // literal value
	case map[string]any:
		source, _ := t["source"].(string)
		id, _ := t["id"].(string)
		switch source {
		case "env":
			if val := getenv(id); val != "" {
				return val, ""
			}
			return "", "references env var " + id + " which isn't set"
		case "":
			return "", "unrecognized credential format"
		default:
			return "", "uses an external secret provider (source=" + source + ")"
		}
	default:
		return "", "unrecognized credential format"
	}
}

// envShorthand recognizes "$NAME" and "${NAME}" and returns NAME.
func envShorthand(s string) (string, bool) {
	if !strings.HasPrefix(s, "$") {
		return "", false
	}
	name := strings.TrimPrefix(s, "$")
	name = strings.TrimPrefix(name, "{")
	name = strings.TrimSuffix(name, "}")
	if name == "" {
		return "", false
	}
	return name, true
}

// mergeAllow flattens allow-list sources into a de-duplicated string slice.
// OpenClaw entries may be strings or numbers (chat/user ids).
func mergeAllow(lists ...[]any) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, v := range list {
			s := anyToString(v)
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}
