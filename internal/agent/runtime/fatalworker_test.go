package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
	"google.golang.org/genai"
)

// apiErr builds a real provider API error, the way llm's fallback tests do — the
// terminal/retryable split is read off the HTTP status by the registered extractor.
func apiErr(code int, msg string) error {
	return fmt.Errorf("gemini stream: %w", &genai.APIError{Code: code, Message: msg})
}

// fatalWorkerProvider fails EVERY call with a 400 — the shape of the Gemini
// thought-signature bug, which fails identically no matter how often it is retried.
type fatalWorkerProvider struct{ calls int }

func (p *fatalWorkerProvider) Complete(context.Context, wire.Request) (wire.Response, error) {
	p.calls++
	return wire.Response{}, apiErr(400, "function call missing thoughtSignature")
}

// TestTerminalWorkerErrorArmsTheTurnFatal: a delegated worker that dies on a
// terminal error must arm turn.fatalErr — the loop reads that and ends the turn
// with the cause — rather than handing the model a retryable "agent failed".
//
// This is the 12-minute review-run regression: ~150 identical 400s, one every two
// seconds, because nothing could distinguish "try again" from "this can never work".
func TestTerminalWorkerErrorArmsTheTurnFatal(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, &fatalWorkerProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.turn = newTurnState()

	res := s.workerFailed("agent", apiErr(400, "function call missing thoughtSignature"))

	if s.turn.fatalErr == nil {
		t.Fatal("a terminal worker error must arm turn.fatalErr so the loop stops retrying")
	}
	// The cause survives to the surface: the loop returns this error, and the
	// runtime prints it — the user sees "gemini 400", not a silent 20-minute hang.
	if got := s.turn.fatalErr.Error(); !strings.Contains(got, "thoughtSignature") || !strings.Contains(got, "agent") {
		t.Errorf("fatal error must name the worker AND the cause, got %q", got)
	}
	// The model still gets told, so a turn that somehow continues isn't left blind.
	if len(res.blocks) == 0 {
		t.Error("the tool result must still be returned to the model")
	}
}

// TestTransientWorkerErrorDoesNotKillTheTurn: the flip side. A 429 or a network
// blip IS worth retrying, so it stays an ordinary tool error and the turn lives.
func TestTransientWorkerErrorDoesNotKillTheTurn(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, &fatalWorkerProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	for _, err := range []error{
		apiErr(429, "rate limited"),
		apiErr(503, "backend unavailable"),
		errors.New("connection reset by peer"),
	} {
		s.turn = newTurnState()
		s.workerFailed("explore", err)
		if s.turn.fatalErr != nil {
			t.Errorf("%v is retryable and must NOT kill the turn", err)
		}
	}
}
