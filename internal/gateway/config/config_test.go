package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDirAndPath(t *testing.T) {
	// XDG_CONFIG_HOME wins and both the gateway config and its operational state
	// resolve under the same global dir (never a repo).
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/tmp/xdg/memcode" {
		t.Errorf("Dir() = %q, want /tmp/xdg/memcode", dir)
	}
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "gateway.yaml"); p != want {
		t.Errorf("Path() = %q, want %q (inside Dir)", p, want)
	}
}

func TestResolveProject(t *testing.T) {
	real := t.TempDir()
	// A registered path reached through a symlink must resolve to the real dir —
	// the canonical root is the execution authority, not the config string.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	s := Settings{Projects: map[string]Project{
		"app":     {Path: link, Enabled: true},
		"off":     {Path: real, Enabled: false},
		"missing": {Path: filepath.Join(real, "nope"), Enabled: true},
	}}

	got, err := s.ResolveProject("app")
	if err != nil {
		t.Fatalf("ResolveProject(app): %v", err)
	}
	realResolved, _ := filepath.EvalSymlinks(real)
	if got != realResolved {
		t.Errorf("resolved root = %q, want canonical %q (symlink not resolved)", got, realResolved)
	}

	if _, err := s.ResolveProject("unregistered"); err == nil {
		t.Error("an unregistered id must be refused (no arbitrary root reaches execution)")
	}
	if _, err := s.ResolveProject("off"); err == nil {
		t.Error("a disabled project must be refused")
	}
	if _, err := s.ResolveProject("missing"); err == nil {
		t.Error("a non-existent path must be refused")
	}
}

func TestAllowed(t *testing.T) {
	s := Settings{Channels: map[string]Channel{
		"telegram": {AllowFrom: []string{"@tim", "123"}},
		"discord":  {AllowFrom: []string{"*"}},
		"slack":    {}, // configured but no one allowed
	}}

	cases := []struct {
		channel, principal string
		want               bool
	}{
		{"telegram", "@tim", true},
		{"telegram", "123", true},
		{"telegram", "@eve", false},
		{"discord", "anyone", true}, // wildcard
		{"slack", "@tim", false},    // empty allow-list = deny
		{"unknown", "@tim", false},  // unconfigured channel = deny
	}
	for _, c := range cases {
		if got := s.Allowed(c.channel, c.principal); got != c.want {
			t.Errorf("Allowed(%q,%q) = %v, want %v", c.channel, c.principal, got, c.want)
		}
	}

	// The global escape hatch allows everyone everywhere.
	open := Settings{AllowAll: true}
	if !open.Allowed("telegram", "@anybody") {
		t.Error("AllowAll should permit any principal")
	}
}

func TestAgentAutonomyFieldsAndValidation(t *testing.T) {
	// An ordinary agent stays valid and stays non-autonomous by default —
	// autonomy is never acquired implicitly.
	ordinary := Settings{Agents: map[string]Agent{"ordinary": {Model: "m"}}}
	if err := ordinary.Validate(); err != nil {
		t.Fatalf("ordinary agent must remain valid: %v", err)
	}
	if a := ordinary.Agents["ordinary"]; a.Autonomous || a.Unattended() || a.Objective != "" {
		t.Fatalf("ordinary agent defaulted to autonomy: %+v", a)
	}

	// Objective and Autonomous are independent: holding a goal is not
	// permission to pursue it unprompted.
	goalOnly := Agent{Objective: "find backend roles"}
	if goalOnly.Unattended() {
		t.Fatal("an objective alone must not make an agent unattended")
	}
	// ...and an agent may run unattended with no standing objective (scheduled
	// work under governance), which is the case a single overloaded switch
	// could not express.
	scheduled := Agent{Autonomous: true}
	if !scheduled.Unattended() {
		t.Fatal("autonomous with no objective must still be governed as unattended")
	}

	for _, br := range []string{"", BrowserEphemeral, BrowserExistingChrome} {
		s := Settings{Agents: map[string]Agent{"a": {Browser: br}}}
		if err := s.Validate(); err != nil {
			t.Fatalf("browser %q rejected: %v", br, err)
		}
	}
	bad := Settings{Agents: map[string]Agent{"a": {Browser: "safari"}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown browser backend accepted")
	}
}
func TestGetZeroValue(t *testing.T) {
	var s Settings // nil Channels map
	if got := s.Get("telegram"); !reflect.DeepEqual(got, Channel{}) {
		t.Errorf("Get on nil map = %+v, want zero Channel", got)
	}
}

// Pairing defaults per channel kind: chat channels offer codes to unknown DM
// senders; email never does unless explicitly opted in — the watched mailbox
// may be a personal inbox, and pairing replies would be an auto-responder.
func TestPairingEnabledDefaults(t *testing.T) {
	var s Settings
	if !s.PairingEnabled("telegram") {
		t.Error("telegram pairing should default on")
	}
	if s.PairingEnabled("email") {
		t.Error("email pairing should default OFF")
	}
	on, off := true, false
	s.Channels = map[string]Channel{"email": {Pairing: &on}, "telegram": {Pairing: &off}}
	if !s.PairingEnabled("email") {
		t.Error("explicit email pairing:true ignored")
	}
	if s.PairingEnabled("telegram") {
		t.Error("explicit telegram pairing:false ignored")
	}
}
