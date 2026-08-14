package importer

import "testing"

func TestProviderKeys(t *testing.T) {
	env := map[string]string{
		"OPENAI_API_KEY":     "sk-openai",
		"ANTHROPIC_API_KEY":  "sk-ant",
		"GEMINI_API_KEY":     "",   // present but empty → not migrated
		"TELEGRAM_BOT_TOKEN": "tg", // a channel token, not a provider key
		"RANDOM_THING":       "x",
	}
	got := ProviderKeys(env)
	if len(got) != 2 {
		t.Fatalf("expected 2 provider keys, got %d: %v", len(got), got)
	}
	if got["OPENAI_API_KEY"] != "sk-openai" || got["ANTHROPIC_API_KEY"] != "sk-ant" {
		t.Errorf("wrong values: %v", got)
	}
	if _, ok := got["GEMINI_API_KEY"]; ok {
		t.Error("an empty key should not be migrated")
	}
	if _, ok := got["TELEGRAM_BOT_TOKEN"]; ok {
		t.Error("a channel token is not a provider key")
	}
}
