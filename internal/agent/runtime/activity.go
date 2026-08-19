package runtime

import "encoding/json"

// SetToolNotify installs a callback invoked once per tool call with a short
// human label ("bash(go test ./...)", "read_file(internal/sync.go)"). It is a
// lightweight activity tap for frontends that surface what a detached agent is
// doing right now (the job heartbeat); unlike toolLine it fires for quiet
// tools too. Nil detaches. Set before the session runs; not for mid-turn swaps.
func (s *Session) SetToolNotify(fn func(label string)) { s.toolNotify = fn }

// toolActivityLabel maps a tool_use block to its display label: the tool name
// plus a best-effort argument extracted from the input JSON, clipped short.
func toolActivityLabel(name string, input json.RawMessage) string {
	var in map[string]any
	if err := json.Unmarshal(input, &in); err != nil {
		return name
	}
	// First matching key wins — ordered by how well each names the work.
	for _, k := range []string{"command", "path", "file_path", "pattern", "query", "url", "task"} {
		if v, ok := in[k].(string); ok && v != "" {
			return name + "(" + clip(v, 60) + ")"
		}
	}
	return name
}
