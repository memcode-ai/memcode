package tools

import "testing"

// Every advertised tool must belong to exactly one toolset, and every toolset
// member must be a real tool — the toolset registry is the public naming
// contract (docs/agents/tools), so drift on either side fails here.
func TestToolsetRegistryCoversAllTools(t *testing.T) {
	inSet := map[string]string{}
	for set, members := range toolsetDefs {
		for _, m := range members {
			if prev, dup := inSet[m]; dup {
				t.Errorf("tool %q is in two toolsets: %s and %s", m, prev, set)
			}
			inSet[m] = set
		}
	}
	advertised := map[string]bool{}
	for _, d := range Defs() {
		advertised[d.Name] = true
		if inSet[d.Name] == "" {
			t.Errorf("tool %q has no toolset — add it to toolsetDefs and the docs page", d.Name)
		}
	}
	for _, d := range BrowserDefs() {
		advertised[d.Name] = true
		if inSet[d.Name] != "browser" {
			t.Errorf("browser tool %q must be in the browser toolset", d.Name)
		}
	}
	// MCP defs are built dynamically (only when servers are connected), so
	// they're registry-only here.
	dynamic := map[string]bool{MCP: true, MCPResource: true, MCPPrompt: true, MCPCodeExec: true}
	for tool, set := range inSet {
		if !advertised[tool] && !dynamic[tool] {
			t.Errorf("toolset %s lists %q, which no def advertises", set, tool)
		}
	}
}

func TestPolicySemantics(t *testing.T) {
	// Allow-list by set, deny by individual tool: deny wins.
	p, unknown := NewPolicy([]string{"files", "web"}, []string{"edit_file"})
	if len(unknown) != 0 {
		t.Fatalf("unexpected unknown entries: %v", unknown)
	}
	if !p.Allows("read_file") || !p.Allows("web_search") {
		t.Error("allowed-set members must pass")
	}
	if p.Allows("bash") {
		t.Error("a tool outside the allow-list must be blocked")
	}
	if p.Allows("edit_file") {
		t.Error("deny must win over allow")
	}
	// Deny-only: everything else passes.
	p, _ = NewPolicy(nil, []string{"shell"})
	if p.Allows("bash") || p.Allows("script") {
		t.Error("denied toolset members must be blocked")
	}
	if !p.Allows("read_file") {
		t.Error("with no allow-list, undenied tools pass")
	}
	// Empty policy restricts nothing.
	p, _ = NewPolicy(nil, nil)
	if !p.Empty() || !p.Allows("bash") {
		t.Error("empty policy must allow everything")
	}
	// Unknown entries are reported, never silently dropped.
	_, unknown = NewPolicy([]string{"filez"}, []string{"nonsense"})
	if len(unknown) != 2 {
		t.Errorf("unknown entries must be reported, got %v", unknown)
	}
}
