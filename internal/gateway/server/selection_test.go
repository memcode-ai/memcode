package server

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

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
			Agents:         map[string]gwconfig.Agent{"personal": {}, "coder": {}},
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

	// Valid /agent switches the conversation's agent.
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

// channels.<name>.projects narrows /project and the channel's effective default
// to the listed set — the grain that keeps a shared group channel from being
// pointed at any registered project.
func TestChannelProjectPolicy(t *testing.T) {
	ctx := context.Background()
	gw, err := state.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	sender := &capturingSender{}
	rt := &runtime{
		gw: gw,
		settings: gwconfig.Settings{
			Channels: map[string]gwconfig.Channel{"discord": {Projects: []string{"www"}}},
			Projects: map[string]gwconfig.Project{
				"memcode": {Path: t.TempDir(), Enabled: true},
				"www":     {Path: t.TempDir(), Enabled: true},
			},
			DefaultProject: "memcode",
		},
		byName: map[string]replySender{"discord": sender},
		out:    io.Discard,
		notify: make(chan struct{}, 1),
	}

	// /project to a registered-but-unlisted project is refused for this channel.
	if !rt.handleCommand(ctx, channels.Inbound{Channel: "discord", Conversation: "1", Text: "/project memcode"}) {
		t.Fatal("/project should be recognized as a command")
	}
	if _, p, _ := gw.Conversation(ctx, "discord", "1"); p != "" {
		t.Errorf("disallowed project must not change selection, got %q", p)
	}

	// /project to a listed project works.
	rt.handleCommand(ctx, channels.Inbound{Channel: "discord", Conversation: "1", Text: "/project www"})
	if _, p, _ := gw.Conversation(ctx, "discord", "1"); p != "www" {
		t.Errorf("allowed project selection = %q, want www", p)
	}

	// The gateway default is NOT in this channel's set, so a fresh conversation
	// resolves to the channel's first allowed project, never the gateway default.
	if _, p := rt.resolveSelection(ctx, "discord", "fresh"); p != "www" {
		t.Errorf("resolveSelection default = %q, want www (channel policy)", p)
	}
}

// A queued task whose snapshotted project is disabled or deleted before
// execution is REFUSED — never run in the gateway default root (P1 from the
// channels-baseline review).
func TestRunJobRefusesUnresolvableProject(t *testing.T) {
	ctx := context.Background()
	gw, err := state.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	sender := &capturingSender{}
	rt := &runtime{
		root: t.TempDir(),
		gw:   gw,
		settings: gwconfig.Settings{
			// The id is allowed on the channel, but the project itself is disabled.
			Projects: map[string]gwconfig.Project{"www": {Path: t.TempDir(), Enabled: false}},
		},
		mediaDir: t.TempDir(),
		byName:   map[string]replySender{"telegram": sender},
		out:      io.Discard,
		notify:   make(chan struct{}, 1),
	}
	it := state.Item{Channel: "telegram", MessageID: "m1", Conversation: "1", Principal: "me", Text: "do it", Project: "www"}
	if _, err := gw.Accept(ctx, it, time.Now()); err != nil {
		t.Fatal(err)
	}
	rt.runJob(ctx, it)
	if !strings.Contains(sender.last, "no longer available") {
		t.Fatalf("want refusal reply, got %q", sender.last)
	}
	// The refusal is durable: the item moved past pending without spawning.
	if p, _ := gw.Pending(ctx); len(p) != 0 {
		t.Errorf("item still pending: %+v", p)
	}
}
