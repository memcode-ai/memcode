package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/artifacts"
	"github.com/memcode-ai/memcode/internal/provider"
)

// artifactSession: a test session + fake www server. Returns the session and a
// counter of requests that actually reached the server.
func artifactSession(t *testing.T) (*Session, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id": "art-1", "url": "https://memcode.ai/code/artifact/art-1",
			})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"artifacts": []map[string]string{{"id": "art-1", "title": "T", "url": "https://memcode.ai/code/artifact/art-1"}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "art-1", "url": "https://memcode.ai/code/artifact/art-1"})
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv(provider.EnvAPIToken, "memcode_testtoken")
	t.Setenv("MEMCODE_WEB_APP_URL", srv.URL)
	return newTodoSession(t), &hits
}

func artifactCall(s *Session, in tools.ArtifactInput) toolResult {
	b, _ := json.Marshal(in)
	return s.artifactTool(context.Background(), b)
}

func writeArtifactFile(t *testing.T, s *Session, name, content string) string {
	t.Helper()
	p := filepath.Join(s.root, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

// Publish: gated in ask mode, URL + id land in the result so the model can echo
// and later update them.
func TestArtifactPublish(t *testing.T) {
	s, _ := artifactSession(t)
	asked := 0
	s.approve = func(_ context.Context, r ApprovalRequest) ApprovalDecision {
		asked++
		if r.Label != "Publish artifact" {
			t.Errorf("unexpected approval label %q", r.Label)
		}
		return ApprovalDecision{Allow: true}
	}
	path := writeArtifactFile(t, s, "page.html", "<h1>hi</h1>")
	res := artifactCall(s, tools.ArtifactInput{Action: "publish", Path: path, Title: "My page"})
	if res.isError {
		t.Fatalf("publish failed: %s", res.text())
	}
	if asked != 1 {
		t.Fatalf("publish must be gated in ask mode, asked=%d", asked)
	}
	out := res.text()
	if !strings.Contains(out, "/code/artifact/art-1") || !strings.Contains(out, "art-1") {
		t.Fatalf("result must carry URL and id:\n%s", out)
	}
}

// The gate declines and remembers correctly.
func TestArtifactGate(t *testing.T) {
	s, hits := artifactSession(t)
	path := writeArtifactFile(t, s, "page.html", "<p>x</p>")

	s.approve = func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: false}
	}
	res := artifactCall(s, tools.ArtifactInput{Action: "publish", Path: path, Title: "t"})
	if !res.isError || !strings.Contains(res.text(), "declined") {
		t.Fatalf("declined publish must error with 'declined': %s", res.text())
	}
	if *hits != 0 {
		t.Fatal("a declined publish must never reach the server")
	}

	// Approve with remember: the second publish skips the prompt, and the
	// approval file persists.
	asked := 0
	s.approve = func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		asked++
		return ApprovalDecision{Allow: true, Remember: true}
	}
	if res := artifactCall(s, tools.ArtifactInput{Action: "publish", Path: path, Title: "t"}); res.isError {
		t.Fatalf("approved publish failed: %s", res.text())
	}
	if res := artifactCall(s, tools.ArtifactInput{Action: "publish", Path: path, Title: "t"}); res.isError {
		t.Fatalf("remembered publish failed: %s", res.text())
	}
	if asked != 1 {
		t.Fatalf("remember must skip the second prompt, asked=%d", asked)
	}
	if !loadArtifactApproval(s.root) {
		t.Fatal("remember must persist to .memcode/artifact-approvals")
	}
}

// Guardrails: path escapes, oversize files, and missing auth fail fast and never
// hit the network.
func TestArtifactGuardrails(t *testing.T) {
	s, hits := artifactSession(t)
	s.approve = func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true}
	}

	res := artifactCall(s, tools.ArtifactInput{Action: "publish", Path: "../evil.html", Title: "t"})
	if !res.isError {
		t.Fatal("path escape must be rejected")
	}

	big := writeArtifactFile(t, s, "big.html", strings.Repeat("x", artifacts.MaxHTMLBytes+1))
	res = artifactCall(s, tools.ArtifactInput{Action: "publish", Path: big, Title: "t"})
	if !res.isError || !strings.Contains(res.text(), "1.5MB") {
		t.Fatalf("oversize must fail fast with the limit named: %s", res.text())
	}
	if *hits != 0 {
		t.Fatal("guardrail failures must never reach the server")
	}

	t.Setenv(provider.EnvAPIToken, "")
	res = artifactCall(s, tools.ArtifactInput{Action: "list"})
	if !res.isError || !strings.Contains(res.text(), "memcode login") {
		t.Fatalf("missing token must point at `memcode login`: %s", res.text())
	}
}

// Plan mode: list works (research), publish waits for execution.
func TestArtifactPlanMode(t *testing.T) {
	s, _ := artifactSession(t)
	enterPlanForTest(s, "")
	path := writeArtifactFile(t, s, "page.html", "<p>x</p>")

	res := artifactCall(s, tools.ArtifactInput{Action: "publish", Path: path, Title: "t"})
	if !res.isError || !strings.Contains(res.text(), "denied") {
		t.Fatalf("plan mode must deny publish: %s", res.text())
	}
	if res := artifactCall(s, tools.ArtifactInput{Action: "list"}); res.isError {
		t.Fatalf("plan mode must allow list: %s", res.text())
	}
}

// allowTool: invisible without a token or to read-only explorers.
func TestArtifactAllowTool(t *testing.T) {
	s, _ := artifactSession(t)
	if !s.allowTool(tools.Artifact) {
		t.Fatal("logged-in executive session must see the artifact tool")
	}
	s.readOnly = true
	if s.allowTool(tools.Artifact) {
		t.Fatal("read-only explorers must not see the artifact tool")
	}
	s.readOnly = false
	t.Setenv(provider.EnvAPIToken, "")
	if s.allowTool(tools.Artifact) {
		t.Fatal("without a token the artifact tool must be hidden")
	}
}
