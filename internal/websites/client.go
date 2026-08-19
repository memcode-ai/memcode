// Package websites is the CLI's client for memcode.ai's Websites feature —
// AI-built static sites. list/pull/push/publish map onto the www /api/website
// endpoints, authenticated with the same bearer token the login flow mints
// (MEMCODE_API_TOKEN). Like the artifacts client, this talks to the WEB APP,
// not the inference gateway.
//
// The local workflow is deliberately git-native: `pull` clones the site's
// repo bundle into ./<slug> (which carries MEMCODE.md + .memcode/, so plain
// `memcode` in that directory IS website mode), `push` bundles the repo back
// up and queues a server-side rebuild of the draft preview.
package websites

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/provider"
)

const (
	// defaultWebAppURL deliberately duplicates the artifacts client's constant —
	// internal packages must not import cmd. Same env override, same default.
	defaultWebAppURL = "https://memcode.ai"
	envWebAppURL     = "MEMCODE_WEB_APP_URL"
	requestTimeout   = 60 * time.Second

	// metaRelPath is written into a pulled repo so push/publish can resolve the
	// site without a lookup. Committed on purpose: a re-clone keeps it.
	metaRelPath = ".memcode/website.json"
)

// Site is one website, as the www API reports it.
type Site struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Slug    string `json:"slug"`
	Status  string `json:"status"`
	LiveURL string `json:"url,omitempty"` // www list route emits `url` (siteUrl(slug))
}

// Meta is the .memcode/website.json marker written at pull time.
type Meta struct {
	SiteID string `json:"site_id"`
	Slug   string `json:"slug"`
}

// Client is an authenticated memcode.ai websites client.
type Client struct {
	base  string
	token string
	http  *http.Client

	// runGit is swappable in tests; defaults to exec'ing the real git.
	runGit func(dir string, args ...string) error
}

// New builds a client from the environment. Errors when the user has no token —
// the caller surfaces "run `memcode login`".
func New() (*Client, error) {
	token := strings.TrimSpace(os.Getenv(provider.EnvAPIToken))
	if token == "" {
		return nil, fmt.Errorf("not logged in — run `memcode login` first")
	}
	base := strings.TrimSpace(os.Getenv(envWebAppURL))
	if base == "" {
		base = defaultWebAppURL
	}
	return &Client{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		http:   &http.Client{Timeout: requestTimeout},
		runGit: execGit,
	}, nil
}

func execGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// List returns the org's sites.
func (c *Client) List(ctx context.Context) ([]Site, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/website", nil, "")
	if err != nil {
		return nil, err
	}
	// Tolerant decode: {websites:[...]} (the www convention) or a bare array.
	var wrapped struct {
		Websites []Site `json:"websites"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Websites != nil {
		return wrapped.Websites, nil
	}
	var arr []Site
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	return nil, fmt.Errorf("decoding site list: unexpected response shape")
}

// Resolve finds a site by slug (exact match).
func (c *Client) Resolve(ctx context.Context, slug string) (Site, error) {
	sites, err := c.List(ctx)
	if err != nil {
		return Site{}, err
	}
	for _, s := range sites {
		if s.Slug == slug {
			return s, nil
		}
	}
	return Site{}, fmt.Errorf("no site with slug %q — run `memcode websites list`", slug)
}

// Pull clones the site's repo bundle into destDir (which must not exist) and
// writes the .memcode/website.json marker.
func (c *Client) Pull(ctx context.Context, slug, destDir string) (Site, error) {
	site, err := c.Resolve(ctx, slug)
	if err != nil {
		return Site{}, err
	}
	if _, err := os.Stat(destDir); err == nil {
		return Site{}, fmt.Errorf("%s already exists — remove it or pull elsewhere", destDir)
	}

	raw, err := c.do(ctx, http.MethodGet, "/api/website/"+site.ID+"/bundle", nil, "")
	if err != nil {
		return Site{}, err
	}
	var signed struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &signed); err != nil || signed.URL == "" {
		return Site{}, fmt.Errorf("no repo bundle available for %q (has it been built yet?)", slug)
	}

	tmp, err := os.CreateTemp("", "memcode-site-*.bundle")
	if err != nil {
		return Site{}, err
	}
	defer os.Remove(tmp.Name())
	if err := c.download(ctx, signed.URL, tmp); err != nil {
		return Site{}, err
	}
	if err := tmp.Close(); err != nil {
		return Site{}, err
	}

	if err := c.runGit("", "clone", tmp.Name(), destDir); err != nil {
		return Site{}, err
	}
	if err := writeMeta(destDir, Meta{SiteID: site.ID, Slug: site.Slug}); err != nil {
		return Site{}, err
	}
	return site, nil
}

// Push bundles the repo at dir and uploads it; the server rebuilds the draft
// preview from the pushed source.
func (c *Client) Push(ctx context.Context, dir string) (Site, error) {
	meta, err := ReadMeta(dir)
	if err != nil {
		return Site{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, "MEMCODE.md")); err != nil {
		return Site{}, fmt.Errorf("%s doesn't look like a memcode website repo (no MEMCODE.md)", dir)
	}

	tmp, err := os.CreateTemp("", "memcode-site-*.bundle")
	if err != nil {
		return Site{}, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	if err := c.runGit(dir, "bundle", "create", tmpPath, "--all"); err != nil {
		return Site{}, err
	}
	bundle, err := os.ReadFile(tmpPath)
	if err != nil {
		return Site{}, err
	}

	if _, err := c.do(ctx, http.MethodPost, "/api/website/"+meta.SiteID+"/bundle",
		bytes.NewReader(bundle), "application/octet-stream"); err != nil {
		return Site{}, err
	}
	return Site{ID: meta.SiteID, Slug: meta.Slug}, nil
}

// Publish promotes the site's draft to its live URL. Returns the live URL when
// the server reports one.
func (c *Client) Publish(ctx context.Context, siteID string) (string, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/website/"+siteID+"/publish", nil, "")
	if err != nil {
		return "", err
	}
	var out struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.URL, nil
}

// Unpublish takes the site offline.
func (c *Client) Unpublish(ctx context.Context, siteID string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/website/"+siteID+"/unpublish", nil, "")
	return err
}

// ReadMeta loads the .memcode/website.json marker from a pulled repo.
func ReadMeta(dir string) (Meta, error) {
	raw, err := os.ReadFile(filepath.Join(dir, metaRelPath))
	if err != nil {
		return Meta{}, fmt.Errorf("%s is not a pulled memcode website (missing %s) — use `memcode websites pull <slug>`", dir, metaRelPath)
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil || m.SiteID == "" {
		return Meta{}, fmt.Errorf("corrupt %s", metaRelPath)
	}
	return m, nil
}

func writeMeta(dir string, m Meta) error {
	if err := os.MkdirAll(filepath.Join(dir, ".memcode"), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(dir, metaRelPath), append(raw, '\n'), 0o644)
}

// download streams a (signed, pre-authenticated) URL to w — no bearer header,
// the signature IS the auth and GCS rejects double auth.
func (c *Client) download(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bundle download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("bundle download failed: %s", resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// do runs one authenticated round trip and returns the raw body. body may be
// nil (JSON-less request) or an io.Reader with contentType set.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("memcode.ai unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("memcode.ai rejected the token — run `memcode login`")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("memcode.ai returned %s", resp.Status)
	}
	return raw, nil
}
