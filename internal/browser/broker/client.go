package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// Client talks to a gateway-owned Server over its Unix socket. A delegated
// worker is a separate OS process from the gateway (see jobs.SpawnWithSpec),
// so it cannot hold the *Broker* itself — this is how it reaches the SAME
// broker the gateway owns to get an exclusive existing-Chrome lease.
type Client struct {
	socketPath string
	http       *http.Client
}

// NewClient does not itself verify the socket is reachable — call Acquire and
// handle ErrNotConnected; that is the fail-closed path callers must take.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Reachable reports whether the socket exists and a gateway is actually
// listening on it — the check callers use to fail closed before ever trying
// to drive existing-Chrome, rather than surfacing a confusing connect error
// mid-task.
func (c *Client) Reachable() bool {
	if _, err := os.Stat(c.socketPath); err != nil {
		return false
	}
	resp, err := c.http.Get("http://broker/can_mutate?token=&page=")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func (c *Client) post(path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.http.Post("http://broker"+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotConnected, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Acquire requests exclusive existing-Chrome mutation rights for (agentID,
// runID). Callers MUST fail closed on error — no ephemeral-browser fallback.
func (c *Client) Acquire(agentID, runID string, ttl time.Duration) (Lease, error) {
	var lease Lease
	err := c.post("/acquire", map[string]any{"AgentID": agentID, "RunID": runID, "TTLSeconds": int(ttl.Seconds())}, &lease)
	return lease, err
}

func (c *Client) Release(token string) error {
	return c.post("/release", map[string]string{"Token": token}, nil)
}

func (c *Client) OwnPage(token, page string) error {
	return c.post("/own_page", map[string]string{"Token": token, "Page": page}, nil)
}

func (c *Client) CanMutate(token, page string) bool {
	resp, err := c.http.Get(fmt.Sprintf("http://broker/can_mutate?token=%s&page=%s", token, page))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		CanMutate bool `json:"can_mutate"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.CanMutate
}
