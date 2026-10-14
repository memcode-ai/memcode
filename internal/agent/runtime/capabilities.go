package runtime

import (
	"encoding/json"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Deferred capabilities (see tools.Capabilities): typed tools left out of the
// default tool list and turned on for the rest of the session when needed.
// Three ways a capability turns on, all through enableCapability:
//
//   1. prefetchCapabilities — the request names it ("what websites do I
//      have?"): enabled before the model is called, no extra model turn.
//   2. the `capability` tool — the model reads the index and asks for one.
//   3. a direct call to the deferred tool — honored, never refused for not
//      having been announced first.
//
// Only the catalog's IDs can be enabled. Skills never enable tools.

// availableCaps returns the deferred capabilities this session could use:
// the same mode and account gating (allowTool) as any other tool.
func (s *Session) availableCaps() []tools.Capability {
	// A website worker builds one site; publishing, scheduling and the rest
	// stay with the session that started it.
	if s.adminMode || s.site != nil {
		return nil
	}
	var out []tools.Capability
	for _, c := range tools.Capabilities {
		if s.allowTool(c.Tool) {
			out = append(out, c)
		}
	}
	return out
}

// enableCapability turns a deferred tool on for the rest of the session.
// Monotonic: there is no disable. Reports whether it was newly enabled.
func (s *Session) enableCapability(id string) bool {
	if !tools.IsDeferred(id) || !s.allowTool(id) {
		return false
	}
	if s.enabledCaps == nil {
		s.enabledCaps = map[string]bool{}
	}
	if s.enabledCaps[id] {
		return false
	}
	s.enabledCaps[id] = true
	return true
}

// prefetchCapabilities enables every available capability the request names,
// before the model is called.
func (s *Session) prefetchCapabilities(text string) {
	words := skillWordSet(text)
	lower := strings.ToLower(text)
	for _, c := range s.availableCaps() {
		for _, t := range c.Triggers {
			if triggerMatches(t, words, lower) {
				s.enableCapability(c.Tool)
				break
			}
		}
	}
}

// enableCapabilitiesUsedIn turns on every deferred tool the history already
// called. Within a session a tool never disappears (enabling is monotonic);
// a RESUMED session starts with nothing enabled, and its history would
// otherwise carry calls to tools the request no longer declares, which some
// providers reject.
func (s *Session) enableCapabilitiesUsedIn(msgs []wire.Message) {
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == "tool_use" && tools.IsDeferred(b.Name) {
				s.enableCapability(b.Name)
			}
		}
	}
}

// triggerMatches: a single-word trigger matches a whole word; one containing
// a space or dot ("memcode website", "memcode.app") matches as a phrase.
func triggerMatches(trig string, words map[string]bool, lower string) bool {
	if strings.ContainsAny(trig, " .") {
		return strings.Contains(lower, trig)
	}
	return words[trig]
}

// capabilitiesIndex is the system-prompt index of deferred capabilities.
// Stable for the session (it lists every available capability, enabled or
// not), so turning one on never rewrites the cached prompt prefix.
func (s *Session) capabilitiesIndex() string {
	caps := s.availableCaps()
	if len(caps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("AVAILABLE CAPABILITIES — not loaded by default. Before acting on a request, check whether it needs one of these; if it does, call `capability` with its name first, and its tool is on from your next step for the rest of the session. If its tool is already in your tool list, just use it.\n")
	for _, c := range caps {
		b.WriteString("- " + c.Tool + ": " + c.Summary + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// capabilityTool handles the `capability` call.
func (s *Session) capabilityTool(input json.RawMessage) toolResult {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	id := strings.TrimSpace(in.Name)
	available := false
	for _, c := range s.availableCaps() {
		if c.Tool == id {
			available = true
		}
	}
	if !available {
		return errResult("no capability named " + id + " is available in this session")
	}
	if !s.enableCapability(id) {
		return textResult("`" + id + "` is already on — call it directly.")
	}
	s.toolLine(true, "Capability", id, "", false)
	return textResult("`" + id + "` is on. Call it directly from your next step.")
}
