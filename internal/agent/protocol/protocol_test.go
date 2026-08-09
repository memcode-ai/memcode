package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// fakeProvider answers every turn immediately with a one-line reply (no tools), so a
// user_turn drives a single round-trip we can observe on the protocol stream.
type fakeProvider struct{}

func (fakeProvider) Complete(_ context.Context, _ wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{wire.TextBlock("hello from the agent")}, OutputTokens: 5}, nil
}

func TestStreamJSONRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sess := runtime.New(st, llm.NewRunner(fakeProvider{}), t.TempDir(), catalog.ModelSonnet, permissions.ModeAllowAll, io.Discard)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		_ = Run(ctx, sess, inR, outW)
		outW.Close()
	}()

	// Drive inputs in a goroutine: io.Pipe is synchronous, and the driver emits
	// `initialized` to stdout before reading stdin, so sends and reads must overlap.
	go func() {
		enc := json.NewEncoder(inW)
		send := func(typ string, data any) {
			raw, _ := json.Marshal(data)
			_ = enc.Encode(wire.Envelope{Version: wire.StreamJSONVersion, Type: typ, Data: raw})
		}
		send(wire.MsgInitialize, wire.InitializeData{Mode: "allow-all"})
		send(wire.MsgUserTurn, wire.UserTurnData{Text: "hi"})
	}()

	// Read events until we see the turn result (then close stdin to end the session).
	sawInitialized, sawDelta, sawResult := false, false, false
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var env wire.Envelope
		if json.Unmarshal(sc.Bytes(), &env) != nil {
			t.Fatalf("non-JSON on protocol stdout: %q", sc.Text())
		}
		if env.Version != wire.StreamJSONVersion {
			t.Fatalf("envelope missing version: %s", sc.Text())
		}
		switch env.Type {
		case wire.MsgInitialized:
			sawInitialized = true
		case wire.MsgAssistantDelta:
			var d wire.AssistantDeltaData
			_ = json.Unmarshal(env.Data, &d)
			if strings.Contains(d.Text, "hello from the agent") {
				sawDelta = true
			}
		case wire.MsgResult:
			sawResult = true
		}
		if sawResult {
			break
		}
	}
	inW.Close()

	if !sawInitialized {
		t.Error("expected an initialized event")
	}
	if !sawDelta {
		t.Error("expected the assistant text as an assistant_delta event")
	}
	if !sawResult {
		t.Error("expected a result event for the turn")
	}
}

// TestInitializePinsTheSession proves a pin riding the initialize message lands on
// the runtime session — label AND window (resolved from the SDK catalog, exactly
// what the /model picker would have supplied) — and that an unknown label is
// skipped rather than pinned (the gateway would silently serve Automatic for it).
func TestInitializePinsTheSession(t *testing.T) {
	for _, tc := range []struct {
		name, pin, wantPin string
		wantWindow         int
	}{
		// haiku's catalog window (200K) differs from the session model's (sonnet,
		// 1M), so the window assertion proves the catalog lookup, not a default.
		{name: "known label", pin: "haiku", wantPin: "haiku", wantWindow: 200_000},
		{name: "unknown label", pin: "no-such-model", wantPin: "", wantWindow: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			sess := runtime.New(st, llm.NewRunner(fakeProvider{}), t.TempDir(), catalog.ModelSonnet, permissions.ModeAllowAll, io.Discard)

			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			go func() {
				_ = Run(ctx, sess, inR, outW)
				outW.Close()
			}()
			go func() {
				enc := json.NewEncoder(inW)
				send := func(typ string, data any) {
					raw, _ := json.Marshal(data)
					_ = enc.Encode(wire.Envelope{Version: wire.StreamJSONVersion, Type: typ, Data: raw})
				}
				send(wire.MsgInitialize, wire.InitializeData{Mode: "allow-all", Pin: tc.pin})
				send(wire.MsgUserTurn, wire.UserTurnData{Text: "hi"})
			}()

			// The result event is the sync point: by then the reader has processed the
			// initialize (it precedes the user_turn on stdin), so Pin() is settled.
			sc := bufio.NewScanner(outR)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			sawResult := false
			for sc.Scan() {
				var env wire.Envelope
				if json.Unmarshal(sc.Bytes(), &env) != nil {
					t.Fatalf("non-JSON on protocol stdout: %q", sc.Text())
				}
				if env.Type == wire.MsgResult {
					sawResult = true
					break
				}
			}
			inW.Close()
			if !sawResult {
				t.Fatal("expected a result event for the turn")
			}

			if got := sess.Pin(); got != tc.wantPin {
				t.Errorf("session pin = %q, want %q", got, tc.wantPin)
			}
			if tc.wantWindow > 0 {
				if got := sess.ContextWindow(); got != tc.wantWindow {
					t.Errorf("pinned context window = %d, want %d (the catalog window for %q)", got, tc.wantWindow, tc.pin)
				}
			}
		})
	}
}
