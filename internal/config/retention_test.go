package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Project overrides user, user overrides the default; 0 is "forever" and a
// negative value reads as 0. Saving the retention keeps the user's model pin.
func TestSessionRetentionLayering(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if d, src := SessionRetention(nil); d != DefaultSessionRetentionDays || src != "default" {
		t.Fatalf("no settings: %d %s", d, src)
	}
	if err := SaveUserPin("sonnet", 1000); err != nil {
		t.Fatal(err)
	}
	seven := 7
	if err := SaveUserSessionRetention(&seven); err != nil {
		t.Fatal(err)
	}
	if d, src := SessionRetention(&Config{}); d != 7 || src != "user" {
		t.Fatalf("user setting: %d %s", d, src)
	}
	if p := loadUserPrefs(); p.PinnedModel != "sonnet" {
		t.Fatalf("saving retention dropped the model pin: %+v", p)
	}
	zero, neg := 0, -3
	if d, src := SessionRetention(&Config{SessionRetentionDays: &zero}); d != 0 || src != "project" {
		t.Fatalf("project 0: %d %s", d, src)
	}
	if d, _ := SessionRetention(&Config{SessionRetentionDays: &neg}); d != 0 {
		t.Fatalf("negative: %d", d)
	}
	if err := SaveUserSessionRetention(nil); err != nil {
		t.Fatal(err)
	}
	if d, src := SessionRetention(&Config{}); d != DefaultSessionRetentionDays || src != "default" {
		t.Fatalf("cleared: %d %s", d, src)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "memcode", "prefs.json")); err != nil {
		t.Fatal(err)
	}
}
