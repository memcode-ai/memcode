package protocol

import (
	"bufio"
	"bytes"
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
	"github.com/memcode-ai/memcode/internal/todos"
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

// The runtime's optional diff/tool observer seams and the plan hook must surface as
// the structured protocol events a desktop/SDK client renders — with the runtime's
// internal todo statuses mapped to the wire vocabulary (active -> in_progress).
func TestObserverEmitsStructuredEvents(t *testing.T) {
	var buf bytes.Buffer
	d := &driver{out: json.NewEncoder(&buf)}

	d.Todos(todos.List{{Title: "build the thing", Status: todos.StatusActive}})
	d.EmitDiff("main.go", "go", "@@ -1 +1 @@\n-old\n+new", 1, 1, false)
	d.EmitTool("Write", "main.go", false)

	var sawTodos, sawDiff, sawToolCall, sawToolResult bool
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		var env wire.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			t.Fatalf("non-JSON on protocol stdout: %q", sc.Text())
		}
		switch env.Type {
		case wire.MsgTodos:
			var td wire.TodosData
			_ = json.Unmarshal(env.Data, &td)
			if len(td.Items) != 1 || td.Items[0].Status != "in_progress" || td.Items[0].Text != "build the thing" {
				t.Errorf("todos event = %+v, want one in_progress item", td.Items)
			}
			sawTodos = true
		case wire.MsgDiff:
			var dd wire.DiffData
			_ = json.Unmarshal(env.Data, &dd)
			if dd.Path != "main.go" || dd.Added != 1 || dd.Removed != 1 {
				t.Errorf("diff event = %+v, want main.go +1/-1", dd)
			}
			sawDiff = true
		case wire.MsgToolCall:
			var tc wire.ToolCallData
			_ = json.Unmarshal(env.Data, &tc)
			if tc.Name != "Write" || tc.Target != "main.go" {
				t.Errorf("tool_call event = %+v, want Write(main.go)", tc)
			}
			sawToolCall = true
		case wire.MsgToolResult:
			var tr wire.ToolResultData
			_ = json.Unmarshal(env.Data, &tr)
			if tr.Status != "ok" {
				t.Errorf("tool_result status = %q, want ok", tr.Status)
			}
			sawToolResult = true
		}
	}
	if !sawTodos || !sawDiff || !sawToolCall || !sawToolResult {
		t.Errorf("missing events: todos=%v diff=%v tool_call=%v tool_result=%v", sawTodos, sawDiff, sawToolCall, sawToolResult)
	}
}
