package config

import (
	"reflect"
	"testing"
)

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
