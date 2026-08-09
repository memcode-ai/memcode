package websites

import (
	"context"
	"encoding/json"
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
