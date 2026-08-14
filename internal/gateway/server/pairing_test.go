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

// An unknown DIRECT sender gets exactly one pairing code; repeat messages stay
// silent, and nothing is ever enqueued for an unauthorized sender.
func TestUnknownDMGetsPairingCodeOnce(t *testing.T) {
	ctx := context.Background()
	gw, err := state.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	sender := &capturingSender{}
	rt := &runtime{
		gw:       gw,
		settings: gwconfig.Settings{Channels: map[string]gwconfig.Channel{"telegram": {AllowFrom: []string{"me"}}}},
		byName:   map[string]replySender{"telegram": sender},
		out:      io.Discard,
		notify:   make(chan struct{}, 1),
	}

	_ = rt.Deliver(ctx, channels.Inbound{Channel: "telegram", Conversation: "9", Principal: "stranger", Text: "hi", MessageID: "m1", IsDirect: true})
	if !strings.Contains(sender.last, "pairing code") {
		t.Fatalf("unknown DM should be offered a pairing code, got %q", sender.last)
	}
	first := sender.last

	sender.last = ""
	_ = rt.Deliver(ctx, channels.Inbound{Channel: "telegram", Conversation: "9", Principal: "stranger", Text: "hello?", MessageID: "m2", IsDirect: true})
	if sender.last != "" {
		t.Errorf("repeat message from the same stranger must not re-send a code, got %q", sender.last)
	}

	// A group message from an unknown sender gets no code at all.
	sender.last = ""
	_ = rt.Deliver(ctx, channels.Inbound{Channel: "telegram", Conversation: "group", Principal: "stranger2", Text: "@bot do", MessageID: "m3", Mentioned: true})
	if sender.last != "" {
		t.Errorf("group messages must not trigger pairing, got %q", sender.last)
	}

	if p, _ := gw.Pending(ctx); len(p) != 0 {
		t.Errorf("unauthorized messages must never enqueue, got %+v", p)
	}

	// The pending request is listable and consumable.
	reqs, err := gw.PendingPairings(ctx, time.Now())
	if err != nil || len(reqs) != 1 {
		t.Fatalf("want 1 pending pairing, got %d (%v)", len(reqs), err)
	}
	if !strings.Contains(first, reqs[0].Code) {
		t.Errorf("reply %q should carry the recorded code %s", first, reqs[0].Code)
	}
	p, err := gw.TakePairing(ctx, strings.ToLower(reqs[0].Code), time.Now()) // case-insensitive
	if err != nil || p.Principal != "stranger" || p.Channel != "telegram" {
		t.Errorf("TakePairing = %+v, %v", p, err)
	}
	if reqs, _ := gw.PendingPairings(ctx, time.Now()); len(reqs) != 0 {
		t.Errorf("consumed pairing should be gone, got %+v", reqs)
	}
}

// The pending-request table is capped and requests expire.
func TestPairingCapAndExpiry(t *testing.T) {
	ctx := context.Background()
	gw, err := state.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	now := time.Now()
	for i := 0; i < state.PairingCap; i++ {
		code, _ := newPairCode()
		if _, _, err := gw.CreatePairing(ctx, "telegram", "u"+string(rune('a'+i)), "", code, now); err != nil {
			t.Fatal(err)
		}
	}
	code, _ := newPairCode()
	if _, _, err := gw.CreatePairing(ctx, "telegram", "one-too-many", "", code, now); err == nil {
		t.Error("request past the cap must be refused")
	}
	// After the TTL passes, old requests no longer count or list.
	later := now.Add(state.PairingTTL + time.Minute)
	if reqs, _ := gw.PendingPairings(ctx, later); len(reqs) != 0 {
		t.Errorf("expired requests must not list, got %d", len(reqs))
	}
	code, _ = newPairCode()
	if _, created, err := gw.CreatePairing(ctx, "telegram", "fresh", "", code, later); err != nil || !created {
		t.Errorf("expiry should free capacity: created=%v err=%v", created, err)
	}
}
