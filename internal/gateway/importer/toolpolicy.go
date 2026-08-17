// Tool-policy migration, mapped against each product's REAL registry (not
// guessed): Hermes toolset names per its reference/toolsets docs (toolsets:
// allow-list + agent.disabled_toolsets deny), OpenClaw tool IDs and group:
// refs per its gateway/config-tools docs (tools.allow/deny, deny wins,
// case-insensitive). Entries whose meaning has no memcode equivalent become
// notes — never silently dropped, never guessed.
package importer

import (
	"encoding/json"
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v4"

	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// hermesToolset maps a Hermes toolset name onto memcode toolsets/tools.
// Source of truth: hermes reference/toolsets — web, search, terminal, file,
// browser, vision, image_gen, skills, moa, tts, todo, memory, session_search,
// clarify, code_execution, delegation, cronjob, messaging, honcho,
// homeassistant, rl, plus composites debugging and safe.
var hermesToolset = map[string][]string{
	"web":            {"web"},
	"search":         {"web_search"},
	"terminal":       {"shell"},
	"file":           {"files"},
	"browser":        {"browser"},
	"skills":         {"skills"},
	"todo":           {"todo"},
	"memory":         {"memory"},
	"clarify":        {"ask_user"},
	"code_execution": {"shell"},
	"delegation":     {"delegation"},
	"debugging":      {"shell", "web", "files"}, // hermes composite
	"safe":           {"web"},                   // hermes composite (vision/image_gen have no memcode tools)
}

// openclawEntry maps an OpenClaw tool ID or group: ref onto memcode
// toolsets/tools. Source of truth: OpenClaw's tools + gateway/config-tools
// docs (exec, process, terminal, code_execution, read, write, edit,
// apply_patch, ask_user, web_search, web_fetch, browser, message, cron, ...;
// group:fs, group:runtime, ...).
var openclawEntry = map[string][]string{
	"exec": {"bash"}, "process": {"shell"}, "terminal": {"shell"}, "code_execution": {"shell"},
	"read": {"read_file"}, "write": {"edit_file"}, "edit": {"edit_file"}, "apply_patch": {"apply_patch"},
	"ask_user": {"ask_user"}, "web_search": {"web_search"}, "web_fetch": {"fetch"},
	"browser":   {"browser"},
	"subagents": {"delegation"}, "agents_list": {"delegation"}, "agents_wait": {"delegation"},
	"session_status": {"delegation"},
	"group:fs":       {"files"}, "group:runtime": {"shell"}, "group:web": {"web"},
	"group:session": {"delegation"}, "group:memory": {"memory"},
}

// mapEntries converts source entries via table, collecting unmappable ones.
func mapEntries(entries []string, table map[string][]string) (mapped, unmapped []string) {
	seen := map[string]bool{}
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			mapped = append(mapped, t)
		}
	}
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if targets, ok := table[e]; ok {
			for _, t := range targets {
				add(t)
			}
			continue
		}
		if tools.ValidPolicyEntry(e) { // already a memcode name
			add(e)
			continue
		}
		unmapped = append(unmapped, e)
	}
	return mapped, unmapped
}

// HermesToolPolicy reads a Hermes config.yaml's tool restrictions: the
// top-level toolsets: allow-list (its "all" alias means unrestricted) and
// agent.disabled_toolsets. Both use hermes toolset names.
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
	allow, un = mapEntries(allowSrc, hermesToolset)
	if len(un) > 0 {
		notes = append(notes, fmt.Sprintf("tools: Hermes toolsets %v have no memcode equivalent — the rest carried over; see memcode.ai/docs/agents/tools", un))
	}
	disabled, un = mapEntries(hc.Agent.DisabledToolsets, hermesToolset)
	if len(un) > 0 {
		notes = append(notes, fmt.Sprintf("tools: Hermes disabled_toolsets %v have no memcode equivalent — nothing to disable for them; see memcode.ai/docs/agents/tools", un))
	}
	return allow, disabled, notes
}

// OpenClawToolPolicy reads the global tools.profile/allow/deny of an
// openclaw.json. Wildcard patterns, unmappable IDs, profiles, and per-sender
// overrides are reported, not guessed.
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
	split := func(entries []string) (plain, wild []string) {
		for _, e := range entries {
			if strings.Contains(e, "*") {
				wild = append(wild, e)
			} else {
				plain = append(plain, e)
			}
		}
		return
	}
	if p := strings.ToLower(strings.TrimSpace(oc.Tools.Profile)); p != "" && p != "full" {
		notes = append(notes, fmt.Sprintf("tools: OpenClaw profile %q has no direct memcode equivalent — set the agent's toolsets to match (see memcode.ai/docs/agents/tools for the groups)", p))
	}
	plainAllow, wildAllow := split(oc.Tools.Allow)
	plainDeny, wildDeny := split(oc.Tools.Deny)
	if len(wildAllow)+len(wildDeny) > 0 {
		notes = append(notes, fmt.Sprintf("tools: OpenClaw wildcard patterns %v aren't supported — name toolsets or tools instead", append(wildAllow, wildDeny...)))
	}
	var un []string
	allow, un = mapEntries(plainAllow, openclawEntry)
	if len(un) > 0 {
		notes = append(notes, fmt.Sprintf("tools: OpenClaw allow entries %v have no memcode equivalent — the rest carried over; see memcode.ai/docs/agents/tools", un))
	}
	disabled, un = mapEntries(plainDeny, openclawEntry)
	if len(un) > 0 {
		notes = append(notes, fmt.Sprintf("tools: OpenClaw deny entries %v have no memcode equivalent — nothing to disable for them; see memcode.ai/docs/agents/tools", un))
	}
	if len(oc.Tools.ToolsBySender) > 0 && string(oc.Tools.ToolsBySender) != "null" {
		notes = append(notes, "tools: OpenClaw per-sender tool overrides (toolsBySender) have no memcode equivalent — memcode's tool policy is per agent; bind restricted senders' channels to a restricted agent instead")
	}
	return allow, disabled, notes
}
