package config

import "testing"
import yaml "go.yaml.in/yaml/v4"

// A bare `personal:` key (null value) must load as an existing agent with zero
// knobs — the docs write agents this way in block style.
func TestNullAgentEntryLoads(t *testing.T) {
	var s Settings
	if err := yaml.Unmarshal([]byte("agents:\n  personal:\n  researcher:\n    model: m\n"), &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Agents["personal"]; !ok {
		t.Fatal("bare agent key must exist in the map")
	}
	if s.Agents["researcher"].Model != "m" {
		t.Fatal("sibling agent lost its model")
	}
}
