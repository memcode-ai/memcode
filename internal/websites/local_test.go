package websites

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// fakeWebApp serves the template and records create + bundle uploads.
type fakeWebApp struct {
	mu       sync.Mutex
	template []byte
	created  []string
	bundles  int
	hosted   []byte // the last pushed repo bundle, served back like memcode.ai does
}

func (f *fakeWebApp) handler(srvURL *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/website/template":
			json.NewEncoder(w).Encode(map[string]string{"url": *srvURL + "/tpl.tar.gz"})
		case r.URL.Path == "/tpl.tar.gz":
			w.Write(f.template)
		case r.URL.Path == "/api/website" && r.Method == http.MethodPost:
			var body struct {
				Name   string `json:"name"`
				Prompt string `json:"prompt"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Prompt != "" {
				http.Error(w, "a push must not start a prompt build", 400)
				return
			}
			f.created = append(f.created, body.Name)
			json.NewEncoder(w).Encode(map[string]any{"website": map[string]string{"id": "s-new", "slug": "crumb-bakery", "name": body.Name}})
		case r.URL.Path == "/api/website/s-new/bundle" && r.Method == http.MethodGet:
			if f.hosted == nil {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"url": *srvURL + "/hosted.bundle"})
		case r.URL.Path == "/hosted.bundle":
			w.Write(f.hosted)
		case r.URL.Path == "/api/website/s-new/builds":
			json.NewEncoder(w).Encode(map[string]any{"builds": []any{}})
		case r.URL.Path == "/api/website/s-new/bundle" && r.Method == http.MethodPost:
			f.hosted, _ = io.ReadAll(r.Body)
			f.bundles++
			json.NewEncoder(w).Encode(map[string]any{"build": map[string]string{"id": "b1"}})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 500)
		}
	}
}

func localClient(t *testing.T, f *fakeWebApp) *Client {
	t.Setenv("HOME", t.TempDir())
	var url string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.handler(&url)(w, r) }))
	url = srv.URL
	return c
}

// A site created locally is a git repo of the starter in a managed folder,
// and nothing exists on memcode.ai.
func TestInitLocalCreatesNothingRemote(t *testing.T) {
	f := &fakeWebApp{template: tarGz(t, map[string]string{
		"package.json": `{"name":"site"}`, "src/App.jsx": "export default 1", ".gitignore": "node_modules\n",
	})}
	c := localClient(t, f)
	site, err := c.InitLocal(context.Background(), "Crumb Bakery")
	if err != nil {
		t.Fatal(err)
	}
	if site.Pushed() || len(f.created) != 0 {
		t.Fatal("init must not create anything on memcode.ai")
	}
	if !strings.HasSuffix(site.Source, filepath.Join("websites", "crumb-bakery", "source")) {
		t.Fatalf("source = %s", site.Source)
	}
	for _, p := range []string{"package.json", "src/App.jsx", ".git"} {
		if _, err := os.Stat(filepath.Join(site.Source, p)); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	gi, _ := os.ReadFile(filepath.Join(site.Source, ".gitignore"))
	if !strings.Contains(string(gi), ".memcode/") {
		t.Error(".memcode/ must be ignored so its marker never gets pushed")
	}
	if found, ok := FindLocal("crumb bakery"); !ok || found.Source != site.Source {
		t.Fatalf("FindLocal by name: %+v %v", found, ok)
	}
	if _, err := c.InitLocal(context.Background(), "Crumb Bakery"); err == nil {
		t.Fatal("a second local site with the same name must be refused")
	}
}

// The first push creates the site on memcode.ai (with no prompt build) and
// uploads the code, including edits not yet committed.
func TestFirstPushCreatesTheRemoteSite(t *testing.T) {
	f := &fakeWebApp{template: tarGz(t, map[string]string{"package.json": `{}`, "src/App.jsx": "1"})}
	c := localClient(t, f)
	site, err := c.InitLocal(context.Background(), "Crumb Bakery")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(site.Source, "src", "App.jsx"), []byte("2"), 0o644)

	pushed, err := c.Push(context.Background(), site.Source)
	if err != nil {
		t.Fatal(err)
	}
	if pushed.ID != "s-new" || len(f.created) != 1 || f.created[0] != "Crumb Bakery" || f.bundles != 1 {
		t.Fatalf("pushed=%+v created=%v bundles=%d", pushed, f.created, f.bundles)
	}
	m, _ := ReadMeta(site.Source)
	if m.SiteID != "s-new" || m.Slug != "crumb-bakery" {
		t.Fatalf("meta after push = %+v", m)
	}
	out, _ := exec.Command("git", "-C", site.Source, "status", "--porcelain").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the local edit should have been committed before the push: %s", out)
	}
	// A second push reuses the site.
	if _, err := c.Push(context.Background(), site.Source); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 {
		t.Fatal("only the first push creates the site")
	}
}

func TestExtractRefusesPathsOutsideTheFolder(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	os.WriteFile(archive, tarGz(t, map[string]string{"../../evil": "x"}), 0o644)
	dest := filepath.Join(dir, "dest")
	os.MkdirAll(dest, 0o755)
	if err := extractTarGz(archive, dest); err != nil {
		// Refused outright is fine too.
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
		t.Fatal("an archive path escaped the destination")
	}
}

// A zip carries the project's own files and nothing generated or
// machine-specific, whether or not the folder is a git repo.
func TestZipLeavesOutGeneratedFiles(t *testing.T) {
	for _, git := range []bool{true, false} {
		dir := t.TempDir()
		write := func(rel, body string) {
			p := filepath.Join(dir, rel)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(body), 0o644)
		}
		write("package.json", "{}")
		write("src/App.jsx", "1")
		write("node_modules/react/index.js", "x")
		write("dist/index.html", "x")
		write(".memcode/website.json", "{}")
		write(".gitignore", "node_modules\ndist\n")
		if git {
			exec.Command("git", "-C", dir, "init", "-q").Run()
			write("src/New.jsx", "untracked but not ignored")
		}
		out := filepath.Join(t.TempDir(), "site.zip")
		if _, err := Zip(dir, out); err != nil {
			t.Fatal(err)
		}
		zr, err := zip.OpenReader(out)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		zr.Close()
		got := strings.Join(names, ",")
		for _, bad := range []string{"node_modules", "dist/", ".memcode", ".git/"} {
			if strings.Contains(got, bad) {
				t.Errorf("git=%v: zip contains %s: %s", git, bad, got)
			}
		}
		for _, want := range []string{"package.json", "src/App.jsx", ".gitignore"} {
			if !strings.Contains(got, want) {
				t.Errorf("git=%v: zip is missing %s: %s", git, want, got)
			}
		}
		if git && !strings.Contains(got, "src/New.jsx") {
			t.Errorf("a new, not-ignored file belongs in the zip: %s", got)
		}
	}
}

// A build on memcode.ai moves the hosted copy past the local one. A push
// would replace the hosted repo, so it's refused; a clean local copy
// catches up instead.
func TestPushRefusesToDiscardHostedChanges(t *testing.T) {
	f := &fakeWebApp{template: tarGz(t, map[string]string{"package.json": `{}`, "src/App.jsx": "1"})}
	c := localClient(t, f)
	ctx := context.Background()
	site, err := c.InitLocal(ctx, "Crumb Bakery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Push(ctx, site.Source); err != nil {
		t.Fatal(err)
	}

	// The hosted builder commits on top of what was pushed.
	other := filepath.Join(t.TempDir(), "hosted")
	hosted := filepath.Join(t.TempDir(), "h.bundle")
	os.WriteFile(hosted, f.hosted, 0o644)
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=b", "-c", "user.email=b@x"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("", "clone", "-q", hosted, other)
	os.WriteFile(filepath.Join(other, "src", "App.jsx"), []byte("built on memcode.ai"), 0o644)
	git(other, "commit", "-qam", "hosted build")
	git(other, "bundle", "create", "-q", hosted, "--all")
	f.hosted, _ = os.ReadFile(hosted)

	// A clean copy catches up rather than overwriting.
	local, _ := FindLocal("Crumb Bakery")
	if moved, err := c.CatchUp(ctx, local); err != nil || !moved {
		t.Fatalf("catch up: moved=%v err=%v", moved, err)
	}
	if b, _ := os.ReadFile(filepath.Join(site.Source, "src", "App.jsx")); string(b) != "built on memcode.ai" {
		t.Fatalf("after catch-up App.jsx = %q", b)
	}

	// The hosted copy moves again, and this time the local copy has its own work.
	git(other, "pull", "-q", "--ff-only")
	os.WriteFile(filepath.Join(other, "src", "App.jsx"), []byte("second hosted build"), 0o644)
	git(other, "commit", "-qam", "another hosted build")
	git(other, "bundle", "create", "-q", hosted, "--all")
	f.hosted, _ = os.ReadFile(hosted)
	os.WriteFile(filepath.Join(site.Source, "src", "Local.jsx"), []byte("mine"), 0o644)
	before := f.bundles
	if _, err := c.Push(ctx, site.Source); !errors.Is(err, ErrBehind) {
		t.Fatalf("want ErrBehind, got %v", err)
	}
	if f.bundles != before {
		t.Fatal("a refused push must not upload")
	}
}
