// Tool-policy migration. The engine's policy compiler (internal/agent/tools)
// natively accepts OpenClaw tool IDs, Hermes toolset names, group: refs, and
// trailing-* wildcards as aliases for memcode's canonical names — so source
// entries carry over VERBATIM when they resolve, keeping the user's own
// vocabulary in their gateway.yaml. Entries that resolve to nothing become
// notes; profiles and per-sender overrides have no equivalent and are noted,
// never guessed.
package importer

import (
	"encoding/json"
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v4"

	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// carryEntries keeps the entries the policy compiler understands (normalized
// to lower case), reporting the rest.
func carryEntries(entries []string) (kept, unknown []string) {
	seen := map[string]bool{}
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		if tools.ValidPolicyEntry(e) {
			kept = append(kept, e)
		} else {
			unknown = append(unknown, e)
		}
	}
	return kept, unknown
}

// HermesToolPolicy reads a Hermes config.yaml's tool restrictions: the
// top-level toolsets: allow-list (its "all" alias means unrestricted) and
// agent.disabled_toolsets.
func HermesToolPolicy(configYAML []byte) (allow, disabled, notes []string) {
	var hc struct {
		Toolsets []string `yaml:"toolsets"`
		Agent    struct {
			DisabledToolsets []string `yaml:"disabled_toolsets"`
		} `yaml:"agent"`
	}
	if err := yaml.Unmarshal(configYAML, &hc); err != nil {
		return nil, nil, nil
	}
	allowSrc := hc.Toolsets
	for _, e := range allowSrc {
		if strings.EqualFold(strings.TrimSpace(e), "all") {
			allowSrc = nil // hermes "all" = unrestricted
			break
		}
	}
	var un []string
	allow, un = carryEntries(allowSrc)
	if len(un) > 0 {
		notes = append(notes, fmt.Sprintf("tools: Hermes toolsets %v have no memcode equivalent — the rest carried over; see memcode.ai/docs/agents/tools", un))
	}
	disabled, un = carryEntries(hc.Agent.DisabledToolsets)
	if len(un) > 0 {
		notes = append(notes, fmt.Sprintf("tools: Hermes disabled_toolsets %v have no memcode equivalent — nothing to disable for them; see memcode.ai/docs/agents/tools", un))
	}
	return allow, disabled, notes
}

// OpenClawToolPolicy reads the global tools.profile/allow/deny of an
// openclaw.json.
func OpenClawToolPolicy(data []byte) (allow, disabled, notes []string) {
	var oc struct {
		Tools struct {
			Profile       string          `json:"profile"`
			Allow         []string        `json:"allow"`
			Deny          []string        `json:"deny"`
			ToolsBySender json.RawMessage `json:"toolsBySender"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &oc); err != nil {
		return nil, nil, nil
	}
	if p := strings.ToLower(strings.TrimSpace(oc.Tools.Profile)); p != "" && p != "full" {
		notes = append(notes, fmt.Sprintf("tools: OpenClaw profile %q has no direct memcode equivalent — set the agent's toolsets to match (see memcode.ai/docs/agents/tools for the groups)", p))
	}
	var un []string
	allow, un = carryEntries(oc.Tools.Allow)
	if len(un) > 0 {
		notes = append(notes, fmt.Sprintf("tools: OpenClaw allow entries %v have no memcode equivalent — the rest carried over; see memcode.ai/docs/agents/tools", un))
	}
	disabled, un = carryEntries(oc.Tools.Deny)
	if len(un) > 0 {
		notes = append(notes, fmt.Sprintf("tools: OpenClaw deny entries %v have no memcode equivalent — nothing to disable for them; see memcode.ai/docs/agents/tools", un))
	}
	if len(oc.Tools.ToolsBySender) > 0 && string(oc.Tools.ToolsBySender) != "null" {
		notes = append(notes, "tools: OpenClaw per-sender tool overrides (toolsBySender) have no memcode equivalent — memcode's tool policy is per agent; bind restricted senders' channels to a restricted agent instead")
	}
	return allow, disabled, notes
}
