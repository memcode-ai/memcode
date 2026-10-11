package websites

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/provider"
)

// newTestClient points a Client at a fake www server with a stubbed git.
func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv(provider.EnvAPIToken, "memcode_testtoken")
	t.Setenv(envWebAppURL, srv.URL)
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func requireBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer memcode_testtoken" {
		t.Fatalf("missing/wrong bearer: %q", got)
	}
}

func TestNewRequiresToken(t *testing.T) {
	t.Setenv(provider.EnvAPIToken, "")
	if _, err := New(); err == nil || !strings.Contains(err.Error(), "memcode login") {
		t.Fatalf("want login nudge, got %v", err)
	}
}

func TestListParsesWrappedAndBare(t *testing.T) {
	sites := []Site{{ID: "s1", Name: "Portfolio", Slug: "portfolio", Status: "published", LiveURL: "https://portfolio.memcode.app"}}

	for name, body := range map[string]any{
		"wrapped": map[string]any{"websites": sites},
		"bare":    sites,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireBearer(t, r)
				if r.URL.Path != "/api/website" {
					t.Fatalf("path %s", r.URL.Path)
				}
				json.NewEncoder(w).Encode(body)
			}))
			got, err := c.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Slug != "portfolio" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestRenderList(t *testing.T) {
	out := RenderList([]Site{
		{Name: "Portfolio", Slug: "portfolio", Status: "published", LiveURL: "https://portfolio.memcode.app"},
		{Name: "Cafe", Slug: "cafe", Status: "draft"},
	})
	if !strings.Contains(out, "portfolio") || !strings.Contains(out, "https://portfolio.memcode.app") {
		t.Fatalf("render missing fields:\n%s", out)
	}
	if !strings.Contains(out, "draft") {
		t.Fatalf("render missing status:\n%s", out)
	}
}

func TestPullClonesAndWritesMeta(t *testing.T) {
	var bundleServed bool
	var signedURL string // set after the server starts; returned by the bundle endpoint
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/website":
			requireBearer(t, r)
			json.NewEncoder(w).Encode(map[string]any{"websites": []Site{{ID: "s1", Name: "Cafe", Slug: "cafe"}}})
		case r.URL.Path == "/api/website/s1/bundle" && r.Method == http.MethodGet:
			requireBearer(t, r)
			json.NewEncoder(w).Encode(map[string]string{"url": signedURL})
		case r.URL.Path == "/signed-bundle":
			// The signed URL must be fetched WITHOUT the bearer (signature is auth).
			if r.Header.Get("Authorization") != "" {
				t.Fatal("bearer sent to signed URL")
			}
			io.WriteString(w, "BUNDLEBYTES")
			bundleServed = true
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	signedURL = srv.URL + "/signed-bundle"

	dest := filepath.Join(t.TempDir(), "cafe")
	var cloned []string
	c.runGit = func(dir string, args ...string) error {
		cloned = args
		// simulate git clone creating the working dir
		return os.MkdirAll(dest, 0o755)
	}

	site, err := c.Pull(context.Background(), "cafe", dest)
	if err != nil {
		t.Fatal(err)
	}
	if site.ID != "s1" || !bundleServed {
		t.Fatalf("site %+v bundleServed=%v", site, bundleServed)
	}
	if len(cloned) < 3 || cloned[0] != "clone" || cloned[len(cloned)-1] != dest {
		t.Fatalf("git args %v", cloned)
	}
	meta, err := ReadMeta(dest)
	if err != nil || meta.SiteID != "s1" || meta.Slug != "cafe" {
		t.Fatalf("meta %+v err %v", meta, err)
	}
}

func TestPullRefusesExistingDir(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"websites": []Site{{ID: "s1", Slug: "cafe"}}})
	}))
	dest := t.TempDir() // exists
	if _, err := c.Pull(context.Background(), "cafe", dest); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want already-exists error, got %v", err)
	}
}

func TestPushBundlesAndPosts(t *testing.T) {
	dir := t.TempDir()
	if err := writeMeta(dir, Meta{SiteID: "s9", Slug: "cafe"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMCODE.md"), []byte("doctrine"), 0o644); err != nil {
		t.Fatal(err)
	}

	var postedBody []byte
	var postedType string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/website/s9/bundle" || r.Method != http.MethodPost {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		requireBearer(t, r)
		postedType = r.Header.Get("Content-Type")
		postedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		io.WriteString(w, "{}")
	}))
	c.runGit = func(gitDir string, args ...string) error {
		if gitDir != dir || args[0] != "bundle" || args[1] != "create" {
			t.Fatalf("git in %s: %v", gitDir, args)
		}
		return os.WriteFile(args[2], []byte("FAKEBUNDLE"), 0o644)
	}

	site, err := c.Push(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if site.Slug != "cafe" {
		t.Fatalf("site %+v", site)
	}
	if string(postedBody) != "FAKEBUNDLE" || postedType != "application/octet-stream" {
		t.Fatalf("posted %q type %q", postedBody, postedType)
	}
}

func TestPushRequiresDoctrine(t *testing.T) {
	dir := t.TempDir()
	if err := writeMeta(dir, Meta{SiteID: "s9", Slug: "cafe"}); err != nil {
		t.Fatal(err)
	}
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if _, err := c.Push(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "MEMCODE.md") {
		t.Fatalf("want doctrine error, got %v", err)
	}
}

func TestPublishAndUnpublish(t *testing.T) {
	var paths []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		if r.Method != http.MethodPost {
			t.Fatalf("method %s", r.Method)
		}
		paths = append(paths, r.URL.Path)
		json.NewEncoder(w).Encode(map[string]string{"url": "https://cafe.memcode.app"})
	}))
	url, err := c.Publish(context.Background(), "s9")
	if err != nil || url != "https://cafe.memcode.app" {
		t.Fatalf("publish url=%q err=%v", url, err)
	}
	if err := c.Unpublish(context.Background(), "s9"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/api/website/s9/publish", "/api/website/s9/unpublish"}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("paths %v", paths)
	}
}

func TestReadMetaMissing(t *testing.T) {
	if _, err := ReadMeta(t.TempDir()); err == nil || !strings.Contains(err.Error(), "pull") {
		t.Fatalf("want pull hint, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	var gotMethod, gotPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	if err := c.Delete(context.Background(), "s9"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/website/s9" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
}

func TestUpdate(t *testing.T) {
	cases := []struct {
		name     string
		namePtr  *string
		modelPtr *string
		wantErr  bool
		wantBody string
	}{
		{name: "both fields", namePtr: strPtr("New Name"), modelPtr: strPtr("claude-sonnet-5"), wantBody: `{"model":"claude-sonnet-5","name":"New Name"}`},
		{name: "name only", namePtr: strPtr("New Name"), wantBody: `{"name":"New Name"}`},
		{name: "neither set is an error", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantErr {
				c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					t.Fatal("must not hit the network when nothing is set")
				}))
				if _, err := c.Update(context.Background(), "s9", tc.namePtr, tc.modelPtr); err == nil {
					t.Fatal("want error")
				}
				return
			}
			var gotBody []byte
			var gotMethod, gotPath string
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireBearer(t, r)
				gotMethod, gotPath = r.Method, r.URL.Path
				gotBody, _ = io.ReadAll(r.Body)
				json.NewEncoder(w).Encode(map[string]any{
					"website": Site{ID: "s9", Slug: "cafe", Name: "New Name"},
				})
			}))
			site, err := c.Update(context.Background(), "s9", tc.namePtr, tc.modelPtr)
			if err != nil {
				t.Fatal(err)
			}
			if site.Slug != "cafe" {
				t.Fatalf("site %+v", site)
			}
			if gotMethod != http.MethodPatch || gotPath != "/api/website/s9" {
				t.Fatalf("got %s %s", gotMethod, gotPath)
			}
			if strings.TrimSpace(string(gotBody)) != tc.wantBody {
				t.Fatalf("body = %s, want %s", gotBody, tc.wantBody)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

func TestCreate(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(map[string]any{
			"website": Site{ID: "s1", Name: "Cafe", Slug: "cafe", Status: "draft"},
		})
	}))
	site, err := c.Create(context.Background(), "Cafe", "a one-page site for my coffee shop", "")
	if err != nil {
		t.Fatal(err)
	}
	if site.Slug != "cafe" {
		t.Fatalf("site %+v", site)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/website" {
		t.Fatalf("got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(string(gotBody), `"prompt"`) || !strings.Contains(string(gotBody), `"name"`) {
		t.Fatalf("body missing fields: %s", gotBody)
	}
}

func TestCreateClientAllowsEmptyNameAndPrompt(t *testing.T) {
	// The name-or-prompt rule lives in the callers (CLI, tool); the server names
	// an empty site "Untitled site". Lock that the client invents no rule of its own.
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"website": Site{ID: "s1", Slug: "untitled-site"}})
	}))
	if _, err := c.Create(context.Background(), "", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestBuildQueuesAndDecodesBuild(t *testing.T) {
	var gotBody map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/website/s1/build" || r.Method != http.MethodPost {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		json.NewEncoder(w).Encode(map[string]any{
			"build": Build{ID: "b1", Status: "pending", Mode: "build", Prompt: "add a contact form"},
		})
	}))
	build, err := c.Build(context.Background(), "s1", "add a contact form", "")
	if err != nil {
		t.Fatal(err)
	}
	if build.ID != "b1" || build.Mode != "build" {
		t.Fatalf("build %+v", build)
	}
	if gotBody["mode"] != "build" || gotBody["prompt"] != "add a contact form" {
		t.Fatalf("posted body %+v", gotBody)
	}
}

func TestBuildWithNoPromptIsARebuild(t *testing.T) {
	var gotMode string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		gotMode, _ = body["mode"].(string)
		json.NewEncoder(w).Encode(map[string]any{"build": Build{ID: "b2", Status: "pending", Mode: "rebuild"}})
	}))
	if _, err := c.Build(context.Background(), "s1", "", ""); err != nil {
		t.Fatal(err)
	}
	if gotMode != "rebuild" {
		t.Fatalf("mode = %q, want rebuild", gotMode)
	}
}

// Gating is detected by STATUS (403 on the build routes), never by wording —
// the fake's message is deliberately unlike the real server's.
func TestBuildGatedReturnsErrBuildsGated(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "reworded someday"})
	}))
	_, err := c.Build(context.Background(), "s1", "add a page", "")
	if !errors.Is(err, ErrBuildsGated) {
		t.Fatalf("want ErrBuildsGated, got %v", err)
	}
}

// A non-403 failure (e.g. the 402 credits gate) must surface as itself.
func TestBuildNonGateErrorPassesThrough(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		json.NewEncoder(w).Encode(map[string]string{"error": "Out of credits"})
	}))
	_, err := c.Build(context.Background(), "s1", "add a page", "")
	var apiErr *APIError
	if errors.Is(err, ErrBuildsGated) || !errors.As(err, &apiErr) || apiErr.Status != http.StatusPaymentRequired || err.Error() != "Out of credits" {
		t.Fatalf("want the 402 APIError, got %v", err)
	}
}

// Push rebuilds on the server, so it carries the same gate as Build.
func TestPushGatedReturnsErrBuildsGated(t *testing.T) {
	dir := t.TempDir()
	if err := writeMeta(dir, Meta{SiteID: "s9", Slug: "cafe"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMCODE.md"), []byte("doctrine"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "Website builds are in limited internal preview"})
	}))
	c.runGit = func(_ string, args ...string) error { return os.WriteFile(args[2], []byte("B"), 0o644) }
	if _, err := c.Push(context.Background(), dir); !errors.Is(err, ErrBuildsGated) {
		t.Fatalf("want ErrBuildsGated, got %v", err)
	}
}

// A never-built site has no repo bundle (404) — say so plainly.
func TestPullNeverBuiltSite(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/website" {
			json.NewEncoder(w).Encode(map[string]any{"websites": []Site{{ID: "s1", Slug: "cafe"}}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "No repository yet — build the site first"})
	}))
	dest := filepath.Join(t.TempDir(), "cafe")
	if _, err := c.Pull(context.Background(), "cafe", dest); err == nil || !strings.Contains(err.Error(), "never been built") {
		t.Fatalf("want never-built error, got %v", err)
	}
}

func TestEditRequiresEdits(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not hit the network with no edits")
	}))
	if _, err := c.Edit(context.Background(), "s1", nil, ""); err == nil {
		t.Fatal("want error")
	}
}

func TestEditPostsEditsAsEditMode(t *testing.T) {
	var gotBody map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		json.NewEncoder(w).Encode(map[string]any{"build": Build{ID: "b3", Status: "pending", Mode: "edit"}})
	}))
	build, err := c.Edit(context.Background(), "s1", []Edit{{Find: "Hello", Replace: "Hi"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if build.Mode != "edit" {
		t.Fatalf("build %+v", build)
	}
	if gotBody["mode"] != "edit" {
		t.Fatalf("mode = %v", gotBody["mode"])
	}
	edits, ok := gotBody["edits"].([]any)
	if !ok || len(edits) != 1 {
		t.Fatalf("edits = %v", gotBody["edits"])
	}
}

func TestListBuilds(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/website/s1/builds" || r.Method != http.MethodGet {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"builds": []Build{
				{ID: "b2", Status: "completed", Mode: "build"},
				{ID: "b1", Status: "completed", Mode: "build"},
			},
		})
	}))
	builds, err := c.ListBuilds(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != 2 || builds[0].ID != "b2" {
		t.Fatalf("builds %+v", builds)
	}
}

// GetBySlug merges the two routes as the real server shapes them: the detail
// route carries model/published_at but NOT has_preview (only the list route
// computes it). Regression: the merged result used to read preview=false.
func TestGetBySlugMergesListAndDetail(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/website":
			json.NewEncoder(w).Encode(map[string]any{"websites": []map[string]any{
				{"id": "s1", "slug": "cafe", "has_preview": true},
			}})
		case "/api/website/s1":
			json.NewEncoder(w).Encode(map[string]any{"website": map[string]any{
				"id": "s1", "slug": "cafe", "model": "claude-sonnet-5", "published_at": "2026-10-01T00:00:00Z",
			}})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	site, err := c.GetBySlug(context.Background(), "cafe")
	if err != nil {
		t.Fatal(err)
	}
	if site.Model != "claude-sonnet-5" || site.PublishedAt == "" || !site.HasPreview {
		t.Fatalf("site %+v", site)
	}
}

func TestPreview(t *testing.T) {
	var sawPreviewToken bool
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		switch {
		case r.URL.Path == "/api/website/s9" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{
				"website": Site{ID: "s9", Slug: "cafe", LiveURL: "https://cafe.memcode.app"},
			})
		case r.URL.Path == "/api/website/s9/preview-token" && r.Method == http.MethodPost:
			sawPreviewToken = true
			json.NewEncoder(w).Encode(map[string]any{"token": "tok123", "expiresInSeconds": 21600})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	url, err := c.Preview(context.Background(), "s9")
	if err != nil {
		t.Fatal(err)
	}
	if !sawPreviewToken {
		t.Fatal("never minted a preview token")
	}
	want := "https://preview--cafe.memcode.app/?__memcode_token=tok123"
	if url != want {
		t.Fatalf("got %q, want %q", url, want)
	}
}

func TestPreviewRequiresLiveURL(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"website": Site{ID: "s9", Slug: "cafe"}})
	}))
	if _, err := c.Preview(context.Background(), "s9"); err == nil {
		t.Fatal("want error for a site with no live URL yet")
	}
}

func TestAssetsListUploadDelete(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "brochure.pdf")
	if err := os.WriteFile(file, []byte("%PDF-fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	var uploadedBody []byte
	var uploadedContentType string
	var deletedPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		switch {
		case r.URL.Path == "/api/website/s9/assets" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{
				"assets": []Asset{{Name: "brochure.pdf", Size: 9, URL: "https://cafe.memcode.app/assets/brochure.pdf"}},
			})
		case r.URL.Path == "/api/website/s9/assets" && r.Method == http.MethodPost:
			uploadedContentType = r.Header.Get("Content-Type")
			uploadedBody, _ = io.ReadAll(r.Body)
			json.NewEncoder(w).Encode(Asset{Name: "brochure.pdf", Size: 9, URL: "https://cafe.memcode.app/assets/brochure.pdf"})
		case r.Method == http.MethodDelete:
			deletedPath = r.URL.Path
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))

	list, err := c.ListAssets(context.Background(), "s9")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "brochure.pdf" {
		t.Fatalf("list %+v", list)
	}

	asset, err := c.UploadAsset(context.Background(), "s9", file, "")
	if err != nil {
		t.Fatal(err)
	}
	if asset.Name != "brochure.pdf" {
		t.Fatalf("uploaded %+v", asset)
	}
	if !strings.HasPrefix(uploadedContentType, "multipart/form-data") {
		t.Fatalf("content-type %q", uploadedContentType)
	}
	if !strings.Contains(string(uploadedBody), "%PDF-fake") {
		t.Fatalf("multipart body missing file bytes: %q", uploadedBody)
	}

	if err := c.DeleteAsset(context.Background(), "s9", "brochure.pdf"); err != nil {
		t.Fatal(err)
	}
	if deletedPath != "/api/website/s9/assets/brochure.pdf" {
		t.Fatalf("deleted path %q", deletedPath)
	}
}
