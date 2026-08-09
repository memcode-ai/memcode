// Package artifacts is the CLI's client for memcode.ai's artifact hosting: the
// agent publishes a self-contained HTML page and gets a stable, unguessable URL
// (memcode.ai/code/artifact/<id>) back. Publish/Update/List/Delete map onto the
// www /api/cli/artifacts endpoints, authenticated with the same bearer token the
// login flow mints (MEMCODE_API_TOKEN). This talks to the WEB APP, not the
// inference gateway — a deliberate, separate client.
package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/provider"
)

const (
	// defaultWebAppURL deliberately duplicates cmd/login.go's constant —
	// internal packages must not import cmd. Same env override, same default.
	defaultWebAppURL = "https://memcode.ai"
	envWebAppURL     = "MEMCODE_WEB_APP_URL"
	requestTimeout   = 30 * time.Second

	// MaxHTMLBytes mirrors the server's 1.5MB cap so an oversize artifact fails
	// fast locally instead of uploading 1.5MB to get a 413.
	MaxHTMLBytes = 1536 * 1024
)

// Artifact is one published page, as the www API reports it.
type Artifact struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	URL       string `json:"url"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// Client is an authenticated memcode.ai artifacts client.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New builds a client from the environment. Errors when the user has no token —
// the caller surfaces "run `memcode login`".
func New() (*Client, error) {
	token := strings.TrimSpace(os.Getenv(provider.EnvAPIToken))
	if token == "" {
		return nil, fmt.Errorf("not logged in — run `memcode login` to publish artifacts")
	}
	base := strings.TrimSpace(os.Getenv(envWebAppURL))
	if base == "" {
		base = defaultWebAppURL
	}
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: requestTimeout},
	}, nil
}

// Publish creates a new artifact and returns it (URL included).
func (c *Client) Publish(ctx context.Context, title, html string) (Artifact, error) {
	var out Artifact
	err := c.do(ctx, http.MethodPost, "/api/cli/artifacts",
		map[string]string{"title": title, "html": html}, &out)
	return out, err
}

// Update replaces an artifact's content in place — the URL stays stable.
func (c *Client) Update(ctx context.Context, id, title, html string) (Artifact, error) {
	body := map[string]string{"html": html}
	if title != "" {
		body["title"] = title
	}
	var out Artifact
	err := c.do(ctx, http.MethodPut, "/api/cli/artifacts/"+id, body, &out)
	return out, err
}

// List returns the org's artifacts, newest first.
func (c *Client) List(ctx context.Context) ([]Artifact, error) {
	var out struct {
		Artifacts []Artifact `json:"artifacts"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/cli/artifacts", nil, &out); err != nil {
		return nil, err
	}
	return out.Artifacts, nil
}

// Delete takes a published artifact down.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/cli/artifacts/"+id, nil, nil)
}

// do runs one JSON request/response round trip with the bearer attached.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("memcode.ai unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("memcode.ai rejected the token — run `memcode login`")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Surface the server's error message verbatim (413/429 carry actionable text).
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("memcode.ai returned %s: %s", resp.Status, clipBody(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding memcode.ai response: %w", err)
	}
	return nil
}

func clipBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
