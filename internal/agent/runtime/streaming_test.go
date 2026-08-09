package runtime

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/mood"
	"github.com/memcode-ai/memcode/internal/agent/room"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/todos"
	"github.com/memcode-ai/memcode/internal/wire"
)

// fakeStreamer implements ModelProvider + Streamer, emitting word-by-word deltas.
type fakeStreamer struct{ text string }

func (f fakeStreamer) Complete(context.Context, wire.Request) (wire.Response, error) {
	return wire.Response{
		Blocks:       []wire.Block{{Type: "text", Text: f.text}},
		InputTokens:  10,
		OutputTokens: 7,
	}, nil
}
func (f fakeStreamer) Stream(_ context.Context, _ wire.Request, h wire.StreamHandler) (wire.Response, error) {
	for _, w := range strings.Fields(f.text) {
		if h.Text != nil {
			h.Text(w + " ")
		}
	}
	if h.Usage != nil {
		h.Usage(10, 7) // authoritative output for this call
	}
	return wire.Response{
		Blocks:       []wire.Block{{Type: "text", Text: f.text}},
		InputTokens:  10,
		OutputTokens: 7,
	}, nil
}

type tokenObserver struct{ tokens []int }

func (o *tokenObserver) Routed(input.Route, string) {}
func (o *tokenObserver) QueueChanged([]string)      {}
func (o *tokenObserver) Busy(bool)                  {}
func (o *tokenObserver) Mood(mood.Reading)          {}
func (o *tokenObserver) Room(room.State)            {}
func (o *tokenObserver) Todos(todos.List)           {}
func (o *tokenObserver) Tokens(out int)             { o.tokens = append(o.tokens, out) }
func (o *tokenObserver) Raw(string)                 {}

// complete() is NON-streaming: one Complete call, the full reply rendered as a single block,
// and the ↓ token counter snapped to committedOut + the real output count.
func TestNonStreamingTextAndTokenBase(t *testing.T) {
	s := newTodoSession(t)
	var buf bytes.Buffer
	s.out = &buf
	obs := &tokenObserver{}
	s.observer = obs
	s.runner = llm.NewRunner(fakeStreamer{text: "Hello there world"})

	resp, err := s.complete(context.Background(), llm.MainLoop, wire.Request{}, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	// The full reply reached the output writer (as one block — no streamed deltas).
	if !strings.Contains(buf.String(), "Hello there world") {
		t.Fatalf("reply missing from output: %q", buf.String())
	}
	if resp.OutputTokens != 7 {
		t.Errorf("OutputTokens = %d, want 7", resp.OutputTokens)
	}
	// The ↓ counter snaps to committedOut(100) + OutputTokens(7) = 107.
	if len(obs.tokens) == 0 {
		t.Fatal("no token update reported")
	}
	if last := obs.tokens[len(obs.tokens)-1]; last != 107 {
		t.Errorf("final token count = %d, want 107", last)
	}
}
