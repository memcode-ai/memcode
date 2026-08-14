package server

import (
	"context"
	"io"
	"testing"

	"github.com/memcode-ai/memcode/internal/channels"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/gateway/state"
)

type fakeSender struct{}

func (fakeSender) Send(context.Context, string, channels.Outbound) error { return nil }

func TestScheduleSpec(t *testing.T) {
	cases := []struct {
		every, cron string
		want        string
		ok          bool
	}{
		{"24h", "", "@every 24h", true},
		{"", "0 9 * * 1-5", "0 9 * * 1-5", true},
		{"", "", "", false},             // neither set
		{"24h", "0 9 * * *", "", false}, // both set
	}
	for _, c := range cases {
		got, ok := scheduleSpec(gwconfig.Schedule{Every: c.every, Cron: c.cron})
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("scheduleSpec(every=%q,cron=%q) = (%q,%v), want (%q,%v)", c.every, c.cron, got, ok, c.want, c.ok)
		}
	}
}

// A fired schedule enqueues a Trusted inbound into the durable inbox, so it flows
// through the same worker/reply path as a chat message.
func TestFireScheduleEnqueues(t *testing.T) {
	ctx := context.Background()
	gw, err := state.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	rt := &runtime{
		gw:       gw,
		settings: gwconfig.Settings{}, // no allow-list needed — scheduled inbound is Trusted
		byName:   map[string]replySender{"telegram": fakeSender{}},
		out:      io.Discard,
		notify:   make(chan struct{}, 1),
	}

	rt.fireSchedule(ctx, gwconfig.Schedule{Name: "standup", Task: "summarize commits"}, "telegram", "42")

	pending, err := gw.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending inbox item, got %d", len(pending))
	}
	if it := pending[0]; it.Channel != "telegram" || it.Conversation != "42" || it.Text != "summarize commits" {
		t.Errorf("unexpected inbox item %+v", it)
	}
}

// A schedule whose deliver_to channel isn't configured is dropped, not enqueued.
func TestFireScheduleUnknownChannelDropped(t *testing.T) {
	ctx := context.Background()
	gw, err := state.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	rt := &runtime{
		gw:       gw,
		settings: gwconfig.Settings{},
		byName:   map[string]replySender{}, // telegram not configured
		out:      io.Discard,
		notify:   make(chan struct{}, 1),
	}
	rt.fireSchedule(ctx, gwconfig.Schedule{Name: "x", Task: "do"}, "telegram", "42")

	if pending, _ := gw.Pending(ctx); len(pending) != 0 {
		t.Errorf("a schedule to an unconfigured channel must not enqueue, got %+v", pending)
	}
}
