package importer

import (
	"strings"
	"testing"
	"time"
)

func TestHermesSchedules(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	data := `[
	 {"id":"a1","name":"Daily briefing","prompt":"Summarize AI news","enabled":true,"state":"scheduled",
	  "schedule":{"kind":"cron","expr":"0 9 * * *"},"deliver":"telegram:-100123"},
	 {"id":"b2","name":"Poll","prompt":"Check the queue","enabled":false,"state":"paused",
	  "schedule":{"kind":"every","expr":"every 30m"},"deliver":"slack:C01"},
	 {"id":"c3","name":"Reminder","prompt":"Ping me","enabled":true,"state":"scheduled",
	  "schedule":{"kind":"at","expr":"` + future + `"},"deliver":"telegram:42"},
	 {"id":"d4","name":"Done","prompt":"Old","enabled":true,"state":"completed",
	  "schedule":{"kind":"at","expr":"2020-01-01T00:00"},"deliver":"telegram:42"},
	 {"id":"e5","name":"Scripted","prompt":"x","enabled":true,"state":"scheduled","script":"do.sh",
	  "schedule":{"kind":"cron","expr":"* * * * *"},"deliver":"telegram:42"}
	]`
	scheds, notes := HermesSchedules([]byte(data))
	if len(scheds) != 3 {
		t.Fatalf("want 3 migrated schedules, got %d (%+v)", len(scheds), scheds)
	}
	if s := scheds[0]; s.Name != "daily-briefing" || s.Cron != "0 9 * * *" || s.DeliverTo != "telegram:-100123" || s.Disabled {
		t.Errorf("cron job mapped wrong: %+v", s)
	}
	if s := scheds[1]; s.Every != "30m" || !s.Disabled {
		t.Errorf("paused interval job must carry over disabled with a bare duration: %+v", s)
	}
	if s := scheds[2]; s.At == "" {
		t.Errorf("future one-shot must carry its at time: %+v", s)
	}
	// The script job must be reported, never silently dropped.
	found := false
	for _, n := range notes {
		if strings.Contains(n, "scripted") || strings.Contains(n, "script") {
			found = true
		}
	}
	if !found {
		t.Errorf("script-payload job must produce a note, got %v", notes)
	}
}

func TestOpenClawSchedules(t *testing.T) {
	enabled := true
	_ = enabled
	data := `{"jobs":[
	 {"id":"j1","name":"Standup","message":"Summarize commits","channel":"telegram","to":"123",
	  "schedule":{"kind":"cron","expr":"0 9 * * 1-5"},"enabled":true},
	 {"id":"j2","name":"Cleaner","command":"rm -rf /tmp/x","channel":"telegram","to":"123",
	  "schedule":{"kind":"every","expr":"1h"},"enabled":true},
	 {"id":"j3","name":"NoRoute","message":"hello","channel":"","to":"",
	  "schedule":{"kind":"every","expr":"1h"},"enabled":true}
	]}`
	scheds, notes := OpenClawSchedules([]byte(data))
	if len(scheds) != 2 {
		t.Fatalf("want 2 migrated schedules, got %d (%+v)", len(scheds), scheds)
	}
	if s := scheds[0]; s.Cron != "0 9 * * 1-5" || s.DeliverTo != "telegram:123" || s.Disabled {
		t.Errorf("cron job mapped wrong: %+v", s)
	}
	if s := scheds[1]; !s.Disabled {
		t.Errorf("routeless job must import disabled: %+v", s)
	}
	// The command job must be reported, never silently dropped.
	found := false
	for _, n := range notes {
		if strings.Contains(n, "cleaner") {
			found = true
		}
	}
	if !found {
		t.Errorf("command-payload job must produce a note, got %v", notes)
	}
}

func TestToolPolicyMapping(t *testing.T) {
	// Hermes: toolsets allow-list + disabled_toolsets deny, real hermes names.
	allowH, disabled, notes := HermesToolPolicy([]byte("toolsets: [file, terminal, clarify]\nagent:\n  disabled_toolsets: [terminal, browser, honcho]\n"))
	if strings.Join(allowH, ",") != "files,shell,ask_user" {
		t.Errorf("hermes allow mapping = %v, want [files shell ask_user]", allowH)
	}
	if strings.Join(disabled, ",") != "shell,browser" {
		t.Errorf("hermes deny mapping = %v, want [shell browser]", disabled)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "honcho") {
		t.Errorf("unmappable entry must be noted, got %v", notes)
	}
	// Hermes "all" alias = unrestricted.
	allowH, _, _ = HermesToolPolicy([]byte("toolsets: [all]\n"))
	if len(allowH) != 0 {
		t.Errorf("hermes all must mean unrestricted, got %v", allowH)
	}
	// OpenClaw: real tool IDs and group: refs map; wildcards and per-sender
	// overrides become notes.
	allow, deny, notes := OpenClawToolPolicy([]byte(`{"tools":{"profile":"coding","allow":["read","web_fetch","group:fs","sessions_*"],"deny":["exec"],"toolsBySender":{"u1":{"deny":["bash"]}}}}`))
	if strings.Join(allow, ",") != "read_file,fetch,files" {
		t.Errorf("openclaw allow = %v, want [read_file fetch files]", allow)
	}
	if len(deny) != 1 || deny[0] != "bash" {
		t.Errorf("openclaw deny = %v, want [bash]", deny)
	}
	wantNotes := []string{"profile", "wildcard", "toolsBySender"}
	for _, w := range wantNotes {
		found := false
		for _, n := range notes {
			if strings.Contains(n, w) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a %s note, got %v", w, notes)
		}
	}
	_ = notes
}
