package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	agentrt "github.com/memcode-ai/memcode/internal/agent/runtime"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// jobContext is the envelope the gateway persists (keyed by session id) for a
// spawned job to self-discover: the agent's composed supplemental context plus
// its extra skill roots. The JSON shape is the contract with cmd's loader.
type jobContext struct {
	Items      []agentrt.ContextItem `json:"items,omitempty"`
	SkillRoots []string              `json:"skill_roots,omitempty"`
	// Attachments are media spool IDs (bare <sha256>.<ext> filenames) riding this
	// task. IDs, never paths: the child resolves them strictly inside the gateway
	// media spool, so a corrupted or stale context file cannot point a job at
	// arbitrary local files.
	Attachments []string `json:"attachments,omitempty"`
	// Model is the agent's pinned model (agents.<id>.model); the child uses it
	// in place of its config default. Empty = automatic routing.
	Model string `json:"model,omitempty"`
	// Reasoning is the agent's pinned thinking effort ("off"|"medium"|"high");
	// empty = per-turn automatic.
	Reasoning string `json:"reasoning,omitempty"`
}

func (jc jobContext) empty() bool {
	return len(jc.Items) == 0 && len(jc.SkillRoots) == 0 && len(jc.Attachments) == 0 && jc.Model == "" && jc.Reasoning == ""
}

// jobContextFor composes everything a bound agent layers onto a run: its
// instructions and memory as generic ContextItems, and its own skills dir as an
// extra skill root. A zero envelope means the run is byte-for-byte a plain CLI run.
func jobContextFor(agentID string) jobContext {
	return jobContext{Items: agentIdentityContext(agentID), SkillRoots: agentSkillRoots(agentID)}
}

// agentIdentityContext composes a bound agent's supplemental context: its own
// instructions and memory (from ~/.memcode/agents/<id>), classified into generic
// ContextItems. User-global (~/.memcode) and project context are sourced by the
// coding engine itself, so they are deliberately NOT duplicated here — the engine
// stays the owner of those tiers. Returns nil when there is no agent or no
// material.
func agentIdentityContext(agentID string) []agentrt.ContextItem {
	if agentID == "" {
		return nil
	}
	home, err := gwconfig.AgentHome(agentID)
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
	add("SOUL.md", agentrt.KindInstruction) // the ecosystem-standard identity file (Hermes/OpenClaw use the same name)
	add("MEMCODE.md", agentrt.KindInstruction)
	add("memory.md", agentrt.KindMemory)
	return items
}

// agentSkillRoots returns the agent's own skills directory
// (~/.memcode/agents/<id>/skills) when it exists — an extra discovery root that
// ranks between repo-local and user-global skills, so a agent carries its own
// capabilities without touching the project or the user's global skill set.
func agentSkillRoots(agentID string) []string {
	if agentID == "" {
		return nil
	}
	home, err := gwconfig.AgentHome(agentID)
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, "skills")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil
	}
	return []string{dir}
}

// writeContext persists a session's composed job context so the spawned child can
// self-discover it by session id. With an empty envelope it removes any stale
// file, so a prior agent's context never leaks into a later run on the same
// session.
func writeContext(session string, jc jobContext) error {
	path, err := gwconfig.ContextPath(session)
	if err != nil {
		return err
	}
	if jc.empty() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(jc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
