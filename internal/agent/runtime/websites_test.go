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

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/provider"
)

// websitesSession: a test session pointed at a fake www server (the websites
// client's MEMCODE_WEB_APP_URL override, same env as the artifact client's).
func websitesSession(t *testing.T, handler http.Handler) *Session {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv(provider.EnvAPIToken, "memcode_testtoken")
	t.Setenv("MEMCODE_WEB_APP_URL", srv.URL)
	return newTodoSession(t)
}

func websitesCall(s *Session, in tools.WebsitesInput) toolResult {
	b, _ := json.Marshal(in)
	return s.websitesTool(context.Background(), b)
}

// fakeWebsitesServer mirrors the REAL www response shapes for one site
// "cafe"/"s1" — notably, has_preview appears only on the list route; the
// detail route (select('*')) never carries it.
func fakeWebsitesServer(t *testing.T) (*int, http.HandlerFunc) {
	hits := 0
	return &hits, func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch {
		case r.URL.Path == "/api/website" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"websites": []map[string]any{{"id": "s1", "name": "Cafe", "slug": "cafe", "has_preview": true}}})
		case r.URL.Path == "/api/website" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"website": map[string]any{"id": "s1", "name": "Cafe", "slug": "cafe"}})
		case r.URL.Path == "/api/website/s1" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"website": map[string]any{"id": "s1", "name": "Cafe", "slug": "cafe", "model": "claude-sonnet-5"}})
		case r.URL.Path == "/api/website/s1" && r.Method == http.MethodPatch:
			json.NewEncoder(w).Encode(map[string]any{"website": map[string]any{"id": "s1", "slug": "cafe", "name": "New Name"}})
		case r.URL.Path == "/api/website/s1" && r.Method == http.MethodDelete:
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		case r.URL.Path == "/api/website/s1/build" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"build": map[string]any{"id": "b1", "status": "pending", "mode": "build"}})
		case r.URL.Path == "/api/website/s1/builds" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"builds": []map[string]any{{"id": "b1", "status": "completed", "mode": "build"}}})
		case r.URL.Path == "/api/website/s1/publish" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]string{"url": "https://cafe.memcode.app"})
		case r.URL.Path == "/api/website/s1/unpublish" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		case r.URL.Path == "/api/website/s1/assets" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"assets": []map[string]any{{"name": "brochure.pdf", "size": 9}}})
		case r.URL.Path == "/api/website/s1/assets" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"name": "logo.png", "size": 4, "url": "https://cafe.memcode.app/assets/logo.png"})
		case r.URL.Path == "/api/website/s1/assets/brochure.pdf" && r.Method == http.MethodDelete:
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// gatedBuildServer answers /build with the internal-preview 403. The message
// is deliberately NOT the real server wording: detection must key off the
// status, not the text.
func gatedBuildServer(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/website" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"websites": []map[string]any{{"id": "s1", "slug": "cafe"}}})
		case r.URL.Path == "/api/website" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"website": map[string]any{"id": "s1", "name": "Cafe", "slug": "cafe"}})
		case r.URL.Path == "/api/website/s1/build":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "Not for you (yet)"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}
}

func allowAll(asked *int) func(context.Context, ApprovalRequest) ApprovalDecision {
	return func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		*asked++
		return ApprovalDecision{Allow: true}
	}
}

func TestWebsitesList(t *testing.T) {
	_, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	res := websitesCall(s, tools.WebsitesInput{Action: "list"})
	if res.isError || !strings.Contains(res.text(), "cafe") {
		t.Fatalf("list: %s (err=%v)", res.text(), res.isError)
	}
}

// Regression: get used to overwrite the list row with the detail row, which
// has no has_preview — so preview always read false.
func TestWebsitesGetKeepsHasPreviewFromList(t *testing.T) {
	_, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	res := websitesCall(s, tools.WebsitesInput{Action: "get", Slug: "cafe"})
	if res.isError {
		t.Fatalf("get failed: %s", res.text())
	}
	if !strings.Contains(res.text(), "claude-sonnet-5") {
		t.Fatalf("get missing detail-route field: %s", res.text())
	}
	if !strings.Contains(res.text(), "preview: true") {
		t.Fatalf("get lost the list route's has_preview: %s", res.text())
	}
}

// Bad arguments fail before any network call or approval prompt.
func TestWebsitesValidatesBeforeNetwork(t *testing.T) {
	hits, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	asked := 0
	s.approve = allowAll(&asked)
	for name, in := range map[string]tools.WebsitesInput{
		"get without slug":         {Action: "get"},
		"create without name":      {Action: "create"},
		"edit without edits":       {Action: "edit", Slug: "cafe"},
		"update without fields":    {Action: "update", Slug: "cafe"},
		"assets_delete no name":    {Action: "assets_delete", Slug: "cafe"},
		"assets_upload no path":    {Action: "assets_upload", Slug: "cafe"},
		"unknown action":           {Action: "explode", Slug: "cafe"},
		"assets_upload escapes":    {Action: "assets_upload", Slug: "cafe", AssetPath: "../outside.png"},
		"assets_upload not exists": {Action: "assets_upload", Slug: "cafe", AssetPath: "nope.png"},
	} {
		if res := websitesCall(s, in); !res.isError {
			t.Errorf("%s: want an error, got %s", name, res.text())
		}
	}
	if *hits != 0 || asked != 0 {
		t.Fatalf("validation failures must not hit the server (%d) or prompt (%d)", *hits, asked)
	}
}

func TestWebsitesCreateGatedInAskMode(t *testing.T) {
	_, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	var labels []string
	s.approve = func(_ context.Context, r ApprovalRequest) ApprovalDecision {
		labels = append(labels, r.Label)
		return ApprovalDecision{Allow: true}
	}
	if res := websitesCall(s, tools.WebsitesInput{Action: "create", Name: "Cafe"}); res.isError {
		t.Fatalf("create failed: %s", res.text())
	}
	// With a prompt, the one approval must name BOTH effects (the build spends credits).
	if res := websitesCall(s, tools.WebsitesInput{Action: "create", Name: "Cafe", Prompt: "a coffee site"}); res.isError {
		t.Fatalf("create+build failed: %s", res.text())
	}
	if len(labels) != 2 || labels[0] != "Create website" || labels[1] != "Create website and queue a build" {
		t.Fatalf("approval labels = %v", labels)
	}
}

func TestWebsitesCreateDeclinedNeverHitsServer(t *testing.T) {
	hits, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	s.approve = func(_ context.Context, _ ApprovalRequest) ApprovalDecision { return ApprovalDecision{Allow: false} }
	res := websitesCall(s, tools.WebsitesInput{Action: "create", Name: "Cafe"})
	if !res.isError || !strings.Contains(res.text(), "declined") {
		t.Fatalf("declined create must error with 'declined': %s", res.text())
	}
	if *hits != 0 {
		t.Fatal("a declined create must never reach the server")
	}
}

// A gated build is an account restriction: reported as an error that says not
// to retry, and it never promises a local fallback (there is none for a site
// that was never built).
func TestWebsitesBuildGatedByStatus(t *testing.T) {
	s := websitesSession(t, gatedBuildServer(t))
	s.mode = permissions.ModeAuto
	res := websitesCall(s, tools.WebsitesInput{Action: "build", Slug: "cafe", Prompt: "add a contact form"})
	if !res.isError || !strings.Contains(res.text(), "internal preview") || !strings.Contains(res.text(), "don't retry") {
		t.Fatalf("gated build: %s (err=%v)", res.text(), res.isError)
	}
	if strings.Contains(res.text(), "pull") {
		t.Fatalf("gated build must not suggest the pull fallback: %s", res.text())
	}
}

func TestWebsitesCreateWithGatedBuildReportsEmptySite(t *testing.T) {
	s := websitesSession(t, gatedBuildServer(t))
	s.mode = permissions.ModeAuto
	res := websitesCall(s, tools.WebsitesInput{Action: "create", Name: "Cafe", Prompt: "a coffee site"})
	if res.isError {
		t.Fatalf("create itself succeeded and must not be an error: %s", res.text())
	}
	if !strings.Contains(res.text(), "empty") || strings.Contains(res.text(), "pull") {
		t.Fatalf("create+gated build text: %s", res.text())
	}
}

// Dangerous actions prompt even in auto mode; Medium ones don't.
func TestWebsitesDangerousActionsPromptInAutoMode(t *testing.T) {
	_, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	s.mode = permissions.ModeAuto
	asked := 0
	s.approve = allowAll(&asked)

	if res := websitesCall(s, tools.WebsitesInput{Action: "update", Slug: "cafe", Name: "New Name"}); res.isError {
		t.Fatalf("update failed: %s", res.text())
	}
	if asked != 0 {
		t.Fatalf("Medium update must not prompt in auto mode, asked=%d", asked)
	}
	for _, in := range []tools.WebsitesInput{
		{Action: "publish", Slug: "cafe"},
		{Action: "unpublish", Slug: "cafe"},
		{Action: "delete", Slug: "cafe"},
		{Action: "assets_delete", Slug: "cafe", AssetName: "brochure.pdf"},
	} {
		before := asked
		if res := websitesCall(s, in); res.isError {
			t.Fatalf("%s failed: %s", in.Action, res.text())
		}
		if asked != before+1 {
			t.Errorf("%s must prompt in auto mode", in.Action)
		}
	}
}

// Remember is per action and Medium-only: remembering create skips the next
// create, but never pre-approves delete — and delete offers no remember at all.
func TestWebsitesRememberIsPerActionAndNeverCoversDangerous(t *testing.T) {
	_, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	var reqs []ApprovalRequest
	s.approve = func(_ context.Context, r ApprovalRequest) ApprovalDecision {
		reqs = append(reqs, r)
		return ApprovalDecision{Allow: true, Remember: true}
	}
	websitesCall(s, tools.WebsitesInput{Action: "create", Name: "Cafe"})
	websitesCall(s, tools.WebsitesInput{Action: "create", Name: "Cafe"})
	if len(reqs) != 1 {
		t.Fatalf("remembered create must skip the second prompt, prompts=%d", len(reqs))
	}
	websitesCall(s, tools.WebsitesInput{Action: "delete", Slug: "cafe"})
	websitesCall(s, tools.WebsitesInput{Action: "delete", Slug: "cafe"})
	if len(reqs) != 3 {
		t.Fatalf("delete must prompt every time despite Remember, prompts=%d", len(reqs))
	}
	if reqs[1].Editable {
		t.Fatal("a Dangerous action must not offer don't-ask-again")
	}

	got := loadWebsitesApproval(s.root)
	if !got["create"] || got["delete"] || len(got) != 1 {
		t.Fatalf("persisted approvals = %v, want only create", got)
	}
}

// A hand-edited approvals file can't smuggle in a Dangerous action.
func TestWebsitesApprovalFileIgnoresDangerousActions(t *testing.T) {
	root := t.TempDir()
	p := websitesApprovalsPath(root)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("delete\npublish\nbuild\nnonsense\n"), 0o644)
	got := loadWebsitesApproval(root)
	if !got["build"] || got["delete"] || got["publish"] || got["nonsense"] {
		t.Fatalf("loaded approvals = %v, want only build", got)
	}
}

// assets_upload must never publish hidden or credential-bearing files —
// including through a symlink with an innocent name.
func TestWebsitesAssetsUploadRefusesSecretsAndHidden(t *testing.T) {
	hits, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	asked := 0
	s.approve = allowAll(&asked)
	write := func(rel string) {
		p := filepath.Join(s.root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	write(".env")
	write("config/.secrets/token.txt")
	write("deploy/server.pem")
	write("credentials")
	write(".ssh/id_rsa")
	if err := os.Symlink(filepath.Join(s.root, ".ssh", "id_rsa"), filepath.Join(s.root, "logo.png")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(outside, []byte("private"), 0o644)
	os.Symlink(outside, filepath.Join(s.root, "banner.png"))
	for _, rel := range []string{".env", "config/.secrets/token.txt", "deploy/server.pem", "credentials", "logo.png", "banner.png"} {
		res := websitesCall(s, tools.WebsitesInput{Action: "assets_upload", Slug: "cafe", AssetPath: rel})
		if !res.isError || !(strings.Contains(res.text(), "refusing") || strings.Contains(res.text(), "escapes")) {
			t.Errorf("%s: must be refused, got %s", rel, res.text())
		}
	}
	if *hits != 0 || asked != 0 {
		t.Fatalf("refused uploads must not hit the server (%d) or prompt (%d)", *hits, asked)
	}

	write("public/brand.png")
	res := websitesCall(s, tools.WebsitesInput{Action: "assets_upload", Slug: "cafe", AssetPath: "public/brand.png"})
	if res.isError || asked != 1 {
		t.Fatalf("an ordinary file must upload after one prompt: %s (asked=%d)", res.text(), asked)
	}
}

func TestWebsitesAssetsListAndDelete(t *testing.T) {
	_, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	asked := 0
	s.approve = allowAll(&asked)
	res := websitesCall(s, tools.WebsitesInput{Action: "assets_list", Slug: "cafe"})
	if res.isError || !strings.Contains(res.text(), "brochure.pdf") {
		t.Fatalf("assets_list: %s (err=%v)", res.text(), res.isError)
	}
	if res := websitesCall(s, tools.WebsitesInput{Action: "assets_delete", Slug: "cafe", AssetName: "brochure.pdf"}); res.isError {
		t.Fatalf("assets_delete failed: %s", res.text())
	}
}

// Plan mode: read-only actions work (research), changes wait for execution.
func TestWebsitesPlanMode(t *testing.T) {
	_, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	enterPlanForTest(s, "")
	if res := websitesCall(s, tools.WebsitesInput{Action: "delete", Slug: "cafe"}); !res.isError || !strings.Contains(res.text(), "denied") {
		t.Fatalf("plan mode must deny delete: %s", res.text())
	}
	if res := websitesCall(s, tools.WebsitesInput{Action: "list"}); res.isError {
		t.Fatalf("plan mode must allow list: %s", res.text())
	}
}

// allowTool: invisible without a token or to read-only explorers.
func TestWebsitesAllowTool(t *testing.T) {
	_, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	if !s.allowTool(tools.Websites) {
		t.Fatal("logged-in executive session must see the websites tool")
	}
	s.readOnly = true
	if s.allowTool(tools.Websites) {
		t.Fatal("read-only explorers must not see the websites tool")
	}
	s.readOnly = false
	t.Setenv(provider.EnvAPIToken, "")
	if s.allowTool(tools.Websites) {
		t.Fatal("without a token the websites tool must be hidden")
	}
}

func TestWebsitesMissingToken(t *testing.T) {
	_, h := fakeWebsitesServer(t)
	s := websitesSession(t, h)
	t.Setenv(provider.EnvAPIToken, "")
	res := websitesCall(s, tools.WebsitesInput{Action: "list"})
	if !res.isError || !strings.Contains(res.text(), "memcode login") {
		t.Fatalf("missing token must point at `memcode login`: %s", res.text())
	}
}
