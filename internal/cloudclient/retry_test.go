package cloudclient

// The side-channel retry contract: every advisor/websearch/byok call rides
// requestWithRetry — a Cloud Run cold-start 5xx retries (with the notify
// callback firing) instead of killing the call.

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
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

// A 413 carrying the context-overflow code is NEVER retried — the caller handles it
// via compaction, so requestWithRetry must return wire.ErrContextOverflow after one call.
func TestOverflowNeverRetried(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"code":"` + wire.CodeContextOverflow + `"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	fastRetry(c)
	resp, raw, err := c.requestWithRetry(context.Background(), http.MethodPost, "/v1/x", []byte(`{}`), false, "")
	if !errors.Is(err, wire.ErrContextOverflow) {
		t.Fatalf("err = %v, want ErrContextOverflow", err)
	}
	if resp != nil {
		t.Error("overflow must return a nil resp (body already consumed)")
	}
	if len(raw) == 0 {
		t.Error("overflow must return the raw body for the caller")
	}
	if calls != 1 {
		t.Fatalf("calls=%d — overflow must never retry", calls)
	}
}

// Exhausting the retries on a retryable status returns the error with a nil resp and the
// raw body (so the caller's own body inspection still works), after maxRetries+1 calls.
func TestRetryExhaustionReturnsRaw(t *testing.T) {
	srv, calls := flaky(t, 99, http.StatusServiceUnavailable, ``)
	c := New(srv.URL, "tok")
	fastRetry(c)
	resp, raw, err := c.requestWithRetry(context.Background(), http.MethodPost, "/v1/x", []byte(`{}`), false, "")
	if err == nil {
		t.Fatal("exhaustion must surface the final error")
	}
	if resp != nil {
		t.Error("exhaustion must return a nil resp (body already closed)")
	}
	if raw == nil {
		t.Error("exhaustion must return the raw body bytes")
	}
	if *calls != maxRetries+1 {
		t.Fatalf("calls=%d, want %d", *calls, maxRetries+1)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	mk := func(v string) *http.Response {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return &http.Response{Header: h}
	}
	if d, ok := retryAfterDelay(mk("2")); !ok || d != 2*time.Second {
		t.Errorf("seconds form: %v %v", d, ok)
	}
	// Values above the cap clamp to it (numeric and HTTP-date forms alike).
	if d, ok := retryAfterDelay(mk("3600")); !ok || d != time.Duration(retryCapMs)*time.Millisecond {
		t.Errorf("cap: %v %v", d, ok)
	}
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if d, ok := retryAfterDelay(mk(future)); !ok || d <= 0 || d > time.Duration(retryCapMs)*time.Millisecond {
		t.Errorf("http-date form: %v %v", d, ok)
	}
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	if d, ok := retryAfterDelay(mk(past)); !ok || d != 0 {
		t.Errorf("past http-date should be 0 delay: %v %v", d, ok)
	}
	if _, ok := retryAfterDelay(mk("")); ok {
		t.Error("absent header must be ok=false")
	}
	if _, ok := retryAfterDelay(mk("soon")); ok {
		t.Error("garbage header must be ok=false")
	}
}

// backoffDelay stays under the cap no matter how large the attempt gets.
func TestBackoffDelayCapped(t *testing.T) {
	cap := time.Duration(retryCapMs) * time.Millisecond
	for attempt := 0; attempt < 12; attempt++ {
		for i := 0; i < 20; i++ {
			if d := backoffDelay(attempt); d < 0 || d >= cap {
				t.Fatalf("attempt %d: delay %v outside [0, %v)", attempt, d, cap)
			}
		}
	}
}

// Only genuinely transient transport failures retry: never NXDOMAIN, TLS certificate
// problems, or an unsupported scheme (*url.Error itself always claims net.Error, so the
// classifier must unwrap it).
func TestRetryableNetErr(t *testing.T) {
	wrap := func(err error) error { return &url.Error{Op: "Post", URL: "https://x", Err: err} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dns not found", wrap(&net.DNSError{Err: "no such host", IsNotFound: true}), false},
		{"dns flaky", wrap(&net.DNSError{Err: "server misbehaving", IsTemporary: true}), true},
		{"timeout", wrap(&net.DNSError{Err: "i/o timeout", IsTimeout: true}), true},
		{"conn refused", wrap(&net.OpError{Op: "dial", Err: errors.New("connection refused")}), true},
		{"tls unknown authority", wrap(x509.UnknownAuthorityError{}), false},
		{"tls hostname", wrap(x509.HostnameError{Host: "x"}), false},
		{"unsupported scheme", wrap(errors.New(`unsupported protocol scheme "ftp"`)), false},
		{"ctx canceled", wrap(context.Canceled), false},
	}
	for _, c := range cases {
		if got := retryableNetErr(c.err); got != c.want {
			t.Errorf("%s: retryableNetErr = %v, want %v", c.name, got, c.want)
		}
	}
}
