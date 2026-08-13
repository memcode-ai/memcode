package cloudclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// WebSearch proxies the gateway's server-side web_search capability.
func (c *Client) WebSearch(ctx context.Context, query string) (string, error) {
	return c.webText(ctx, "/v1/websearch", map[string]string{"query": query})
}

// WebFetch proxies the gateway's server-side web_fetch capability.
func (c *Client) WebFetch(ctx context.Context, url string) (string, error) {
	return c.webText(ctx, "/v1/webfetch", map[string]string{"url": url})
}

// Advise asks the gateway's second-opinion advisor (a different vendor, reasoning on)
// for guidance on the best path forward. effort is the reasoning depth
// (low|medium|high; "" → high).
func (c *Client) Advise(ctx context.Context, question, effort string) (string, error) {
	body, err := json.Marshal(map[string]string{"question": question, "effort": effort})
	if err != nil {
		return "", err
	}
	resp, raw, err := c.postWithRetry(ctx, "/v1/advisor", body, false, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if raw == nil {
		raw, _ = io.ReadAll(resp.Body)
	}
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp.StatusCode, raw)
	}
	var out struct {
		Advice string `json:"advice"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decoding advisor response: %w", err)
	}
	return out.Advice, nil
}

func (c *Client) webText(ctx context.Context, path string, in map[string]string) (string, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	resp, raw, err := c.postWithRetry(ctx, path, body, false, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if raw == nil {
		raw, _ = io.ReadAll(resp.Body)
	}
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp.StatusCode, raw)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decoding gateway response: %w", err)
	}
	return out.Text, nil
}
