package client

// The side-channel retry contract: every advisor/websearch/byok call rides
// requestWithRetry — a Cloud Run cold-start 5xx retries (with the notify
// callback firing) instead of killing the call.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func flaky(t *testing.T, failures int, status int, body string) (*httptest.Server, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		if n <= int64(failures) {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func fastRetry(c *Client) {
	c.http = &http.Client{Timeout: 10 * time.Second}
}

func TestWebSearchRetriesColdStart(t *testing.T) {
	srv, calls := flaky(t, 1, http.StatusServiceUnavailable, `{"text":"answer"}`)
	var notified int
	c := New(srv.URL, "tok", WithRetryNotify(func(int, error, time.Duration) { notified++ }))
	fastRetry(c)
	out, err := c.WebSearch(context.Background(), "q")
	if err != nil || out != "answer" {
		t.Fatalf("WebSearch = %q, %v", out, err)
	}
	if *calls != 2 || notified != 1 {
		t.Fatalf("calls=%d notified=%d, want a single retry with notify", *calls, notified)
	}
}

func TestByokListRetriesColdStart(t *testing.T) {
	srv, calls := flaky(t, 1, http.StatusBadGateway, `{"keys":[]}`)
	c := New(srv.URL, "tok")
	fastRetry(c)
	if _, err := c.ByokList(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("calls=%d, want retry", *calls)
	}
}

func TestNonRetryableSurfacesImmediately(t *testing.T) {
	srv, calls := flaky(t, 99, http.StatusForbidden, ``)
	c := New(srv.URL, "tok")
	fastRetry(c)
	if _, err := c.WebSearch(context.Background(), "q"); err == nil {
		t.Fatal("want the 403 to surface")
	}
	if *calls != 1 {
		t.Fatalf("calls=%d — 4xx must never retry", *calls)
	}
}
