package runtime

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/store"
)

func githubSess(t *testing.T) *Session {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newSess(st, captureProviderNil{}, t.TempDir(), "allow-all", permissions.ModeAllowAll, io.Discard)
}

// The github tool validates its args before shelling out, so bad calls fail fast with a
// clear message (not an opaque gh error).
func TestGitHubToolValidation(t *testing.T) {
	s := githubSess(t)
	ctx := context.Background()
	cases := []struct {
		in   tools.GitHubInput
		want string
	}{
		{tools.GitHubInput{Action: "pr_view"}, "number"},
		{tools.GitHubInput{Action: "issue_view"}, "number"},
		{tools.GitHubInput{Action: "pr_create", Title: "t"}, "body"},
		{tools.GitHubInput{Action: "comment", Number: 1}, "body"},
		{tools.GitHubInput{Action: "bogus"}, "action must be"},
	}
	for _, c := range cases {
		b, _ := json.Marshal(c.in)
		r := s.githubTool(ctx, b)
		if !r.isError || !strings.Contains(r.text(), c.want) {
			t.Errorf("github(%+v): isErr=%v text=%q, want error containing %q", c.in, r.isError, r.text(), c.want)
		}
	}
}

// The github tool is advertised to the executive session.
func TestGitHubToolAdvertised(t *testing.T) {
	s := githubSess(t)
	if !hasTool(s.toolDefs(), tools.GitHub) {
		t.Fatal("github tool should be advertised in normal chat")
	}
}
