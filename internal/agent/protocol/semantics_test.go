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
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// The channel boundary, pinned.
//
// assistant_delta is what the MODEL said; display is how the terminal drew it.
// They were one channel until 2026-09-12, and every programmatic consumer of
// this protocol silently received rendered output — ANSI, tool chrome, routing
// lines — wherever it asked for the assistant's words. The code reviewer built
// on that and failed six runs reporting a formatting problem it did not have.
//
// A regression here is invisible: everything still compiles, every event still
// arrives, and consumers quietly start acting on chrome again. So the semantics
// get a test rather than a comment.

// toolThenTalkProvider produces the shape that exposed the bug: a turn that runs
// a tool first (which the CLI RENDERS) and only then says something.
type toolThenTalkProvider struct{ calls int }

// sideCall reports whether this request is one of the judges the session fires
// alongside a turn (routing, task-shape) rather than the conversation itself. A
// scripted provider that counts them scripts the wrong call: the judge eats the
// first response and the main loop gets the second.
func sideCall(r wire.Request) bool { return r.Mode != "chat" }

func blandReply() wire.Response {
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{wire.TextBlock("n/a")}, OutputTokens: 1}
}

func (p *toolThenTalkProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	if sideCall(r) {
		return blandReply(), nil
	}
	p.calls++
	if p.calls == 1 {
		return wire.Response{
			StopReason: "tool_use",
			Blocks: []wire.Block{{
				Type: "tool_use", ID: "t1", Name: "bash",
				Input: json.RawMessage(`{"command":"echo hello"}`),
			}},
			OutputTokens: 4,
		}, nil
	}
	return wire.Response{
		StopReason:   "end_turn",
		Blocks:       []wire.Block{wire.TextBlock("I ran the command and it printed hello.")},
		OutputTokens: 6,
	}, nil
}

// silentProvider never says anything: every iteration is a tool call and then it
// stops. This is the case that used to leave result.text empty and send the
// consumer scavenging through whatever else it had captured.
type silentProvider struct{ calls int }

func (p *silentProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	if sideCall(r) {
		return blandReply(), nil
	}
	p.calls++
	if p.calls == 1 {
		return wire.Response{
			StopReason: "tool_use",
			Blocks: []wire.Block{{
				Type: "tool_use", ID: "t1", Name: "bash",
				Input: json.RawMessage(`{"command":"echo hello"}`),
			}},
			OutputTokens: 4,
		}, nil
	}
	return wire.Response{StopReason: "end_turn", Blocks: nil, OutputTokens: 1}, nil
}

type captured struct {
	assistant []string
	display   []string
	toolCalls []string
	result    wire.ResultData
	sawResult bool
}

// drive runs one turn against a provider and collects the protocol stream.
func drive(t *testing.T, prov provider.ModelProvider) captured {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sess := runtime.New(st, llm.NewRunner(prov), t.TempDir(), catalog.ModelSonnet,
		permissions.ModeAllowAll, io.Discard)

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
		send(wire.MsgInitialize, wire.InitializeData{Mode: "allow-all"})
		send(wire.MsgUserTurn, wire.UserTurnData{Text: "run echo hello"})
	}()

	var got captured
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var env wire.Envelope
		if json.Unmarshal(sc.Bytes(), &env) != nil {
			t.Fatalf("non-JSON on protocol stdout: %q", sc.Text())
		}
		switch env.Type {
		case wire.MsgAssistantDelta:
			var d wire.AssistantDeltaData
			_ = json.Unmarshal(env.Data, &d)
			got.assistant = append(got.assistant, d.Text)
		case wire.MsgDisplay:
			var d wire.DisplayData
			_ = json.Unmarshal(env.Data, &d)
			got.display = append(got.display, d.Text)
		case wire.MsgToolCall:
			var d wire.ToolCallData
			_ = json.Unmarshal(env.Data, &d)
			got.toolCalls = append(got.toolCalls, d.Name)
		case wire.MsgResult:
			_ = json.Unmarshal(env.Data, &got.result)
			got.sawResult = true
		}
		if got.sawResult {
			break
		}
	}
	inW.Close()
	if !got.sawResult {
		t.Fatal("the turn never produced a result event")
	}
	return got
}

// chrome is the vocabulary of rendering: escape sequences and the glyphs the TUI
// draws around tool activity and routing.
func chrome(s string) string {
	for _, marker := range []string{"\x1b[", "⏺", "⇄", "↳", "○", "◇"} {
		if strings.Contains(s, marker) {
			return marker
		}
	}
	return ""
}

func TestAssistantDeltaCarriesOnlyWhatTheModelSaid(t *testing.T) {
	got := drive(t, &toolThenTalkProvider{})

	joined := strings.Join(got.assistant, "")
	if !strings.Contains(joined, "I ran the command and it printed hello.") {
		t.Errorf("the model's words are missing from assistant_delta: %q", joined)
	}
	for _, d := range got.assistant {
		if m := chrome(d); m != "" {
			t.Errorf("assistant_delta carries rendering (%q): %q", m, d)
		}
	}
	// The tool ran, so the terminal drew something. That belongs on display.
	if len(got.display) == 0 {
		t.Error("a turn that ran a tool rendered nothing to display")
	}
	if len(got.toolCalls) == 0 {
		t.Errorf("expected a tool_call event for the bash call; display was: %q", got.display)
	}
}

func TestResultIsThisTurnsSemanticText(t *testing.T) {
	got := drive(t, &toolThenTalkProvider{})

	if !got.result.Spoke {
		t.Error("the model spoke; result.spoke should say so")
	}
	if got.result.Text != "I ran the command and it printed hello." {
		t.Errorf("result.text = %q, want exactly what the model said", got.result.Text)
	}
	if m := chrome(got.result.Text); m != "" {
		t.Errorf("result.text carries rendering (%q): %q", m, got.result.Text)
	}
}

// The case the reviewer hit. A turn can legitimately end without the model
// saying anything, and the protocol has to say so out loud — otherwise an empty
// string is indistinguishable from a lost one, and consumers invent a fallback.
func TestASilentTurnReportsItselfAsSilent(t *testing.T) {
	got := drive(t, &silentProvider{})

	if got.result.Spoke {
		t.Error("the model never spoke; result.spoke must be false")
	}
	if got.result.Text != "" {
		t.Errorf("result.text = %q, want empty for a turn with no assistant text", got.result.Text)
	}
	for _, d := range got.assistant {
		if strings.TrimSpace(d) != "" {
			t.Errorf("a silent turn emitted assistant_delta: %q", d)
		}
	}
}
