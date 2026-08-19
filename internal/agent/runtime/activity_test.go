package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolActivityLabel(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"bash", `{"command":"go test ./..."}`, "bash(go test ./...)"},
		{"read_file", `{"path":"internal/sync.go"}`, "read_file(internal/sync.go)"},
		{"edit_file", `{"file_path":"a.go","old_string":"x"}`, "edit_file(a.go)"},
		{"ripgrep", `{"pattern":"TODO"}`, "ripgrep(TODO)"},
		{"web_search", `{"query":"go fsync rename"}`, "web_search(go fsync rename)"},
		{"fetch", `{"url":"https://example.com"}`, "fetch(https://example.com)"},
		{"agent", `{"task":"review the diff"}`, "agent(review the diff)"},
		{"todo", `{"items":[]}`, "todo"},
		{"bash", `not json`, "bash"},
		{"bash", `{"command":""}`, "bash"},
	}
	for _, c := range cases {
		if got := toolActivityLabel(c.name, json.RawMessage(c.input)); got != c.want {
			t.Errorf("toolActivityLabel(%q, %s) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
	// Long args clip with an ellipsis and never exceed the budget wildly.
	long := strings.Repeat("x", 200)
	got := toolActivityLabel("bash", json.RawMessage(`{"command":"`+long+`"}`))
	if !strings.HasSuffix(got, "…)") || len(got) > 80 {
		t.Errorf("long arg not clipped: %q", got)
	}
}
