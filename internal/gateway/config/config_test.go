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

func TestAgentKindCompatibilityAndValidation(t *testing.T) {
	legacy := Settings{Agents: map[string]Agent{"ordinary": {Model: "m"}}}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy empty kind must remain valid: %v", err)
	}
	if got := legacy.Agents["ordinary"].Kind; got != "" {
		t.Fatalf("legacy kind = %q, want empty", got)
	}

	personal := Settings{Agents: map[string]Agent{"executive": {Kind: "personal"}}}
	if err := personal.Validate(); err != nil {
		t.Fatalf("personal kind rejected: %v", err)
	}

	unknown := Settings{Agents: map[string]Agent{"bad": {Kind: "workflow"}}}
	if err := unknown.Validate(); err == nil {
		t.Fatal("unknown agent kind must be rejected")
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
