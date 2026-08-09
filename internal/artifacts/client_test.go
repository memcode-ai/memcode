package artifacts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/provider"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv(provider.EnvAPIToken, "memcode_testtoken")
	t.Setenv(envWebAppURL, srv.URL)
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRequiresToken(t *testing.T) {
	t.Setenv(provider.EnvAPIToken, "")
	if _, err := New(); err == nil || !strings.Contains(err.Error(), "memcode login") {
		t.Fatalf("New without a token must point at `memcode login`, got %v", err)
	}
}

func TestPublishRoundTrip(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/cli/artifacts" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer memcode_testtoken" {
			t.Errorf("bearer not attached: %q", got)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["title"] != "T" || body["html"] != "<p>x</p>" {
			t.Errorf("body mismatch: %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": "abc", "url": "https://memcode.ai/code/artifact/abc",
		})
	})
	art, err := c.Publish(context.Background(), "T", "<p>x</p>")
	if err != nil {
		t.Fatal(err)
	}
	if art.ID != "abc" || !strings.Contains(art.URL, "/code/artifact/abc") {
		t.Fatalf("bad artifact: %+v", art)
	}
}

func TestUpdateAndDeletePaths(t *testing.T) {
	var gotMethod, gotPath string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc", "url": "u"})
	})
	if _, err := c.Update(context.Background(), "abc", "", "<p>v2</p>"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/cli/artifacts/abc" {
		t.Fatalf("update hit %s %s", gotMethod, gotPath)
	}
	if err := c.Delete(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/cli/artifacts/abc" {
		t.Fatalf("delete hit %s %s", gotMethod, gotPath)
	}
}

func TestErrorSurfaces(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "artifact exceeds the 1.5MB limit"})
	})
	if _, err := c.Publish(context.Background(), "t", "x"); err == nil ||
		!strings.Contains(err.Error(), "1.5MB") {
		t.Fatalf("server error message must surface verbatim, got %v", err)
	}
}

func TestUnauthorizedPointsAtLogin(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := c.List(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "memcode login") {
		t.Fatalf("401 must point at `memcode login`, got %v", err)
	}
}
