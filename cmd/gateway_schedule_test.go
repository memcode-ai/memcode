package cmd

import (
	"encoding/json"
	"testing"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// Run-now must snapshot the same selection a timed fire gets: conversation
// choice over channel/gateway defaults, the schedule's agent pin over both,
// and a project that violates channel policy falls to the channel's list.
func TestScheduleRunTarget(t *testing.T) {
	settings := gwconfig.Settings{
		DefaultProject: "main",
		Channels: map[string]gwconfig.Channel{
			"telegram": {Agent: "personal"},
			"discord":  {Agent: "personal", Projects: []string{"docs"}},
		},
		Projects: map[string]gwconfig.Project{
			"main": {Path: "/tmp/main", Enabled: true},
			"docs": {Path: "/tmp/docs", Enabled: true},
		},
	}

	// Defaults: channel agent + gateway default project.
	if a, p := scheduleRunTarget(settings, gwconfig.Schedule{}, "telegram", "", ""); a != "personal" || p != "main" {
		t.Errorf("defaults = (%q,%q), want (personal, main)", a, p)
	}
	// Conversation selection wins over defaults.
	if a, p := scheduleRunTarget(settings, gwconfig.Schedule{}, "telegram", "coder", "docs"); a != "coder" || p != "docs" {
		t.Errorf("conversation selection = (%q,%q), want (coder, docs)", a, p)
	}
	// The schedule's pinned agent wins over the conversation's.
	if a, _ := scheduleRunTarget(settings, gwconfig.Schedule{Agent: "researcher"}, "telegram", "coder", ""); a != "researcher" {
		t.Errorf("schedule agent pin = %q, want researcher", a)
	}
	// A project outside the channel's policy falls to the channel's list, never
	// through to the gateway default.
	if _, p := scheduleRunTarget(settings, gwconfig.Schedule{}, "discord", "", ""); p != "docs" {
		t.Errorf("policy fallback = %q, want docs", p)
	}
}

// The job-context envelope is a cross-package JSON contract with
// internal/gateway/server — this canonical payload must keep decoding into
// every field, so a rename on either side fails loudly here.
func TestJobContextEnvelopeContract(t *testing.T) {
	payload := `{
	 "items":[{"kind":"instruction","content":"be brief","source":"agent:x"}],
	 "skill_roots":["/tmp/skills"],
	 "attachments":["abc.png"],
	 "model":"claude-sonnet-5",
	 "reasoning":"high"
	}`
	var jc jobContext
	if err := json.Unmarshal([]byte(payload), &jc); err != nil {
		t.Fatal(err)
	}
	if len(jc.Items) != 1 || len(jc.SkillRoots) != 1 || len(jc.Attachments) != 1 ||
		jc.Model != "claude-sonnet-5" || jc.Reasoning != "high" {
		t.Errorf("envelope did not decode fully: %+v", jc)
	}
}
