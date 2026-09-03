// Package cloudclient is the CLI's HTTP client for the memcode gateway's
// SIDE-CHANNEL surfaces: /v1/advisor, /v1/websearch, /v1/webfetch, and the
// /v1/byok key-management routes. The TURN wire lives elsewhere — the shared
// providers/memcode transport (OpenAI-compat + the memcode extensions). Every
// call here rides requestWithRetry (Cloud Run cold-start 5xx / 429 / transient
// net errors), with SetRetryNotify surfacing "⊙ retrying…" in the TUI.
package cloudclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

// Client talks to one memcode gateway deployment.
type Client struct {
	baseURL string // e.g. https://memcode-api-xxxx.run.app
	token   string // bearer token (v1: single static token)
	http    *http.Client
	// retryNotify, when set, is called before each retry backoff so a caller can
	// surface "retrying…" in its UI. nil = silent retry.
	retryNotify func(attempt int, err error, delay time.Duration)
}

// Option configures a Client.
type Option func(*Client)

// WithRetryNotify registers a callback invoked before each retry backoff, so a
// caller (the runtime loop) can surface "⊙ retrying…" in the TUI. The attempt is
// 1-based (1 = first retry); err is the failure that triggered the retry; delay
// is the backoff the client is about to sleep. Non-breaking: callers that don't
// set it get silent retry.
func WithRetryNotify(fn func(attempt int, err error, delay time.Duration)) Option {
	return func(c *Client) { c.retryNotify = fn }
}

// SetRetryNotify wires a retry-notify callback AFTER construction (e.g. from the
// runtime, which doesn't own the client at provider.NewFromEnv time). Same
// callback contract as WithRetryNotify. Non-breaking: nil = silent retry.
func (c *Client) SetRetryNotify(fn func(attempt int, err error, delay time.Duration)) {
	c.retryNotify = fn
}

// New returns a client for the hosted gateway.
func New(baseURL, token string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// http.Client.Timeout caps the ENTIRE request — including reading the streaming
		// body — so a stream running past it is killed mid-flight ("stream ended without a
		// response event"). Set it just ABOVE the gateway's 60-min request timeout so a
		// genuinely-long turn is bounded by the GATEWAY (clean error), and this is only a
		// backstop against a hung connection. The turn ctx (esc / ctrl-c) cancels normally;
		// the per-call stream-cut retry covers transient blips.
		http: &http.Client{Timeout: 62 * time.Minute},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// request issues one HTTP call with the client's auth/session headers. body
// may be nil (GET/DELETE).
func (c *Client) request(ctx context.Context, method, path string, body []byte, stream bool, session string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.token)
	if session != "" {
		// the gateway counts main_loop turns per session (bootstrap vs continuation routing)
		req.Header.Set("X-Memcode-Session", session)
	}
	if stream {
		req.Header.Set("accept", "text/event-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("memcode api request: %w", err)
	}
	return resp, nil
}

// --- retry for transient HTTP errors ---
//
// The gateway sits behind Cloud Run, so a cold-start 502/503, a 429, or a transient
// net.OpError is a realistic failure mode that kills an otherwise-healthy turn. The
// retry wraps the post→response-read sequence: on a retryable status (429, 500, 502,
// 503, 504) or a net error, it backs off with exponential + jitter and re-issues the
// POST (the gateway is stateless between calls — a failed call appended nothing). 4xx
// (except 429) are permanent and never retried; ErrContextOverflow (413+code) is left
// for the caller to handle via compaction. Bounded at maxRetries with a cap so a
// genuinely-down gateway fails cleanly.

const (
	maxRetries  = 3
	retryBaseMs = 1000  // base backoff: 1s
	retryFactor = 2     // exponential factor
	retryCapMs  = 10000 // backoff cap: 10s
)

// retryableStatus reports whether an HTTP status is a transient failure worth
// retrying: 429 (rate limit), 500, 502, 503, 504 (gateway/server errors). 413 is
// NOT here — it may carry the context-overflow code, which the caller handles via
// compaction, not retry.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// retryableNetErr reports whether a request error is a transient transport failure
// (a connection reset, a DNS hiccup, a timeout) rather than a permanent config error
// (DNS NXDOMAIN, a TLS certificate problem, an unsupported scheme) that no amount of
// retrying can fix.
func retryableNetErr(err error) bool {
	// *url.Error (what http.Client returns) itself satisfies net.Error unconditionally,
	// so unwrap it first and classify what actually failed.
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false // the caller's ctx decides, not the retry loop
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return !dnsErr.IsNotFound // retry a flaky resolver, never NXDOMAIN
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	var unknownAuth x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	if errors.As(err, &unknownAuth) || errors.As(err, &hostname) || errors.As(err, &certInvalid) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true // timeout, connection refused/reset
	}
	return false // e.g. unsupported protocol scheme — permanent config error
}

// retryAfterDelay parses the Retry-After header (seconds or HTTP-date) and returns
// the delay. Returns ok=false if the header is absent or unparseable.
func retryAfterDelay(resp *http.Response) (time.Duration, bool) {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	// Numeric seconds (the common form).
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		if d > time.Duration(retryCapMs)*time.Millisecond {
			d = time.Duration(retryCapMs) * time.Millisecond
		}
		return d, true
	}
	// HTTP-date form (rare).
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0, true
		}
		if d > time.Duration(retryCapMs)*time.Millisecond {
			d = time.Duration(retryCapMs) * time.Millisecond
		}
		return d, true
	}
	return 0, false
}

// backoffDelay computes an exponential backoff with full jitter: delay = rand(0, base * 2^attempt),
// capped at retryCap. Jitter spreads concurrent retries so a thundering herd of
// clients doesn't synchronize on the same backoff slot.
func backoffDelay(attempt int) time.Duration {
	base := time.Duration(retryBaseMs) * time.Millisecond
	for i := 0; i < attempt; i++ {
		base *= time.Duration(retryFactor)
		if base > time.Duration(retryCapMs)*time.Millisecond {
			base = time.Duration(retryCapMs) * time.Millisecond
			break
		}
	}
	// Full jitter: [0, base)
	return time.Duration(rand.Int63n(int64(base)))
}

// postWithRetry issues the POST and retries on transient failures (retryable status
// codes or net errors). It reads and closes the response body for non-OK retryable
// statuses (so the connection can be reused) and returns the final response + its
// body bytes. The body is returned separately so the caller can inspect it without
// re-reading (the Body is already closed). For a non-retryable failure, the returned
// resp has an open Body that the caller must close — same contract as post, just with
// retries layered in. When the final attempt is a retryable status, resp.Body is
// closed and raw holds the body bytes.
func (c *Client) postWithRetry(ctx context.Context, path string, body []byte, stream bool, session string) (*http.Response, []byte, error) {
	return c.requestWithRetry(ctx, http.MethodPost, path, body, stream, session)
}

// requestWithRetry wraps request with the transient-failure retry policy — the
// ONE retry path every side-channel and BYOK call rides (the turn wire has its
// own transport in providers/compat).
func (c *Client) requestWithRetry(ctx context.Context, method, path string, body []byte, stream bool, session string) (*http.Response, []byte, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		resp, err := c.request(ctx, method, path, body, stream, session)
		if err != nil {
			if retryableNetErr(err) && attempt < maxRetries {
				delay := backoffDelay(attempt)
				if c.retryNotify != nil {
					c.retryNotify(attempt+1, err, delay)
				}
				select {
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				case <-time.After(delay):
				}
				lastErr = err
				continue
			}
			return nil, nil, err
		}
		// A 413 may carry the context-overflow code — never retried, mapped to the
		// sentinel HERE so every call site handles compaction the same way. A 413
		// without the code stays a plain non-retryable response below.
		if resp.StatusCode == http.StatusRequestEntityTooLarge {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if overflowResponse(resp.StatusCode, raw) {
				return nil, raw, wire.ErrContextOverflow
			}
			return nil, raw, apiError(resp.StatusCode, raw)
		}
		// Non-retryable status → return the open response for the caller to handle.
		if !retryableStatus(resp.StatusCode) {
			return resp, nil, nil
		}
		// Retryable status: read the body for Retry-After handling, close it, then
		// decide whether to retry.
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if attempt < maxRetries {
			// Honor Retry-After if present (429/503), otherwise exponential jitter.
			delay, ok := retryAfterDelay(resp)
			if !ok {
				delay = backoffDelay(attempt)
			}
			if c.retryNotify != nil {
				c.retryNotify(attempt+1, apiError(resp.StatusCode, raw), delay)
			}
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(delay):
			}
			lastErr = apiError(resp.StatusCode, raw)
			continue
		}
		// Exhausted retries on a retryable status → return the error + the raw body
		// (so the caller's overflowResponse check still works).
		return nil, raw, apiError(resp.StatusCode, raw)
	}
	// Unreachable: the loop either returns or continues. Guard for safety.
	return nil, nil, lastErr
}

// overflowResponse reports whether a non-200 gateway response is a context-window
// overflow (HTTP 413 with the machine-readable code) — never retried; the caller
// compacts instead.
func overflowResponse(status int, raw []byte) bool {
	if status != http.StatusRequestEntityTooLarge {
		return false
	}
	var e struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(raw, &e) == nil && e.Code == wire.CodeContextOverflow
}

func apiError(code int, raw []byte) error {
	// A 401 means the stored token is invalid/expired/revoked (e.g. the retired
	// legacy static token after the 2026-07-24 auth change). Wrap the sentinel
	// so hosts can flip to signed-out and prompt for /login instead of showing
	// a raw HTTP error mid-turn.
	if code == http.StatusUnauthorized {
		return fmt.Errorf("%w", wire.ErrUnauthorized)
	}
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	decoded := json.Unmarshal(raw, &e) == nil
	// 402s carry a machine-readable code → sentinel, mapped HERE so every call
	// site (Complete, stream connect, Advise/WebSearch/WebFetch) gets it free.
	if code == http.StatusPaymentRequired && decoded {
		switch e.Code {
		case wire.CodeInsufficientCredit:
			return wire.ErrInsufficientCredit
		case wire.CodeSubscriptionRequired:
			return wire.ErrSubscriptionRequired
		case wire.CodeAccountLocked:
			return wire.ErrAccountLocked
		}
	}
	// A 422 with the byok code means the turn died on the USER's own provider
	// key (fail-the-turn doctrine — never absorbed onto memcode's keys, never
	// retried; the fix is /apikeys). Sentinel, same as the 402 family above.
	if code == http.StatusUnprocessableEntity && decoded && e.Code == wire.CodeByokKeyFailed {
		return wire.ErrByokKeyFailed
	}
	if decoded && e.Error != "" {
		return fmt.Errorf("memcode api http %d: %s", code, e.Error)
	}
	return fmt.Errorf("memcode api http %d: %s", code, string(raw))
}
