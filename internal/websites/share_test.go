package websites

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestShareCodeAcceptsLinksAndCodes(t *testing.T) {
	for in, want := range map[string]string{
		"https://memcode.ai/p/Abc23xyz":   "Abc23xyz",
		"https://memcode.ai/p/Abc23xyz/":  "Abc23xyz",
		"memcode.ai/p/Abc23xyz?utm=x":     "Abc23xyz",
		" Abc23xyz ":                      "Abc23xyz",
		"https://memcode.ai/p/../admin":   "",
		"short":                           "",
		"https://memcode.ai/p/Abc23x%2Fz": "",
	} {
		if got := ShareCode(in); got != want {
			t.Errorf("ShareCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShareAndUnshare(t *testing.T) {
	var gotBody string
	var deleted string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/website/s9/share":
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			json.NewEncoder(w).Encode(map[string]any{"link": map[string]string{
				"code": "Abc23xyz", "url": "https://memcode.ai/p/Abc23xyz", "expiresAt": "2026-10-20T00:00:00Z"}})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/website/s9/share/Abc23xyz":
			deleted = "Abc23xyz"
			json.NewEncoder(w).Encode(map[string]bool{"revoked": true})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	link, err := c.Share(context.Background(), "s9", 3)
	if err != nil || link.URL != "https://memcode.ai/p/Abc23xyz" || gotBody != `{"days":3}` {
		t.Fatalf("share: %+v %v body=%s", link, err, gotBody)
	}
	if err := c.Unshare(context.Background(), "s9", link.URL); err != nil || deleted != "Abc23xyz" {
		t.Fatalf("unshare: %v deleted=%q", err, deleted)
	}
	if err := c.Unshare(context.Background(), "s9", "not a link"); err == nil {
		t.Fatal("a non-link must be refused before any request")
	}
}
