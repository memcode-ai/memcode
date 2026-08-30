package runtime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
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

func TestGitHubPRCreatePreflightBlocksDirtyWorktree(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-b", "feat/pr")
	if err := os.WriteFile(filepath.Join(root, "draft.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := githubSess(t)
	s.root = root
	in, _ := json.Marshal(tools.GitHubInput{Action: "pr_create", Title: "Draft PR", Body: "Body"})
	r := s.githubTool(context.Background(), in)
	if !r.isError {
		t.Fatal("pr_create should fail before gh when the worktree is dirty")
	}
	if !strings.Contains(r.text(), "worktree still has uncommitted changes") {
		t.Fatalf("dirty-worktree reason missing: %q", r.text())
	}
}

func TestGitHubPRCreatePreflightBlocksBranchWithNoCommitsAhead(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "README.md")
	git(t, root, "commit", "-m", "initial")
	git(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(t, root, "switch", "-c", "feat/noop")

	s := githubSess(t)
	s.root = root
	in, _ := json.Marshal(tools.GitHubInput{Action: "pr_create", Title: "Draft PR", Body: "Body"})
	r := s.githubTool(context.Background(), in)
	if !r.isError {
		t.Fatal("pr_create should fail before gh when the branch has no commits")
	}
	if !strings.Contains(r.text(), "no commits ahead of origin/main") {
		t.Fatalf("no-ahead reason missing: %q", r.text())
	}
}

func git(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
