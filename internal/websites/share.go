package websites

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ShareLink is a short link to a site's draft preview (memcode.ai/p/{code}).
// Anyone with it can view the draft until it expires or is turned off; it
// never shows or changes the live site.
type ShareLink struct {
	Code      string    `json:"code"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Share makes a share link for a site's draft, valid for days (0 = the
// server's default, a week).
func (c *Client) Share(ctx context.Context, siteID string, days int) (ShareLink, error) {
	body, _ := json.Marshal(map[string]int{"days": days})
	if days == 0 {
		body = []byte("{}")
	}
	raw, err := c.do(ctx, http.MethodPost, "/api/website/"+siteID+"/share", bytes.NewReader(body), "application/json")
	if err != nil {
		return ShareLink{}, err
	}
	var out struct {
		Link ShareLink `json:"link"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Link.URL == "" {
		return ShareLink{}, fmt.Errorf("decoding share link")
	}
	return out.Link, nil
}

// ShareLinks lists a site's share links that still work.
func (c *Client) ShareLinks(ctx context.Context, siteID string) ([]ShareLink, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/website/"+siteID+"/share", nil, "")
	if err != nil {
		return nil, err
	}
	var out struct {
		Links []ShareLink `json:"links"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding share links")
	}
	return out.Links, nil
}

// Unshare turns a share link off. It takes the code or the whole link.
func (c *Client) Unshare(ctx context.Context, siteID, codeOrURL string) error {
	code := ShareCode(codeOrURL)
	if code == "" {
		return fmt.Errorf("%q isn't a share link", codeOrURL)
	}
	_, err := c.do(ctx, http.MethodDelete, "/api/website/"+siteID+"/share/"+url.PathEscape(code), nil, "")
	return err
}

// ShareCode extracts the code from a share link or returns a bare code.
func ShareCode(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "/p/"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.TrimRight(strings.SplitN(s, "?", 2)[0], "/")
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	if len(s) < 8 || len(s) > 16 {
		return ""
	}
	return s
}
