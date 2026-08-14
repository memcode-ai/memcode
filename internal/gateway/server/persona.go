package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	agentrt "github.com/memcode-ai/memcode/internal/agent/runtime"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// personaContext composes a bound persona's supplemental context: its own
// instructions and memory (from ~/.memcode/agents/<id>), classified into generic
// ContextItems. User-global (~/.memcode) and project context are sourced by the
// coding engine itself, so they are deliberately NOT duplicated here — the engine
// stays the owner of those tiers. Returns nil when there is no persona or no
// material, in which case the run is byte-for-byte a plain CLI run.
func personaContext(agentID string) []agentrt.ContextItem {
	if agentID == "" {
		return nil
	}
	home, err := gwconfig.PersonaHome(agentID)
	if err != nil {
		return nil
	}
	var items []agentrt.ContextItem
	add := func(file, kind string) {
		b, err := os.ReadFile(filepath.Join(home, file))
		if err != nil {
			return
		}
		if txt := strings.TrimSpace(string(b)); txt != "" {
			items = append(items, agentrt.ContextItem{Kind: kind, Content: txt, Source: "agent:" + agentID})
		}
	}
	add("MEMCODE.md", agentrt.KindInstruction)
	add("memory.md", agentrt.KindMemory)
	return items
}

// writeContext persists a session's composed context so the spawned child can
// self-discover it by session id. With no items it removes any stale file, so a
// prior persona's context never leaks into a later run on the same session.
func writeContext(session string, items []agentrt.ContextItem) error {
	path, err := gwconfig.ContextPath(session)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
