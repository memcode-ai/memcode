package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// fakeTokens is a live token source: Token serves cur; Rejected swaps to next.
type fakeTokens struct {
	cur, next string
	rejected  []string
}

func (f *fakeTokens) Token() (string, error) { return f.cur, nil }
func (f *fakeTokens) Rejected(tok string) (string, error) {
	f.rejected = append(f.rejected, tok)
	f.cur = f.next
	return f.cur, nil
}

// Regression: a Claude subscription token captured at startup expired mid-session and every
// turn failed with "OAuth access token has expired" until memcode was restarted, even after
// the user re-authed Claude Code. A 401 now asks the live source for a replacement and retries
// once with it.
func TestStreamHealsExpiredOAuthToken(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seen = append(seen, auth)
		if auth != "Bearer sk-ant-oat-new" {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has expired."}}`))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sampleSSE))
	}))
	defer srv.Close()

	c := NewAnthropic("sk-ant-oat-old")
	c.baseURL = srv.URL
	ts := &fakeTokens{cur: "sk-ant-oat-old", next: "sk-ant-oat-new"}
	c.SetTokenSource(ts)

	resp, err := c.Stream(context.Background(), wire.Request{Model: "m"}, wire.StreamHandler{})
	if err != nil {
		t.Fatalf("Stream should self-heal the 401, got %v", err)
	}
	if len(ts.rejected) != 1 || ts.rejected[0] != "sk-ant-oat-old" {
		t.Fatalf("source should be told the old token was rejected once, got %v", ts.rejected)
	}
	if len(seen) != 2 || seen[1] != "Bearer sk-ant-oat-new" {
		t.Fatalf("want one retry with the fresh token, got %v", seen)
	}
	if resp.InputTokens == 0 {
		t.Fatalf("healed stream should return the real response, got %+v", resp)
	}
}

// Without a live source (a static console key) a 401 surfaces unchanged, with the readable
// "anthropic authentication_error: …" message.
func TestStreamStatic401Surfaces(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
	}))
	defer srv.Close()

	c := NewAnthropic("sk-ant-api-x")
	c.baseURL = srv.URL
	_, err := c.Stream(context.Background(), wire.Request{Model: "m"}, wire.StreamHandler{})
	if err == nil || !strings.HasPrefix(err.Error(), "anthropic authentication_error:") {
		t.Fatalf("want readable auth error, got %v", err)
	}
	if !isAuthError(err) {
		t.Fatal("error must still unwrap to the SDK 401")
	}
	if calls != 1 {
		t.Fatalf("a 401 is not retryable without a token source, got %d calls", calls)
	}
}
