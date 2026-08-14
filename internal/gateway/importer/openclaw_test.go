package importer

import (
	"sort"
	"strings"
	"testing"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

func TestFromOpenClaw(t *testing.T) {
	// A real OpenClaw config (plain JSON, as OpenClaw writes it) exercising each
	// credential form: an env-ref (telegram), a literal (discord), the legacy
	// dm.allowFrom shape (discord), and multi-secret slack.
	cfg := `{
  "channels": {
    "telegram": {
      "enabled": true,
      "botToken": { "source": "env", "provider": "default", "id": "TELEGRAM_BOT_TOKEN" },
      "allowFrom": ["123", 456],
      "groupAllowFrom": [789]
    },
    "discord": {
      "token": "literal-discord-token",
      "dm": { "policy": "allowlist", "allowFrom": ["111111111111111111"] }
    },
    "slack": {
      "botToken": "xoxb-abc",
      "appToken": "xapp-def",
      "allowFrom": ["*"]
    },
    "signal": { "account": "+15555550123" }
  }
}`

	env := map[string]string{"TELEGRAM_BOT_TOKEN": "tg-secret-from-env"}
	res, err := FromOpenClaw([]byte(cfg), func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("FromOpenClaw: %v", err)
	}

	// Secrets: telegram resolved from env, discord/slack from literals.
	wantSecrets := map[string]string{
		gwconfig.EnvTelegramToken: "tg-secret-from-env",
		gwconfig.EnvDiscordToken:  "literal-discord-token",
		gwconfig.EnvSlackBotToken: "xoxb-abc",
		gwconfig.EnvSlackAppToken: "xapp-def",
	}
	for k, want := range wantSecrets {
		if got := res.Secrets[k]; got != want {
			t.Errorf("secret %s = %q, want %q", k, got, want)
		}
	}

	// Allow-lists: telegram merges allowFrom + groupAllowFrom (numbers → strings);
	// discord picks up the legacy dm.allowFrom; slack keeps the wildcard.
	assertAllow(t, res.Settings, "telegram", []string{"123", "456", "789"})
	assertAllow(t, res.Settings, "discord", []string{"111111111111111111"})
	assertAllow(t, res.Settings, "slack", []string{"*"})

	// Signal isn't supported → skipped with a note, not imported.
	if _, ok := res.Settings.Channels["signal"]; ok {
		t.Error("signal should not be imported")
	}
	if !hasNoteContaining(res.Notes, "signal") {
		t.Errorf("expected a note about signal being skipped, got %v", res.Notes)
	}

	// The imported allow-list actually authorizes as expected.
	if !res.Settings.Allowed("telegram", "123") {
		t.Error("imported telegram allow-list should permit 123")
	}
	if res.Settings.Allowed("telegram", "999") {
		t.Error("telegram allow-list should not permit an unlisted id")
	}
}

func TestFromOpenClawUnresolvedEnvRef(t *testing.T) {
	cfg := `{"channels":{"telegram":{"botToken":{"source":"env","id":"TELEGRAM_BOT_TOKEN"}}}}`
	res, err := FromOpenClaw([]byte(cfg), func(string) string { return "" }) // env not set
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Secrets[gwconfig.EnvTelegramToken]; ok {
		t.Error("no secret should be written when the env ref is unset")
	}
	if !hasNoteContaining(res.Notes, "TELEGRAM_BOT_TOKEN") {
		t.Errorf("expected a note about the unset env ref, got %v", res.Notes)
	}
}

func TestFromOpenClawExternalProvider(t *testing.T) {
	cfg := `{"channels":{"discord":{"token":{"source":"exec","provider":"onepassword","id":"op://vault/discord"}}}}`
	res, err := FromOpenClaw([]byte(cfg), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Secrets) != 0 {
		t.Errorf("external provider secret should not be resolved, got %v", res.Secrets)
	}
	if !hasNoteContaining(res.Notes, "external secret provider") {
		t.Errorf("expected a note about the external provider, got %v", res.Notes)
	}
}

func assertAllow(t *testing.T, s gwconfig.Settings, channel string, want []string) {
	t.Helper()
	got := append([]string(nil), s.Channels[channel].AllowFrom...)
	sort.Strings(got)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if strings.Join(got, ",") != strings.Join(w, ",") {
		t.Errorf("%s allow_from = %v, want %v", channel, got, want)
	}
}

func hasNoteContaining(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
