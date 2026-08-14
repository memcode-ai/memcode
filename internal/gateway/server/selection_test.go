package server

import (
	"context"
	"io"
	"testing"

	"github.com/memcode-ai/memcode/internal/channels"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/gateway/state"
)

type capturingSender struct{ last string }

func (c *capturingSender) Send(_ context.Context, _ string, o channels.Outbound) error {
	c.last = o.Text
	return nil
}

func TestHandleCommandAndSelection(t *testing.T) {
	ctx := context.Background()
	gw, err := state.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	rt := &runtime{
		gw: gw,
		settings: gwconfig.Settings{
			Channels:       map[string]gwconfig.Channel{"telegram": {Agent: "personal"}},
			Agents:         map[string]gwconfig.Persona{"personal": {}, "coder": {}},
			Projects:       map[string]gwconfig.Project{"memcode": {Path: t.TempDir(), Enabled: true}},
			DefaultProject: "memcode",
		},
		byName: map[string]replySender{"telegram": &capturingSender{}},
		out:    io.Discard,
		notify: make(chan struct{}, 1),
	}

	// Unknown agent is recognized as a command but changes nothing.
	if !rt.handleCommand(ctx, channels.Inbound{Channel: "telegram", Conversation: "1", Text: "/agent nope"}) {
		t.Fatal("/agent should be recognized as a command")
	}
	if a, _, _ := gw.Conversation(ctx, "telegram", "1"); a != "" {
		t.Errorf("unknown agent must not change selection, got %q", a)
	}

	// Valid /agent switches the conversation's persona.
	rt.handleCommand(ctx, channels.Inbound{Channel: "telegram", Conversation: "1", Text: "/agent coder"})
	if a, _, _ := gw.Conversation(ctx, "telegram", "1"); a != "coder" {
		t.Errorf("agent = %q, want coder", a)
	}

	// resolveSelection: the conversation override wins for agent; project falls
	// back to the gateway default.
	agent, project := rt.resolveSelection(ctx, "telegram", "1")
	if agent != "coder" || project != "memcode" {
		t.Errorf("resolveSelection = (%q,%q), want (coder,memcode)", agent, project)
	}

	// A normal task starting with a word is not a command.
	if rt.handleCommand(ctx, channels.Inbound{Channel: "telegram", Conversation: "1", Text: "fix the bug"}) {
		t.Error("a normal task must not be treated as a command")
	}
}
