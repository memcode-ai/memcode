package server

import (
	"context"
	"io"
	"testing"
	"time"

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

func TestConversationSessionStable(t *testing.T) {
	a := conversationSession("telegram", "42", "")
	if a != conversationSession("telegram", "42", "") {
		t.Error("session id must be deterministic for a conversation")
	}
	if a == conversationSession("telegram", "43", "") || a == conversationSession("discord", "42", "") {
		t.Error("distinct conversations must get distinct session ids")
	}
	if a == conversationSession("telegram", "42", "coder") {
		t.Error("a persona must get its own session, not the default persona's transcript")
	}
	if conversationSession("telegram", "42", "coder") != conversationSession("telegram", "42", "coder") {
		t.Error("a persona's session id must be deterministic")
	}
	if len(a) < 6 || a[:5] != "sess_" {
		t.Errorf("session id must match the sess_ shape, got %q", a)
	}
}

// The Trusted bypass (schedules, signature-verified webhooks) must NOT weaken the
// allow-list for ordinary chat: an untrusted message from an unlisted principal is
// still dropped, while a Trusted producer enqueues regardless.
func TestTrustedBypassIsScoped(t *testing.T) {
	ctx := context.Background()
	gw, err := state.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	rt := &runtime{
		gw:       gw,
		settings: gwconfig.Settings{Channels: map[string]gwconfig.Channel{"telegram": {AllowFrom: []string{"me"}}}},
		byName:   map[string]replySender{"telegram": fakeSender{}},
		out:      io.Discard,
		notify:   make(chan struct{}, 1),
	}

	// Untrusted chat from an unlisted principal → dropped (allow-list still gates).
	_ = rt.Deliver(ctx, channels.Inbound{Channel: "telegram", Conversation: "42", Principal: "attacker", Text: "rm -rf /", MessageID: "m1", IsDirect: true})
	if p, _ := gw.Pending(ctx); len(p) != 0 {
		t.Fatalf("unlisted chat principal must not enqueue, got %+v", p)
	}

	// A Trusted producer (schedule/github) bypasses the allow-list by design.
	_ = rt.Deliver(ctx, channels.Inbound{Channel: "telegram", Conversation: "42", Principal: "schedule:x", Text: "do", MessageID: "m2", Trusted: true})
	if p, _ := gw.Pending(ctx); len(p) != 1 {
		t.Fatalf("trusted inbound should enqueue despite the allow-list, got %d", len(p))
	}
}

// Editing the schedules section takes effect without a restart: applySchedules
// rebuilds the runner from the current settings on each call, and clearing the
// section stops it.
func TestApplySchedulesRebuilds(t *testing.T) {
	ctx := context.Background()
	rt := &runtime{
		settings: gwconfig.Settings{Schedules: []gwconfig.Schedule{
			{Name: "a", Every: "1h", Task: "t", DeliverTo: "telegram:1"},
		}},
		out: io.Discard,
	}
	rt.applySchedules(ctx)
	if rt.sched == nil || len(rt.sched.Entries()) != 1 {
		t.Fatal("want 1 live schedule entry after first apply")
	}
	rt.settings.Schedules = append(rt.settings.Schedules,
		gwconfig.Schedule{Name: "b", Cron: "0 9 * * 1-5", Task: "t2", DeliverTo: "telegram:1"})
	rt.applySchedules(ctx)
	if got := len(rt.sched.Entries()); got != 2 {
		t.Fatalf("want 2 live entries after reload, got %d", got)
	}
	rt.settings.Schedules = nil
	rt.applySchedules(ctx)
	if rt.sched != nil {
		t.Fatal("clearing schedules must stop the runner")
	}
}

// Disabled schedules don't run; a future one-shot arms a timer instead of a
// cron entry; both come and go across re-applies.
func TestApplySchedulesDisabledAndOneShot(t *testing.T) {
	ctx := context.Background()
	rt := &runtime{
		settings: gwconfig.Settings{Schedules: []gwconfig.Schedule{
			{Name: "off", Every: "1h", Task: "t", DeliverTo: "telegram:1", Disabled: true},
			{Name: "later", At: time.Now().Add(time.Hour).Format(time.RFC3339), Task: "t", DeliverTo: "telegram:1"},
		}},
		out: io.Discard,
	}
	rt.applySchedules(ctx)
	if rt.sched != nil {
		t.Error("a disabled schedule must not create a cron entry")
	}
	if len(rt.timers) != 1 {
		t.Fatalf("want 1 armed one-shot timer, got %d", len(rt.timers))
	}
	rt.settings.Schedules = nil
	rt.applySchedules(ctx)
	if len(rt.timers) != 0 {
		t.Error("clearing schedules must drop armed one-shots")
	}
}
