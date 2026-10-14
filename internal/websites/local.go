// local.go — local-first sites, the way git works: create a site on this
// machine, work on it with a local dev server, and decide later whether to
// push it to memcode.ai. A site made here does not exist on memcode.ai (no
// address, no slot) until its first push creates it there.
//
// Local sites live in managed workspaces (~/.memcode/websites/<name>/source).
// The folder keeps its name after the first push, so a dev server running
// from it is never moved out from under; a site is found by scanning the
// workspaces' .memcode/website.json markers (remote sites pulled before this
// existed sit under their site id, and are found the same way).
package websites

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// LocalSite is a site with a managed working copy on this machine.
type LocalSite struct {
	Root   string // the managed workspace root (~/.memcode/websites/<key>)
	Source string // the site's repo working tree
	Meta   Meta
}

// Workspace is the managed {source, assets, runtime} layout `dev` runs from.
func (l LocalSite) Workspace() (Workspace, error) { return ManagedWorkspace(filepath.Base(l.Root)) }

// NeedsPush reports whether memcode.ai is behind this working copy: never
// pushed, uncommitted changes, or commits since the last push.
func (l LocalSite) NeedsPush() bool {
	if !l.Pushed() {
		return true
	}
	if out, err := exec.Command("git", "-C", l.Source, "status", "--porcelain").Output(); err != nil || len(strings.TrimSpace(string(out))) > 0 {
		return true
	}
	head, err := gitHead(l.Source)
	return err != nil || head != l.Meta.PushedHead
}

func gitHead(dir string) (string, error) { return gitRev(dir, "HEAD") }

// Pushed reports whether the site exists on memcode.ai.
func (l LocalSite) Pushed() bool { return l.Meta.SiteID != "" }

// Label is how the site is named to the user: its address once pushed, its
// local name before.
func (l LocalSite) Label() string {
	if l.Meta.Slug != "" {
		return l.Meta.Slug
	}
	return l.Meta.Name
}

func managedBase() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".memcode", "websites"), nil
}

// ListLocal returns every site with a managed working copy.
func ListLocal() ([]LocalSite, error) {
	base, err := managedBase()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []LocalSite
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(base, e.Name())
		src := filepath.Join(root, "source")
		m, err := ReadMeta(src)
		if err != nil {
			continue
		}
		out = append(out, LocalSite{Root: root, Source: src, Meta: m})
	}
	return out, nil
}

// FindLocal finds a managed working copy by local name, local id, slug, or
// site id.
func FindLocal(key string) (LocalSite, bool) {
	key = strings.TrimSpace(key)
	slug := localSlug(key)
	sites, _ := ListLocal()
	for _, s := range sites {
		m := s.Meta
		if key != "" && (key == m.SiteID || key == m.Slug || key == m.LocalID ||
			slug == localSlug(m.Name) || "local-"+slug == m.LocalID) {
			return s, true
		}
	}
	return LocalSite{}, false
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// localSlug turns a site name into a filesystem-safe key fragment.
func localSlug(name string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
	}
	return s
}

// InitLocal creates a site on this machine only: the starter template in a
// new managed workspace, as a git repository with one commit. Nothing is
// created on memcode.ai.
func (c *Client) InitLocal(ctx context.Context, name string) (LocalSite, error) {
	name = strings.TrimSpace(name)
	slug := localSlug(name)
	if slug == "" {
		return LocalSite{}, errors.New("give the site a name")
	}
	if existing, ok := FindLocal(name); ok {
		return LocalSite{}, fmt.Errorf("you already have a local site named %q at %s", existing.Meta.Name, existing.Source)
	}
	id := "local-" + slug
	ws, err := ManagedWorkspace(slug)
	if err != nil {
		return LocalSite{}, err
	}
	if ws.SourceExists() {
		return LocalSite{}, fmt.Errorf("%s already exists", ws.SourceDir)
	}

	raw, err := c.do(ctx, http.MethodGet, "/api/website/template", nil, "")
	if err != nil {
		return LocalSite{}, fmt.Errorf("getting the site template: %w", err)
	}
	var tpl struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil || tpl.URL == "" {
		return LocalSite{}, errors.New("the site template is unavailable")
	}
	tmp, err := os.CreateTemp("", "memcode-template-*.tar.gz")
	if err != nil {
		return LocalSite{}, err
	}
	defer os.Remove(tmp.Name())
	if err := c.download(ctx, tpl.URL, tmp); err != nil {
		tmp.Close()
		return LocalSite{}, err
	}
	tmp.Close()

	if err := os.MkdirAll(ws.SourceDir, 0o755); err != nil {
		return LocalSite{}, err
	}
	if err := extractTarGz(tmp.Name(), ws.SourceDir); err != nil {
		os.RemoveAll(ws.Root)
		return LocalSite{}, fmt.Errorf("unpacking the site template: %w", err)
	}
	if err := ensureIgnored(ws.SourceDir, ".memcode/"); err != nil {
		return LocalSite{}, err
	}
	m := Meta{LocalID: id, Name: name}
	if err := writeMeta(ws.SourceDir, m); err != nil {
		return LocalSite{}, err
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=memcode", "-c", "user.email=websites@memcode.ai", "commit", "-q", "-m", "Start from the memcode website template"},
	} {
		if err := c.runGit(ws.SourceDir, args...); err != nil {
			return LocalSite{}, err
		}
	}
	return LocalSite{Root: ws.Root, Source: ws.SourceDir, Meta: m}, nil
}

// commitManaged commits everything in a managed working copy, so a push
// carries what the user sees in the dev server. Managed copies are memcode's
// own; a checkout the user pulled themselves is left to their own git.
func (c *Client) commitManaged(dir, message string) error {
	if !isManaged(dir) {
		return nil
	}
	if err := c.runGit(dir, "add", "-A"); err != nil {
		return err
	}
	// Nothing staged → nothing to commit; not an error.
	if exec.Command("git", "-C", dir, "diff", "--cached", "--quiet").Run() == nil {
		return nil
	}
	return c.runGit(dir, "-c", "user.name=memcode", "-c", "user.email=websites@memcode.ai",
		"commit", "-q", "-m", message)
}

func isManaged(dir string) bool {
	base, err := managedBase()
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	return strings.HasPrefix(abs, base+string(os.PathSeparator))
}

// ensureIgnored adds a line to dir/.gitignore unless already present.
func ensureIgnored(dir, line string) error {
	p := filepath.Join(dir, ".gitignore")
	raw, _ := os.ReadFile(p)
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	prefix := ""
	if len(raw) > 0 && !strings.HasSuffix(string(raw), "\n") {
		prefix = "\n"
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(prefix + line + "\n")
	return err
}

// extractTarGz unpacks a .tar.gz into dir. Only regular files and
// directories, and every path must stay inside dir.
func extractTarGz(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(root, filepath.Clean("/"+h.Name))
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in archive: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, 64<<20)); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
		// Symlinks and anything else are skipped: the starter needs neither.
	}
}

// ErrBehind means memcode.ai has changes this working copy doesn't (made by
// a build on memcode.ai, or pushed from another copy). A push replaces the
// hosted repo wholesale, so pushing now would throw those changes away.
var ErrBehind = errors.New("memcode.ai has changes this copy doesn't have yet")

// fetchRemote fetches the hosted repo into the working copy and returns the
// hosted HEAD commit. ok is false when the site has no hosted repo yet.
func (c *Client) fetchRemote(ctx context.Context, dir, siteID string) (head string, ok bool, err error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/website/"+siteID+"/bundle", nil, "")
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	var signed struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &signed); err != nil || signed.URL == "" {
		return "", false, nil
	}
	tmp, err := os.CreateTemp("", "memcode-site-*.bundle")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(tmp.Name())
	if err := c.download(ctx, signed.URL, tmp); err != nil {
		tmp.Close()
		return "", false, err
	}
	tmp.Close()
	// No background maintenance: it would outlive the call, and these repos are tiny.
	if err := c.runGit(dir, "-c", "maintenance.auto=false", "-c", "gc.auto=0", "fetch", "-q", tmp.Name(), "HEAD"); err != nil {
		return "", false, err
	}
	head, err = gitRev(dir, "FETCH_HEAD")
	return head, err == nil, err
}

// checkNotBehind refuses a push that would discard hosted changes.
func (c *Client) checkNotBehind(ctx context.Context, dir, siteID string) error {
	remote, ok, err := c.fetchRemote(ctx, dir, siteID)
	if err != nil {
		return fmt.Errorf("checking memcode.ai for changes: %w", err)
	}
	if !ok || exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", remote, "HEAD").Run() == nil {
		return nil
	}
	return ErrBehind
}

// CatchUp brings a working copy up to date with memcode.ai when it has no
// changes of its own (a fast-forward). It reports whether anything changed;
// a copy with its own unpushed work is left alone.
func (c *Client) CatchUp(ctx context.Context, l LocalSite) (bool, error) {
	if !l.Pushed() || l.NeedsPush() {
		return false, nil
	}
	remote, ok, err := c.fetchRemote(ctx, l.Source, l.Meta.SiteID)
	if err != nil || !ok {
		return false, err
	}
	head, _ := gitHead(l.Source)
	if remote == head {
		return false, nil
	}
	if err := c.runGit(l.Source, "-c", "maintenance.auto=false", "-c", "gc.auto=0", "merge", "-q", "--ff-only", remote); err != nil {
		return false, nil // diverged: leave it to the user
	}
	l.Meta.PushedHead = remote
	return true, writeMeta(l.Source, l.Meta)
}

func gitRev(dir, rev string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", rev).Output()
	return strings.TrimSpace(string(out)), err
}

// MergeHosted merges memcode.ai's changes into a managed working copy that
// has its own: local work is committed, then the hosted history merged in.
// A conflict is aborted, leaving the copy as it was, and reported as
// ErrBehind.
func (c *Client) MergeHosted(ctx context.Context, l LocalSite) error {
	if !l.Pushed() || !isManaged(l.Source) {
		return ErrBehind
	}
	if err := c.commitManaged(l.Source, "Changes made locally with memcode"); err != nil {
		return err
	}
	remote, ok, err := c.fetchRemote(ctx, l.Source, l.Meta.SiteID)
	if err != nil || !ok {
		return err
	}
	if err := c.runGit(l.Source, "-c", "maintenance.auto=false", "-c", "gc.auto=0", "-c", "user.name=memcode", "-c", "user.email=websites@memcode.ai",
		"merge", "-q", "--no-edit", "-m", "Merge changes made on memcode.ai", remote); err != nil {
		_ = c.runGit(l.Source, "merge", "--abort")
		return fmt.Errorf("%w, and they conflict with the local changes", ErrBehind)
	}
	return nil
}

// Head is the working copy's current commit ("" when it has none).
func Head(dir string) string {
	h, _ := gitHead(dir)
	return h
}

// Checkpoint records a round of changes in a managed working copy as one
// commit, so each change has a diff the user can see and undo, and returns
// what changed since the commit `since` ("3 files changed, 120 insertions(+),
// 8 deletions(-)"), "" when nothing did. A checkout the user pulled
// themselves is left to their own git; its uncommitted changes are reported.
func (c *Client) Checkpoint(dir, since, message string) (string, error) {
	if isManaged(dir) {
		if err := c.commitManaged(dir, message); err != nil {
			return "", err
		}
		if since == "" {
			return "", nil
		}
		out, _ := exec.Command("git", "-C", dir, "diff", "--shortstat", since, "HEAD").Output()
		return strings.TrimSpace(string(out)), nil
	}
	out, _ := exec.Command("git", "-C", dir, "diff", "--shortstat", "HEAD").Output()
	return strings.TrimSpace(string(out)), nil
}

// ChangedFiles lists the files that differ between commit since and HEAD.
func ChangedFiles(dir, since string) []string {
	if since == "" {
		return nil
	}
	out, err := exec.Command("git", "-C", dir, "diff", "--name-only", since, "HEAD").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}
