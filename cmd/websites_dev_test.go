// websites_dev_test.go — focused correctness checks for the claims in the
// ChatGPT-prompted review: managed mode must never touch an unrelated
// directory (even when it happens to be the shell's $PWD and even when it's
// itself a git repo), and explicit-dir mode's generated state must be
// confined to .memcode/ — invisible to `git status` and never able to ride
// along in a `push` bundle (which only ever contains committed objects).
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/websites"
)

func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%q): %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func initGitRepoT(t *testing.T, dir string) {
	t.Helper()
	runGitT(t, dir, "init", "-q")
	runGitT(t, dir, "config", "user.email", "test@example.com")
	runGitT(t, dir, "config", "user.name", "Test")
}

// snapshotDirT returns a stable listing of every path + size under dir
// (excluding .git), for a byte-for-byte "nothing changed" comparison.
func snapshotDirT(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			return nil
		}
		fmt.Fprintf(&b, "%s %d\n", rel, info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestResolveDevWorkspace_ManagedModeNeverTouchesCWD covers claim A: `memcode
// websites dev <slug>` run from inside an UNRELATED git repo (as $PWD) must
// write nothing there — everything lands under a throwaway $HOME instead.
func TestResolveDevWorkspace_ManagedModeNeverTouchesCWD(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	unrelated := t.TempDir()
	initGitRepoT(t, unrelated)
	if err := os.WriteFile(filepath.Join(unrelated, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, unrelated, "add", "README.md")
	runGitT(t, unrelated, "commit", "-q", "-m", "init")

	beforeStatus := runGitT(t, unrelated, "status", "--short")
	beforeSnap := snapshotDirT(t, unrelated)

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(unrelated); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	// A real site repo for the fake server to hand back as a bundle.
	siteDir := t.TempDir()
	initGitRepoT(t, siteDir)
	if err := os.WriteFile(filepath.Join(siteDir, "MEMCODE.md"), []byte("doctrine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "vite.config.js"), []byte("export default {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, siteDir, "add", "-A")
	runGitT(t, siteDir, "commit", "-q", "-m", "init")
	bundlePath := filepath.Join(t.TempDir(), "site.bundle")
	runGitT(t, siteDir, "bundle", "create", bundlePath, "--all")

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/website":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"websites": []websites.Site{{ID: "site-x", Slug: "demo-site"}},
			})
		case r.URL.Path == "/api/website/site-x/bundle":
			_ = json.NewEncoder(w).Encode(map[string]string{"url": srv.URL + "/bundle-file"})
		case r.URL.Path == "/bundle-file":
			f, err := os.Open(bundlePath)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			_, _ = io.Copy(w, f)
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	t.Setenv(provider.EnvAPIToken, "test-token")
	t.Setenv("MEMCODE_WEB_APP_URL", srv.URL)

	c, err := websites.New()
	if err != nil {
		t.Fatal(err)
	}

	ws, meta, err := resolveDevWorkspace(context.Background(), c, []string{"demo-site"})
	if err != nil {
		t.Fatalf("resolveDevWorkspace: %v", err)
	}
	if meta.Slug != "demo-site" {
		t.Fatalf("meta %+v", meta)
	}
	wantSource := filepath.Join(fakeHome, ".memcode", "websites", "site-x", "source")
	if ws.SourceDir != wantSource {
		t.Fatalf("source dir = %q, want %q", ws.SourceDir, wantSource)
	}
	if _, err := os.Stat(filepath.Join(ws.SourceDir, "MEMCODE.md")); err != nil {
		t.Fatalf("site was not actually cloned under the managed workspace: %v", err)
	}

	afterStatus := runGitT(t, unrelated, "status", "--short")
	if beforeStatus != afterStatus {
		t.Fatalf("unrelated repo's git status changed:\nbefore: %q\nafter:  %q", beforeStatus, afterStatus)
	}
	afterSnap := snapshotDirT(t, unrelated)
	if beforeSnap != afterSnap {
		t.Fatalf("unrelated repo's directory contents changed:\nbefore:\n%s\nafter:\n%s", beforeSnap, afterSnap)
	}
}

// TestResolveDevWorkspace_ExplicitDirStaysOutOfGitAndBundle covers claim B:
// explicit-dir mode's generated state (merged public dir, override config)
// must be confined to .memcode/, invisible to `git status`, and absent from
// a `push`-style bundle. It also demonstrates that `dev` IMPROVES on
// `pull`/`push`'s current gap, where .memcode/website.json is left untracked
// with no .gitignore (pull/push don't call EnsureGitignore today — a
// pre-existing condition, not introduced by this change).
func TestResolveDevWorkspace_ExplicitDirStaysOutOfGitAndBundle(t *testing.T) {
	checkout := t.TempDir()
	initGitRepoT(t, checkout)
	write := func(rel, content string) {
		full := filepath.Join(checkout, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("MEMCODE.md", "doctrine\n")
	write("vite.config.js", "export default {}\n")
	write("public/favicon.svg", "<svg/>")
	runGitT(t, checkout, "add", "-A")
	runGitT(t, checkout, "commit", "-q", "-m", "init")

	// Simulate the state right after a real `memcode websites pull`.
	write(".memcode/website.json", `{"site_id":"site-y","slug":"demo-site"}`+"\n")

	beforeStatus := runGitT(t, checkout, "status", "--short")
	if strings.TrimSpace(beforeStatus) == "" {
		t.Fatal("expected the untracked .memcode/website.json to show up in git status BEFORE dev runs")
	}

	ws, meta, err := resolveDevWorkspace(context.Background(), nil, []string{checkout})
	if err != nil {
		t.Fatalf("resolveDevWorkspace: %v", err)
	}
	if meta.Slug != "demo-site" || ws.Managed {
		t.Fatalf("ws/meta = %+v %+v", ws, meta)
	}
	if ws.SourceDir != checkout {
		t.Fatalf("source dir = %q, want the checkout itself", ws.SourceDir)
	}

	// What `dev` writes beyond EnsureGitignore (already run inside
	// resolveDevWorkspace): the merged public dir + override config.
	mergedPublic, err := mergePublicDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(mergedPublic, "favicon.svg")); err != nil {
		t.Fatalf("real public/ entry not symlinked into the merged dir: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(mergedPublic, "assets")); err != nil {
		t.Fatalf("uploaded-asset symlink missing: %v", err)
	}
	// public/favicon.svg itself must be untouched — read, never written.
	if got, err := os.ReadFile(filepath.Join(checkout, "public", "favicon.svg")); err != nil || string(got) != "<svg/>" {
		t.Fatalf("real public/favicon.svg mutated: %v %q", err, got)
	}
	if _, err := writeViteDevOverride(ws, mergedPublic); err != nil {
		t.Fatal(err)
	}

	afterStatus := runGitT(t, checkout, "status", "--short")
	if strings.TrimSpace(afterStatus) != "" {
		t.Fatalf("git status not clean after dev ran (want EnsureGitignore to have hidden .memcode/ entirely):\n%s", afterStatus)
	}

	// Mirror `push`'s own mechanism and confirm none of the generated dev
	// state rides along — it was never committed, so a bundle can't carry it.
	bundlePath := filepath.Join(t.TempDir(), "out.bundle")
	runGitT(t, checkout, "bundle", "create", bundlePath, "--all")
	clonedElsewhere := filepath.Join(t.TempDir(), "clone")
	runGitT(t, "", "clone", "-q", bundlePath, clonedElsewhere)

	if _, err := os.Stat(filepath.Join(clonedElsewhere, ".memcode")); !os.IsNotExist(err) {
		t.Fatalf(".memcode leaked into the push bundle: stat err=%v", err)
	}
}

// TestMergePublicDir_UploadedAssetsWinAtTopLevel covers claim E: a real
// public/assets/ subtree in the project itself is entirely superseded by the
// uploaded-asset cache — documented, deterministic policy, matching how the
// live bucket's shared {slug}/assets/ GCS prefix already behaves (object-
// level overwrite between the build-output and asset-upload syncs).
func TestMergePublicDir_UploadedAssetsWinAtTopLevel(t *testing.T) {
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "public", "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "public", "assets", "site-authored.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "public", "robots.txt"), []byte("User-agent: *\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws, err := websites.ExplicitWorkspace(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.AssetsDir, "uploaded.pdf"), []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}

	merged, err := mergePublicDir(ws)
	if err != nil {
		t.Fatal(err)
	}

	// robots.txt: an ordinary public/ entry, symlinked through untouched.
	if _, err := os.Lstat(filepath.Join(merged, "robots.txt")); err != nil {
		t.Fatalf("robots.txt missing: %v", err)
	}
	// /assets resolves to the UPLOAD cache, not the site's own public/assets/.
	entries, err := os.ReadDir(filepath.Join(merged, "assets"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "uploaded.pdf" {
		t.Fatalf("merged /assets = %v, want only the uploaded cache's entries (site-authored.png must be shadowed)", names)
	}
}
