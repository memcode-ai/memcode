package config

import (
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

func TestGetZeroValue(t *testing.T) {
	var s Settings // nil Channels map
	if got := s.Get("telegram"); !reflect.DeepEqual(got, Channel{}) {
		t.Errorf("Get on nil map = %+v, want zero Channel", got)
	}
}
