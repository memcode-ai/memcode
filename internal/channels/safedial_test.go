package channels

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "10.1.2.3", "192.168.0.1", "172.16.0.1",
		"169.254.169.254", "100.64.1.1", "0.0.0.0", "fe80::1"}
	for _, s := range blocked {
		if !blockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::"}
	for _, s := range allowed {
		if blockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
	if !blockedIP(nil) {
		t.Error("nil IP must be blocked")
	}
}

// The SSRF-guarded client refuses to connect to a loopback server, even though
// the URL is a normal http:// address — the block happens at dial on the
// resolved IP.
func TestSafeHTTPClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer srv.Close()

	c := SafeHTTPClient(5 * time.Second)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("loopback fetch must be refused")
	}
}

func TestHostAllowed(t *testing.T) {
	yes := [][2]string{
		{"smba.trafficmanager.net", "smba.trafficmanager.net"},
		{"foo.botframework.com", ".botframework.com"},
		{"a.b.googleusercontent.com", ".googleusercontent.com"},
		{"foo.botframework.com:443", ".botframework.com"},
	}
	for _, c := range yes {
		if !HostAllowed(c[0], c[1]) {
			t.Errorf("HostAllowed(%q,%q) = false", c[0], c[1])
		}
	}
	// A lookalike domain must NOT match on a dot boundary.
	if HostAllowed("evil-botframework.com", ".botframework.com") {
		t.Error("lookalike host matched")
	}
	if HostAllowed("botframework.com.attacker.net", ".botframework.com") {
		t.Error("suffix-in-the-middle matched")
	}
}
