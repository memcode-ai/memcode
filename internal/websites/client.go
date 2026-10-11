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
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/provider"
)

// ErrBuildsGated is returned by Build/Edit/Push when the server's internal-
// preview gate (canSeeTestPlans) rejects the request: hosted building — an
// agent build, an inline edit, or a pushed-repo rebuild — is limited to
// internal accounts. There is no local fallback for these accounts (a site
// that was never built has no repo to pull), so callers report it plainly
// rather than retrying or suggesting a workaround.
var ErrBuildsGated = errors.New("website building is in limited internal preview — this account can't build or push sites yet")

// APIError is a non-2xx response from memcode.ai: the HTTP status plus the
// server's {error} message. Kept typed so callers branch on the status, never
// on the wording of a message someone may later rephrase.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

// gatedErr maps the build-path routes' 403 to ErrBuildsGated. On /build and
// POST /bundle a 403 has exactly one source — the internal-preview gate
// (entitlement failures are 402s) — so the status alone identifies it.
func gatedErr(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
		return ErrBuildsGated
	}
	return err
}

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
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Status      string `json:"status"`
	Model       string `json:"model,omitempty"`
	HasPreview  bool   `json:"has_preview,omitempty"`  // list route: a completed build exists to show
	PublishedAt string `json:"published_at,omitempty"` // non-empty once Publish has run
	LiveURL     string `json:"url,omitempty"`          // www list route emits `url` (siteUrl(slug))
}

// Build is one website_build row — a queued or finished build/edit run.
type Build struct {
	ID            string `json:"id"`
	Status        string `json:"status"` // pending|running|completed|failed
	Mode          string `json:"mode"`   // build|rebuild|edit
	Prompt        string `json:"prompt,omitempty"`
	Summary       string `json:"summary,omitempty"`
	Error         string `json:"error,omitempty"`
	DateStarted   string `json:"date_started,omitempty"`
	DateCompleted string `json:"date_completed,omitempty"`
}

// Edit is one verbatim find/replace applied by an edit-mode build, bypassing
// the agent for a fast, deterministic rebuild.
type Edit struct {
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

// Meta is the .memcode/website.json marker written at pull time.
type Meta struct {
	SiteID string `json:"site_id"`
	Slug   string `json:"slug"`
}

// Asset is one uploaded file in the site's durable assets/ prefix — a
// sibling of the build output, never touched by a rebuild.
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url,omitempty"`
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

// Create makes a new site. At least one of name/prompt should be given — the
// server distills a short name from the prompt when name is empty. This does
// NOT queue a build; call Build afterward to produce content (internal
// accounts only — others get ErrBuildsGated).
func (c *Client) Create(ctx context.Context, name, prompt, model string) (Site, error) {
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	if prompt != "" {
		body["prompt"] = prompt
	}
	if model != "" {
		body["model"] = model
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Site{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/website", bytes.NewReader(raw), "application/json")
	if err != nil {
		return Site{}, err
	}
	var wrapped struct {
		Website Site `json:"website"`
	}
	if err := json.Unmarshal(resp, &wrapped); err != nil {
		return Site{}, fmt.Errorf("decoding created site: %w", err)
	}
	return wrapped.Website, nil
}

// Build queues a build for the site: a fresh build from a prompt (prompt
// non-empty) or a no-prompt rebuild of the current state. model may be empty
// to use the site's own pinned model. Returns ErrBuildsGated when the
// server's canSeeTestPlans gate rejects the request.
func (c *Client) Build(ctx context.Context, siteID, prompt, model string) (Build, error) {
	mode := "build"
	if prompt == "" {
		mode = "rebuild"
	}
	return c.queueBuild(ctx, siteID, mode, prompt, model, nil)
}

// Edit queues an inline-edit build: verbatim find/replace edits applied by
// the builder without the agent, then a fast vite rebuild. Returns
// ErrBuildsGated when the server's canSeeTestPlans gate rejects the request.
func (c *Client) Edit(ctx context.Context, siteID string, edits []Edit, model string) (Build, error) {
	if len(edits) == 0 {
		return Build{}, fmt.Errorf("at least one edit is required")
	}
	return c.queueBuild(ctx, siteID, "edit", "", model, edits)
}

func (c *Client) queueBuild(ctx context.Context, siteID, mode, prompt, model string, edits []Edit) (Build, error) {
	body := map[string]any{"mode": mode}
	if prompt != "" {
		body["prompt"] = prompt
	}
	if model != "" {
		body["model"] = model
	}
	if edits != nil {
		body["edits"] = edits
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Build{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/website/"+siteID+"/build", bytes.NewReader(raw), "application/json")
	if err != nil {
		return Build{}, gatedErr(err)
	}
	var wrapped struct {
		Build Build `json:"build"`
	}
	if err := json.Unmarshal(resp, &wrapped); err != nil {
		return Build{}, fmt.Errorf("decoding build: %w", err)
	}
	return wrapped.Build, nil
}

// ListBuilds returns the site's build history, newest first.
func (c *Client) ListBuilds(ctx context.Context, siteID string) ([]Build, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/website/"+siteID+"/builds", nil, "")
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Builds []Build `json:"builds"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("decoding build list: %w", err)
	}
	return wrapped.Builds, nil
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
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			// A site's repo only exists once a build has run — a fresh site
			// has nothing to pull, and building is internal-preview gated.
			return Site{}, fmt.Errorf("%q has no repository yet — it has never been built", slug)
		}
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
		return Site{}, gatedErr(err)
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

// Delete soft-deletes the site: it stops serving and disappears from the org.
func (c *Client) Delete(ctx context.Context, siteID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/website/"+siteID, nil, "")
	return err
}

// Update patches the site's name and/or model. Either pointer may be nil to
// leave that field untouched; at least one must be set.
func (c *Client) Update(ctx context.Context, siteID string, name, model *string) (Site, error) {
	if name == nil && model == nil {
		return Site{}, fmt.Errorf("nothing to update")
	}
	body := map[string]any{}
	if name != nil {
		body["name"] = *name
	}
	if model != nil {
		body["model"] = *model
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Site{}, err
	}
	resp, err := c.do(ctx, http.MethodPatch, "/api/website/"+siteID, bytes.NewReader(raw), "application/json")
	if err != nil {
		return Site{}, err
	}
	var wrapped struct {
		Website Site `json:"website"`
	}
	if err := json.Unmarshal(resp, &wrapped); err != nil {
		return Site{}, fmt.Errorf("decoding updated site: %w", err)
	}
	return wrapped.Website, nil
}

// Get fetches one site by id (not slug), including its live URL. The detail
// route does NOT compute has_preview (only the list route does) — use
// GetBySlug when you need it.
func (c *Client) Get(ctx context.Context, siteID string) (Site, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/website/"+siteID, nil, "")
	if err != nil {
		return Site{}, err
	}
	var wrapped struct {
		Website Site `json:"website"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return Site{}, fmt.Errorf("decoding site: %w", err)
	}
	return wrapped.Website, nil
}

// GetBySlug resolves a slug and returns the site's full detail. It merges the
// two routes' views: the detail route has every column (model, published_at,
// …) but not has_preview, which only the list route computes — so the list
// row's HasPreview is carried over instead of being reset to false.
func (c *Client) GetBySlug(ctx context.Context, slug string) (Site, error) {
	listed, err := c.Resolve(ctx, slug)
	if err != nil {
		return Site{}, err
	}
	full, err := c.Get(ctx, listed.ID)
	if err != nil {
		return Site{}, err
	}
	full.HasPreview = listed.HasPreview
	return full, nil
}

// Preview mints a short-lived draft-preview URL: preview--{slug}.{apex},
// carrying the token website-host's draft serving path checks. Mirrors the
// exact construction the web app's UI does from the same /preview-token
// response — there is no separate CLI auth scheme.
func (c *Client) Preview(ctx context.Context, siteID string) (string, error) {
	site, err := c.Get(ctx, siteID)
	if err != nil {
		return "", err
	}
	if site.LiveURL == "" {
		return "", fmt.Errorf("site has no URL yet — has it been built?")
	}
	raw, err := c.do(ctx, http.MethodPost, "/api/website/"+siteID+"/preview-token", nil, "")
	if err != nil {
		return "", err
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.Token == "" {
		return "", fmt.Errorf("decoding preview token")
	}
	u, err := url.Parse(site.LiveURL)
	if err != nil {
		return "", fmt.Errorf("parsing site URL %q: %w", site.LiveURL, err)
	}
	parts := strings.SplitN(u.Host, ".", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("unexpected site URL host %q", u.Host)
	}
	preview := url.URL{
		Scheme:   u.Scheme,
		Host:     "preview--" + parts[0] + "." + parts[1],
		Path:     "/",
		RawQuery: "__memcode_token=" + tok.Token,
	}
	return preview.String(), nil
}

// ListAssets returns the site's durable uploaded assets.
func (c *Client) ListAssets(ctx context.Context, siteID string) ([]Asset, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/website/"+siteID+"/assets", nil, "")
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Assets []Asset `json:"assets"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("decoding asset list: %w", err)
	}
	return wrapped.Assets, nil
}

// UploadAsset uploads the file at localPath, named by its own base name
// unless name is non-empty.
func (c *Client) UploadAsset(ctx context.Context, siteID, localPath, name string) (Asset, error) {
	if name == "" {
		name = filepath.Base(localPath)
	}
	f, err := os.Open(localPath)
	if err != nil {
		return Asset{}, err
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		return Asset{}, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return Asset{}, err
	}
	if err := w.WriteField("name", name); err != nil {
		return Asset{}, err
	}
	if err := w.Close(); err != nil {
		return Asset{}, err
	}

	raw, err := c.do(ctx, http.MethodPost, "/api/website/"+siteID+"/assets", &buf, w.FormDataContentType())
	if err != nil {
		return Asset{}, err
	}
	var asset Asset
	if err := json.Unmarshal(raw, &asset); err != nil {
		return Asset{}, fmt.Errorf("decoding uploaded asset: %w", err)
	}
	return asset, nil
}

// DeleteAsset removes one uploaded asset by name.
func (c *Client) DeleteAsset(ctx context.Context, siteID, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/website/"+siteID+"/assets/"+url.PathEscape(name), nil, "")
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
		msg := "memcode.ai returned " + resp.Status
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	return raw, nil
}
