package server

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/gateway/state"
)

type flakySender struct {
	err   error
	calls int
}

func (f *flakySender) Send(context.Context, string, channels.Outbound) error {
	f.calls++
	return f.err
}

// A finished job's reply is durable: a failing channel does not lose it or re-run
// the job. The item stays on the outbound queue and delivers once the channel
// recovers.
func TestDeliverReplySurvivesSendFailure(t *testing.T) {
	ctx := context.Background()
	gw, err := state.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	it := state.Item{Channel: "telegram", MessageID: "m1", Conversation: "42", Principal: "p", Text: "hi"}
	gw.Accept(ctx, it, time.Unix(1000, 0))
	if err := gw.SetReplied(ctx, "telegram", "m1", "the answer", ""); err != nil {
		t.Fatal(err)
	}

	failing := &flakySender{err: errors.New("channel down")}
	rt := &runtime{
		gw:       gw,
		settings: gwconfig.Settings{},
		byName:   map[string]replySender{"telegram": failing},
		out:      io.Discard,
		notify:   make(chan struct{}, 1),
	}

	rt.deliverReply(ctx, it, "the answer")
	if failing.calls != 3 {
		t.Errorf("want 3 in-process send attempts, got %d", failing.calls)
	}
	if replies, _ := gw.PendingReplies(ctx); len(replies) != 1 {
		t.Fatalf("a failed delivery must stay on the outbound queue, got %d", len(replies))
	}

	// Channel recovers: the reply delivers and the item clears.
	rt.byName["telegram"] = &flakySender{}
	rt.deliverReply(ctx, it, "the answer")
	if replies, _ := gw.PendingReplies(ctx); len(replies) != 0 {
		t.Errorf("a recovered delivery should clear the queue, got %d", len(replies))
	}
}
