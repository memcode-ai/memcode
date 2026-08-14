package cmd

import (
	"encoding/json"
	"os"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// loadJobContext reads the supplemental context the gateway persisted for this
// session (persona/user context composed above the engine). Returns nil when
// there is none — which is always the case for the interactive CLI, since only
// the gateway sets --session and writes this file, so the engine runs with no
// supplemental context by default.
func loadJobContext(session string) []runtime.ContextItem {
	path, err := gwconfig.ContextPath(session)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var items []runtime.ContextItem
	if json.Unmarshal(data, &items) != nil {
		return nil
	}
	return items
}
