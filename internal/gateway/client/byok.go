package client

// BYOK key management — the /v1/byok surface. Plain JSON calls (not
// turn-shaped): list is read-only metadata, put/delete/validate are explicit
// user actions from /apikeys, so there is no retry machinery here — an error
// surfaces immediately and the user re-runs the action.
//
// Hygiene: ByokPut is the ONLY method that carries key material, request-side
// only over TLS; no response ever returns a key.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ByokKey is one masked key row (never the key itself).
type ByokKey struct {
	Provider  string `json:"provider"`
	Tail      string `json:"tail"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ByokKeys is the /v1/byok/keys listing: the server-enumerated provider
// roster (registry-derived — clients never hardcode it) + this user's rows.
type ByokKeys struct {
	Providers []string  `json:"providers"`
	Keys      []ByokKey `json:"keys"`
	Warning   string    `json:"warning,omitempty"`
}

// ByokPutResult is the masked outcome of storing a key.
type ByokPutResult struct {
	Provider string `json:"provider"`
	Tail     string `json:"tail"`
	Status   string `json:"status"`
	Warning  string `json:"warning,omitempty"`
}

// ByokList fetches the provider roster + the user's masked key rows.
func (c *Client) ByokList(ctx context.Context) (ByokKeys, error) {
	var out ByokKeys
	err := c.byokCall(ctx, http.MethodGet, "/v1/byok/keys", nil, &out)
	return out, err
}

// ByokPut stores (or replaces) the user's key for provider. The gateway
// live-probes the vendor first by default.
func (c *Client) ByokPut(ctx context.Context, provider, key string) (ByokPutResult, error) {
	var out ByokPutResult
	err := c.byokCall(ctx, http.MethodPut, "/v1/byok/keys/"+provider, map[string]string{"key": key}, &out)
	return out, err
}

// ByokDelete removes the user's key for provider.
func (c *Client) ByokDelete(ctx context.Context, provider string) error {
	return c.byokCall(ctx, http.MethodDelete, "/v1/byok/keys/"+provider, nil, nil)
}

// ByokValidate live-probes the STORED key. ok=false carries the vendor's
// complaint in msg.
func (c *Client) ByokValidate(ctx context.Context, provider string) (bool, string, error) {
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.byokCall(ctx, http.MethodPost, "/v1/byok/keys/"+provider+"/validate", nil, &out); err != nil {
		return false, "", err
	}
	return out.OK, out.Error, nil
}

// byokCall is the shared JSON round-trip: bearer-authed, bounded timeout, and
// gateway error bodies surfaced as their message.
func (c *Client) byokCall(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, raw, err := c.requestWithRetry(cctx, method, path, body, false, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if raw == nil {
		raw, _ = io.ReadAll(resp.Body)
	}
	if resp.StatusCode != http.StatusOK {
		return apiError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding byok response: %w", err)
	}
	return nil
}
