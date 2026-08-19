// Package provcore is the shared kernel under the provider WIRE adapters: the
// bounded retry loop, the tuned turn HTTP client, the native web-search tool
// swap, and the context-overflow classification — protocol plumbing shared by
// every adapter (the gateway's and the extracted ones alike), with NO routing
// policy, keys, or metering in it.
//
// This package exists so each provider protocol is implemented exactly ONCE
// and reused by both the hosted gateway and the CLI's direct endpoint mode —
// providers/{openai,anthropic,gemini,compat,memcode} all sit on this kernel.
// It imports NO vendor SDK (a guard test enforces it): adapters register
// their SDK's error shape via RegisterErrorInfo at init. Policy stays in the
// CLI's Runner; money stays in the gateway; this is transport.
package provcore

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

// WithRetry wraps an SDK call with bounded exponential backoff: up to 5
// attempts, 0.5→8s delays (doubling each attempt), Retry-After honored,
// 429 + 529 + 5xx retried. The SDK's own retry is disabled by the adapters
// (WithMaxRetries(0)) so this loop is the only one.
func WithRetry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	const maxAttempts = 5
	for attempt := 1; ; attempt++ {
		val, err := fn()
		if err == nil {
			return val, nil
		}
		// Vendor-agnostic: match Anthropic AND OpenAI (Responses) API errors, so
		// both lanes retry. A type-assert on one SDK's error alone left the
		// other's 429/5xx unretried.
		code, hdr, ok := APIErrorInfo(err)
		if !ok {
			return val, err // transport / non-API error — don't retry
		}
		if !IsRetryable(code) || attempt >= maxAttempts {
			return val, err
		}

		delay := Backoff(attempt)
		if hdr != nil {
			if ra := RetryAfter(hdr); ra > 0 {
				delay = ra
			}
		}
		if ctx.Err() != nil {
			return val, ctx.Err()
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return val, ctx.Err()
		}
	}
}

// StreamWithRetry is WithRetry's streaming twin — the ONE emitted-aware
// stream retry policy, shared by every native adapter (each previously ran
// its own copy of this loop with diverging attempt counts and no Retry-After).
// once runs a single streaming attempt and reports whether any content was
// forwarded to the caller's handler; a stream can't resume mid-flight, so a
// retry happens ONLY when nothing was emitted yet — retrying after partial
// output would duplicate it for the caller. Transient API failures (429/529/
// 5xx via the registered extractors) retry up to 5 attempts with Backoff,
// honoring Retry-After when the vendor's error carries a header. The failing
// attempt's response is returned alongside the error so partial usage still
// reaches the meter.
func StreamWithRetry(ctx context.Context, once func() (wire.Response, bool, error)) (wire.Response, error) {
	const maxAttempts = 5
	for attempt := 1; ; attempt++ {
		resp, emitted, err := once()
		if err == nil {
			return resp, nil
		}
		code, hdr, isAPI := APIErrorInfo(err)
		if emitted || ctx.Err() != nil || !isAPI || !IsRetryable(code) || attempt >= maxAttempts {
			return resp, err
		}
		delay := Backoff(attempt)
		if hdr != nil {
			if ra := RetryAfter(hdr); ra > 0 {
				delay = ra
			}
		}
		if serr := retrySleep(ctx, delay); serr != nil {
			return resp, serr
		}
	}
}

// retrySleep waits out one backoff, returning ctx.Err() on cancel. A package
// var so tests can observe the schedule without real sleeps.
var retrySleep = func(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrorInfoExtractor recognizes ONE vendor SDK's API error shape, returning
// its HTTP status + response header. Each adapter registers its own at init —
// this kernel imports NO vendor SDKs (registering here instead of type-
// asserting is what keeps provcore, and everything that touches it, from
// hard-linking every vendor's client library).
type ErrorInfoExtractor func(error) (status int, header http.Header, ok bool)

var (
	extractorMu sync.RWMutex
	extractors  []ErrorInfoExtractor
)

// RegisterErrorInfo adds a vendor error extractor (called from adapter init).
func RegisterErrorInfo(fn ErrorInfoExtractor) {
	extractorMu.Lock()
	defer extractorMu.Unlock()
	extractors = append(extractors, fn)
}

// APIErrorInfo extracts the HTTP status + response header from a provider SDK
// error via the registered extractors. ok is false for transport/non-API
// errors (not retryable).
func APIErrorInfo(err error) (status int, header http.Header, ok bool) {
	extractorMu.RLock()
	defer extractorMu.RUnlock()
	for _, fn := range extractors {
		if status, header, ok = fn(err); ok {
			return status, header, true
		}
	}
	return 0, nil, false
}

// IsRetryable reports whether an HTTP status is worth another attempt: 429
// (rate limit), 529 (Anthropic overloaded), and any 5xx. 4xx (bad request/
// auth) is not.
func IsRetryable(code int) bool {
	return code == http.StatusTooManyRequests || code == 529 || (code >= 500 && code <= 599)
}

// RetryAfter parses a Retry-After header — the seconds form or the HTTP-date
// form (RFC 9110 allows both) — returning 0 when absent, unparseable, or
// already in the past.
func RetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("retry-after"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

// Backoff is the bounded exponential delay for one attempt: 0.5s, 1s, 2s, 4s…
// capped at 8s.
func Backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << (attempt - 1)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// LogToolInputMalformed reports a replayed tool_use block whose Input is not
// valid JSON — telemetry, never fatal. The event is marshaled (never string-
// interpolated, which produced invalid JSON whenever the error text carried a
// quote) and written to stderr, never stdout, so it can't corrupt the TUI.
func LogToolInputMalformed(provider string, err error) {
	line, merr := json.Marshal(map[string]string{
		"event":    "tool_input_malformed",
		"provider": provider,
		"error":    err.Error(),
	})
	if merr != nil {
		return
	}
	fmt.Fprintln(os.Stderr, string(line))
}

// NewTurnHTTPClient returns the HTTP client used for provider turn calls.
//
// Deliberately NO http.Client.Timeout: that caps the ENTIRE exchange
// including reading a streamed body, which decapitates exactly the turns the
// escalation ladder exists for (frontier xhigh-reasoning turns, 1M-context
// plan synthesis) at the cap with partial usage billed. Turn lifetime is
// bounded by the request context (client cancel, server request timeout);
// the transport bounds only connection setup and time-to-first-byte.
func NewTurnHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 15 * time.Second,
			// Headers arrive immediately on streamed calls; non-streamed
			// vendor calls can legitimately think for minutes before the
			// first byte, so this is a generous stall bound, not a turn cap.
			ResponseHeaderTimeout: 10 * time.Minute,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// WebSearchToolName is the CLI's web_search FUNCTION tool — adapters with a
// native in-request search swap it for the vendor built-in.
const WebSearchToolName = "web_search"

// WebSearchSystemPrompt is the system prompt for every adapter's web-search
// side channel (a research assistant that cites sources) — hoisted here so
// the three vendors' prompts can never drift apart.
const WebSearchSystemPrompt = `You are a web research assistant for a coding agent. Use web search to answer the
request accurately and concisely, and cite source URLs inline. If the request is to read a specific URL,
summarize the content relevant to the agent's task. Prefer authoritative, current sources.`

// SplitWebSearchTool returns tools without the web_search function def, plus
// whether it was present. Copies on removal — the caller's shared slice is
// never mutated.
func SplitWebSearchTool(tools []wire.ToolDef) ([]wire.ToolDef, bool) {
	for i, t := range tools {
		if t.Name == WebSearchToolName {
			rest := make([]wire.ToolDef, 0, len(tools)-1)
			rest = append(rest, tools[:i]...)
			rest = append(rest, tools[i+1:]...)
			return rest, true
		}
	}
	return tools, false
}

// IsOverflowMessage reports whether a backend error message is a
// context-length rejection (input + max_tokens exceeds the served window).
// Covers the OpenAI-compat phrasings and Anthropic's ("prompt is too long:
// N tokens > M maximum"), so one classifier serves every adapter.
func IsOverflowMessage(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "maximum context length") ||
		strings.Contains(m, "longer than the maximum") ||
		strings.Contains(m, "reduce the length of the messages") ||
		strings.Contains(m, "prompt is too long") ||
		strings.Contains(m, "exceeds the maximum") ||
		strings.Contains(m, "context_length_exceeded")
}

// ContextOverflowError is a context-window overflow from ANY backend — the
// prompt + reserved output exceeds the served window. It is the signal the
// CLI watches for to compact-and-retry the turn rather than fail it.
type ContextOverflowError struct {
	Backend string // which lane overflowed ("anthropic" | "openai" | "fireworks" | …)
	Message string
}

func (e *ContextOverflowError) Error() string {
	return fmt.Sprintf("context overflow (%s): %s", e.Backend, e.Message)
}
