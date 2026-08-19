package compat

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

// timeoutErr is a minimal net.Error whose Timeout() reports true — the shape
// of a dial/TLS-handshake/response-header timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "context deadline exceeded (Client.Timeout)" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// retryableNetErr must retry transient transport and NOTHING permanent.
// Every http.Client.Do error arrives wrapped in *url.Error (which itself
// implements net.Error) — the old blanket errors.As match retried permanent
// config errors (bad TLS, unsupported scheme, NXDOMAIN) three times each.
func TestRetryableNetErrClassification(t *testing.T) {
	wrap := func(err error) error { return &url.Error{Op: "Post", URL: "https://x/v1/chat/completions", Err: err} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dial timeout", wrap(timeoutErr{}), true},
		{"connection reset", wrap(&net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}), true},
		{"connection refused", wrap(&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}), true},
		{"unexpected EOF", wrap(io.ErrUnexpectedEOF), true},
		{"EOF", wrap(io.EOF), true},
		{"dns transient", wrap(&net.DNSError{Err: "server misbehaving", IsTemporary: true}), true},
		{"dns NXDOMAIN", wrap(&net.DNSError{Err: "no such host", IsNotFound: true}), false},
		{"tls cert", wrap(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), false},
		{"unsupported scheme", wrap(errors.New(`unsupported protocol scheme "foo"`)), false},
		{"malformed request", errors.New("net/http: invalid header field"), false},
	}
	for _, c := range cases {
		if got := retryableNetErr(c.err); got != c.want {
			t.Errorf("%s: retryableNetErr = %v, want %v", c.name, got, c.want)
		}
	}
}

// 529 (Anthropic overloaded) is transient and retryable, converged with
// provcore.IsRetryable's classification.
func TestRetryable529(t *testing.T) {
	if !retryableStatus(529) {
		t.Fatal("529 must be retryable")
	}
}

// Retry-After parsing (now shared with provcore) still handles both wire
// forms and caps at retryCapMs.
func TestRetryAfterDelayForms(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if _, ok := retryAfterDelay(resp); ok {
		t.Fatal("absent header must report ok=false")
	}
	resp.Header.Set("Retry-After", "2")
	if d, ok := retryAfterDelay(resp); !ok || d != 2*time.Second {
		t.Fatalf("seconds form = (%v, %v), want (2s, true)", d, ok)
	}
	resp.Header.Set("Retry-After", "3600") // beyond the cap
	if d, ok := retryAfterDelay(resp); !ok || d != time.Duration(retryCapMs)*time.Millisecond {
		t.Fatalf("capped form = (%v, %v), want (%dms, true)", d, ok, retryCapMs)
	}
	resp.Header.Set("Retry-After", time.Now().Add(4*time.Second).UTC().Format(http.TimeFormat))
	if d, ok := retryAfterDelay(resp); !ok || d <= 0 || d > 4*time.Second {
		t.Fatalf("HTTP-date form = (%v, %v), want (0, 4s], true", d, ok)
	}
}

// A stream that dies mid-flight must surface the usage the backend already
// reported billed — the native adapters' error paths do, and the meter reads
// it from the error-path response.
func TestStreamErrorPreservesPartialUsage(t *testing.T) {
	usageChunk := ChatChunk{
		Object: "chat.completion.chunk",
		Model:  "sonnet",
		Usage:  &Usage{PromptTokens: 100, CompletionTokens: 8},
	}

	// Mid-stream error envelope.
	tr := newTestTransport(t, true, func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			ChatChunk{Object: "chat.completion.chunk", Choices: []ChunkChoice{{Delta: Delta{Content: sp("par")}}}},
			usageChunk,
			`{"error":{"message":"backend died","type":"server_error"}}`,
			"[DONE]",
		)
	})
	resp, err := tr.Stream(context.Background(), wire.Request{
		Pin:      "sonnet",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
	}, wire.StreamHandler{})
	if err == nil {
		t.Fatal("want the mid-stream error to surface")
	}
	if resp.InputTokens != 100 || resp.OutputTokens != 8 {
		t.Fatalf("error envelope path lost partial usage: %+v", resp)
	}

	// Stream cut without [DONE].
	tr2 := newTestTransport(t, true, func(w http.ResponseWriter, r *http.Request) {
		sse(w, usageChunk) // no [DONE] — connection closes mid-stream
	})
	resp, err = tr2.Stream(context.Background(), wire.Request{
		Pin:      "sonnet",
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("x")}}},
	}, wire.StreamHandler{})
	if !errors.Is(err, wire.ErrStreamIncomplete) {
		t.Fatalf("cut stream must stay ErrStreamIncomplete, got %v", err)
	}
	if resp.InputTokens != 100 || resp.OutputTokens != 8 {
		t.Fatalf("cut-stream path lost partial usage: %+v", resp)
	}
}
