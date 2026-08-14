package grok

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pointAt aims the package at a test server and a temp config dir, restoring
// everything on cleanup.
func pointAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldIssuer, oldDevice, oldDiscovery, oldSleep := issuer, deviceCodeURL, discoveryURL, sleep
	issuer = srv.URL
	deviceCodeURL = srv.URL + "/device"
	discoveryURL = srv.URL + "/.well-known/openid-configuration"
	sleep = func(time.Duration) {}
	t.Cleanup(func() { issuer, deviceCodeURL, discoveryURL, sleep = oldIssuer, oldDevice, oldDiscovery, oldSleep })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestLoginDeviceFlow(t *testing.T) {
	polls := 0
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"authorization_endpoint": srv.URL + "/authorize", "token_endpoint": srv.URL + "/token"})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		if got := r.FormValue("client_id"); got != clientID {
			t.Errorf("device request client_id = %q", got)
		}
		writeJSON(w, 200, map[string]any{
			"device_code": "dev-1", "user_code": "ABCD-1234",
			"verification_uri": "https://accounts.x.ai/activate", "verification_uri_complete": "https://accounts.x.ai/activate?code=ABCD-1234",
			"expires_in": 600, "interval": 1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if got := r.FormValue("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("poll grant_type = %q", got)
		}
		polls++
		switch polls {
		case 1:
			writeJSON(w, 400, map[string]string{"error": "authorization_pending"})
		case 2:
			writeJSON(w, 400, map[string]string{"error": "slow_down"})
		default:
			writeJSON(w, 200, map[string]any{"access_token": "acc-1", "refresh_token": "ref-1", "expires_in": 21600})
		}
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	pointAt(t, srv)

	var sawURL, sawCode string
	err := Login(context.Background(), func(url, code string) { sawURL, sawCode = url, code })
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sawURL != "https://accounts.x.ai/activate?code=ABCD-1234" || sawCode != "ABCD-1234" {
		t.Errorf("prompt got url=%q code=%q", sawURL, sawCode)
	}
	if !Available() {
		t.Fatal("Available() = false after Login")
	}
	tok, err := Resolve()
	if err != nil || tok != "acc-1" {
		t.Fatalf("Resolve = %q, %v", tok, err)
	}
	info, err := os.Stat(tokenPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}
}

func TestResolveRefreshesAndRotates(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if got := r.FormValue("refresh_token"); got != "ref-old" {
			t.Errorf("refresh_token = %q", got)
		}
		// No refresh_token in the response: the old one must be kept.
		writeJSON(w, 200, map[string]any{"access_token": "acc-new", "expires_in": 21600})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	pointAt(t, srv)

	write(creds{AccessToken: "acc-old", RefreshToken: "ref-old", TokenEndpoint: srv.URL + "/token", ExpiresAt: time.Now().Unix() - 10})
	tok, err := Resolve()
	if err != nil || tok != "acc-new" {
		t.Fatalf("Resolve = %q, %v", tok, err)
	}
	c, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if c.RefreshToken != "ref-old" || c.AccessToken != "acc-new" {
		t.Errorf("persisted creds = %+v", c)
	}
}

func TestResolveServesLiveTokenWhenRefreshFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 500, map[string]string{"error": "server_error"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	pointAt(t, srv)

	// Inside the refresh skew but not yet expired: the failed refresh must not
	// fail the turn.
	write(creds{AccessToken: "acc-live", RefreshToken: "r", TokenEndpoint: srv.URL + "/token", ExpiresAt: time.Now().Unix() + 60})
	tok, err := Resolve()
	if err != nil || tok != "acc-live" {
		t.Fatalf("Resolve = %q, %v", tok, err)
	}
}

func TestRefreshTierGate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, map[string]string{"error": "access_denied"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	pointAt(t, srv)

	write(creds{AccessToken: "acc", RefreshToken: "r", TokenEndpoint: srv.URL + "/token", ExpiresAt: time.Now().Unix() - 10})
	if _, err := Resolve(); !errors.Is(err, ErrTierGated) {
		t.Fatalf("Resolve err = %v, want ErrTierGated", err)
	}
}

func TestValidateEndpointRejectsOffVendor(t *testing.T) {
	if err := validateEndpoint("https://auth.x.ai/oauth2/token"); err != nil {
		t.Errorf("x.ai endpoint rejected: %v", err)
	}
	if err := validateEndpoint("https://evil.example.com/token"); err == nil {
		t.Error("off-vendor endpoint accepted")
	}
	if err := validateEndpoint("http://x.ai/token"); err == nil {
		t.Error("plain-http endpoint accepted")
	}
}

func TestResolveWithoutLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := Resolve(); err == nil {
		t.Fatal("Resolve succeeded with no login")
	}
	if Available() {
		t.Error("Available() = true with no login")
	}
}

func TestLogout(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	write(creds{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Unix() + 3600})
	if !Available() {
		t.Fatal("write did not create the token file")
	}
	if err := Logout(); err != nil {
		t.Fatal(err)
	}
	if Available() {
		t.Error("Available() = true after Logout")
	}
	if err := Logout(); err != nil {
		t.Errorf("second Logout: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(tokenPath())); err != nil {
		t.Errorf("config dir should survive logout: %v", err)
	}
}
