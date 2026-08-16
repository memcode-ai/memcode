package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

func adminIn(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The executor round-trip: create an agent, bind a channel to it, allow a
// sender, add a schedule — then read it all back through the overview.
func TestAdminExecuteRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()

	if _, err := adminExecute(ctx, tools.GwAgent, adminIn(t, map[string]string{"action": "add", "name": "researcher"})); err != nil {
		t.Fatal(err)
	}
	// Pin the agent's model and reasoning; both land in gateway.yaml.
	if _, err := adminExecute(ctx, tools.GwAgent, adminIn(t, map[string]string{"action": "model", "name": "researcher", "model": "claude-opus-5"})); err != nil {
		t.Fatal(err)
	}
	if _, err := adminExecute(ctx, tools.GwAgent, adminIn(t, map[string]string{"action": "reasoning", "name": "researcher", "reasoning": "high"})); err != nil {
		t.Fatal(err)
	}
	if _, err := adminExecute(ctx, tools.GwAgent, adminIn(t, map[string]string{"action": "reasoning", "name": "researcher", "reasoning": "bogus"})); err == nil {
		t.Error("a bogus reasoning level must be rejected")
	}
	if cfg, _ := gwconfig.Load(); cfg.Agents["researcher"].Model != "claude-opus-5" || cfg.Agents["researcher"].Reasoning != "high" {
		t.Errorf("agent model/reasoning pins not persisted: %+v", cfg.Agents["researcher"])
	}
	if _, err := adminExecute(ctx, tools.GwChannel, adminIn(t, map[string]string{"channel": "telegram", "field": "agent", "value": "researcher"})); err != nil {
		t.Fatal(err)
	}
	if _, err := adminExecute(ctx, tools.GwChannel, adminIn(t, map[string]string{"channel": "telegram", "field": "allow_add", "value": "123456"})); err != nil {
		t.Fatal(err)
	}
	if _, err := adminExecute(ctx, tools.GwSchedule, adminIn(t, map[string]string{
		"action": "add", "name": "digest", "cron": "0 9 * * 1-5",
		"task": "summarize yesterday", "deliver_to": "telegram:123456",
	})); err != nil {
		t.Fatal(err)
	}

	settings, err := gwconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	ch := settings.Get("telegram")
	if ch.Agent != "researcher" || len(ch.AllowFrom) != 1 || ch.AllowFrom[0] != "123456" {
		t.Errorf("telegram channel = %+v", ch)
	}
	if len(settings.Schedules) != 1 || settings.Schedules[0].Cron != "0 9 * * 1-5" {
		t.Errorf("schedules = %+v", settings.Schedules)
	}

	out, err := adminExecute(ctx, tools.GwOverview, adminIn(t, map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"researcher", "123456", "digest", "0 9 * * 1-5"} {
		if !strings.Contains(out, want) {
			t.Errorf("overview missing %q:\n%s", want, out)
		}
	}

	// Guard rails: unknown channel, bound agent can't be removed, binding to
	// a missing agent fails.
	if _, err := adminExecute(ctx, tools.GwChannel, adminIn(t, map[string]string{"channel": "icq", "field": "allow_add", "value": "1"})); err == nil {
		t.Error("unknown channel accepted")
	}
	if _, err := adminExecute(ctx, tools.GwAgent, adminIn(t, map[string]string{"action": "remove", "name": "researcher"})); err == nil {
		t.Error("removed an agent still bound to a channel")
	}
	if _, err := adminExecute(ctx, tools.GwChannel, adminIn(t, map[string]string{"channel": "slack", "field": "agent", "value": "ghost"})); err == nil {
		t.Error("bound a channel to a missing agent")
	}
}
