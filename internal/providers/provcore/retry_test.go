package provcore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

// fakeAPIErr is a synthetic vendor API error the test extractor recognizes.
type fakeAPIErr struct {
	status int
	header http.Header
}

func (e *fakeAPIErr) Error() string { return fmt.Sprintf("fake api error %d", e.status) }

func init() {
	RegisterErrorInfo(func(err error) (int, http.Header, bool) {
		var fe *fakeAPIErr
		if errors.As(err, &fe) {
			return fe.status, fe.header, true
		}
		return 0, nil, false
	})
}

// stubSleep replaces the retry sleep with a delay recorder for the test's
// lifetime, so the schedule is observable without real waiting.
func stubSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var delays []time.Duration
	orig := retrySleep
	retrySleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return ctx.Err()
	}
	t.Cleanup(func() { retrySleep = orig })
	return &delays
}

// A transient 429 before any emit retries and the call succeeds.
func TestStreamWithRetryRetriesTransient(t *testing.T) {
	stubSleep(t)
	calls := 0
	resp, err := StreamWithRetry(context.Background(), func() (wire.Response, bool, error) {
		calls++
		if calls < 3 {
			return wire.Response{}, false, &fakeAPIErr{status: 429}
		}
		return wire.Response{StopReason: "end_turn"}, false, nil
	})
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("want success after retries, got %v / %+v", err, resp)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

// Once content was emitted a stream must NEVER retry — a re-run would
// duplicate output for the caller. The failing attempt's partial response
// (usage) surfaces with the error.
func TestStreamWithRetryStopsAfterEmit(t *testing.T) {
	calls := 0
	partial := wire.Response{InputTokens: 7, OutputTokens: 2}
	resp, err := StreamWithRetry(context.Background(), func() (wire.Response, bool, error) {
		calls++
		return partial, true, &fakeAPIErr{status: 500}
	})
	if err == nil || calls != 1 {
		t.Fatalf("emitted stream must fail without retry: err=%v calls=%d", err, calls)
	}
	if resp.InputTokens != 7 || resp.OutputTokens != 2 {
		t.Fatalf("partial usage lost on the error path: %+v", resp)
	}
}

// Non-API (transport) errors and non-retryable statuses fail immediately.
func TestStreamWithRetryPermanentFailures(t *testing.T) {
	for name, failure := range map[string]error{
		"transport":   errors.New("connection refused"),
		"bad request": &fakeAPIErr{status: 400},
	} {
		calls := 0
		_, err := StreamWithRetry(context.Background(), func() (wire.Response, bool, error) {
			calls++
			return wire.Response{}, false, failure
		})
		if err == nil || calls != 1 {
			t.Fatalf("%s: want one failing attempt, got err=%v calls=%d", name, err, calls)
		}
	}
}

// The attempt cap bounds a persistently failing stream at 5 attempts.
func TestStreamWithRetryAttemptCap(t *testing.T) {
	stubSleep(t)
	calls := 0
	_, err := StreamWithRetry(context.Background(), func() (wire.Response, bool, error) {
		calls++
		return wire.Response{}, false, &fakeAPIErr{status: 503}
	})
	if err == nil || calls != 5 {
		t.Fatalf("want 5 bounded attempts, got err=%v calls=%d", err, calls)
	}
}

// Retry-After on the vendor error's header overrides the backoff schedule.
func TestStreamWithRetryHonorsRetryAfter(t *testing.T) {
	delays := stubSleep(t)
	hdr := http.Header{}
	hdr.Set("Retry-After", "42")
	calls := 0
	_, err := StreamWithRetry(context.Background(), func() (wire.Response, bool, error) {
		calls++
		if calls == 1 {
			return wire.Response{}, false, &fakeAPIErr{status: 429, header: hdr}
		}
		return wire.Response{StopReason: "end_turn"}, false, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("want success on attempt 2, got err=%v calls=%d", err, calls)
	}
	// Backoff for attempt 1 is 500ms; the header's 42s must win.
	if len(*delays) != 1 || (*delays)[0] != 42*time.Second {
		t.Fatalf("Retry-After not honored: delays = %v", *delays)
	}
}

// RetryAfter parses both wire forms: delta-seconds and HTTP-date.
func TestRetryAfterForms(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "3")
	if d := RetryAfter(h); d != 3*time.Second {
		t.Fatalf("seconds form = %v, want 3s", d)
	}
	h.Set("Retry-After", time.Now().Add(5*time.Second).UTC().Format(http.TimeFormat))
	if d := RetryAfter(h); d <= 0 || d > 5*time.Second {
		t.Fatalf("HTTP-date form = %v, want (0, 5s]", d)
	}
	// A date in the past (or garbage) never produces a delay.
	h.Set("Retry-After", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat))
	if d := RetryAfter(h); d != 0 {
		t.Fatalf("past HTTP-date = %v, want 0", d)
	}
	h.Set("Retry-After", "soonish")
	if d := RetryAfter(h); d != 0 {
		t.Fatalf("garbage = %v, want 0", d)
	}
}
