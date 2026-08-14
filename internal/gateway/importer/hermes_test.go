package importer

import (
	"testing"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

func TestFromHermes(t *testing.T) {
	cfg := `
platforms:
  telegram:
    enabled: true
    allowed_users: [123, 456]
  discord:
    allowed_users: ["789"]
  slack:
    allowed_users: ["U1"]
  signal:
    account: "+15555550123"
`
	// Hermes ~/.hermes/.env uses the same variable names memcode does.
	env := map[string]string{
		"TELEGRAM_BOT_TOKEN": "tg-token",
		"DISCORD_BOT_TOKEN":  "dc-token",
		"SLACK_BOT_TOKEN":    "xoxb-1",
		"SLACK_APP_TOKEN":    "xapp-1",
	}

	res, err := FromHermes([]byte(cfg), env)
	if err != nil {
		t.Fatal(err)
	}

	wantSecrets := map[string]string{
		gwconfig.EnvTelegramToken: "tg-token",
		gwconfig.EnvDiscordToken:  "dc-token",
		gwconfig.EnvSlackBotToken: "xoxb-1",
		gwconfig.EnvSlackAppToken: "xapp-1",
	}
	for k, want := range wantSecrets {
		if got := res.Secrets[k]; got != want {
			t.Errorf("secret %s = %q, want %q", k, got, want)
		}
	}

	assertAllow(t, res.Settings, "telegram", []string{"123", "456"})
	assertAllow(t, res.Settings, "discord", []string{"789"})
	assertAllow(t, res.Settings, "slack", []string{"U1"})

	// Signal isn't a supported channel; it's noted, not imported as credentials.
	if _, ok := res.Secrets["SIGNAL"]; ok {
		t.Error("signal should not produce secrets")
	}
	if !hasNoteContaining(res.Notes, "signal") {
		t.Errorf("expected a note about signal, got %v", res.Notes)
	}
	if !res.Settings.Allowed("telegram", "123") {
		t.Error("imported telegram allow-list should permit 123")
	}
}

func TestFromHermesMissingSlackAppToken(t *testing.T) {
	cfg := "platforms:\n  slack:\n    allowed_users: [\"U1\"]\n"
	res, err := FromHermes([]byte(cfg), map[string]string{"SLACK_BOT_TOKEN": "xoxb-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasNoteContaining(res.Notes, "SLACK_APP_TOKEN") {
		t.Errorf("expected a note about the missing app token, got %v", res.Notes)
	}
}

func TestParseEnv(t *testing.T) {
	env := ParseEnv([]byte("# comment\nexport TELEGRAM_BOT_TOKEN=abc\nDISCORD_BOT_TOKEN=\"def\"\n\nBAD LINE\n"))
	if env["TELEGRAM_BOT_TOKEN"] != "abc" {
		t.Errorf("export-prefixed value = %q", env["TELEGRAM_BOT_TOKEN"])
	}
	if env["DISCORD_BOT_TOKEN"] != "def" {
		t.Errorf("quoted value not unwrapped: %q", env["DISCORD_BOT_TOKEN"])
	}
}
