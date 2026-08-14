package cmd

import (
	"encoding/json"
	"os"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// jobContext mirrors the envelope the gateway persists at gwconfig.ContextPath —
// the persona's supplemental context plus its extra skill roots. The JSON shape
// is the contract with internal/gateway/server.
type jobContext struct {
	Items      []runtime.ContextItem `json:"items,omitempty"`
	SkillRoots []string              `json:"skill_roots,omitempty"`
}

// loadJobContext reads the job context the gateway persisted for this session
// (persona context and skill roots composed above the engine). Returns a zero
// envelope when there is none — which is always the case for the interactive
// CLI, since only the gateway sets --session and writes this file, so the
// engine runs unchanged by default. A file in the pre-envelope shape (a bare
// ContextItem array from an older gateway) is still understood.
func loadJobContext(session string) jobContext {
	path, err := gwconfig.ContextPath(session)
	if err != nil {
		return jobContext{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return jobContext{}
	}
	var jc jobContext
	if json.Unmarshal(data, &jc) == nil {
		return jc
	}
	var items []runtime.ContextItem
	if json.Unmarshal(data, &items) == nil {
		return jobContext{Items: items}
	}
	return jobContext{}
}
