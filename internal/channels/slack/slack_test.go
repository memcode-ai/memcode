package slack

import (
	"testing"

	"github.com/slack-go/slack/slackevents"

	"github.com/memcode-ai/memcode/internal/channels"
)

func TestToInbound(t *testing.T) {
	tests := []struct {
		name      string
		me        *slackevents.MessageEvent
		wantOK    bool
		wantConvo string
		wantWho   string
		wantText  string
	}{
		{"plain user message", &slackevents.MessageEvent{User: "U7", Channel: "C1", Text: "do it", TimeStamp: "ts1"}, true, "C1", "U7", "do it"},
		{"bot message skipped", &slackevents.MessageEvent{User: "U7", Channel: "C1", Text: "hi", BotID: "B9"}, false, "", "", ""},
		{"subtype skipped", &slackevents.MessageEvent{User: "U7", Channel: "C1", Text: "hi", SubType: "message_changed"}, false, "", "", ""},
		{"empty text skipped", &slackevents.MessageEvent{User: "U7", Channel: "C1", Text: "  "}, false, "", "", ""},
		{"no user skipped", &slackevents.MessageEvent{Channel: "C1", Text: "hi"}, false, "", "", ""},
		{"nil skipped", nil, false, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toInbound(tt.me, "BOT")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			want := channels.Inbound{Channel: "slack", Conversation: tt.wantConvo, Principal: tt.wantWho, Text: tt.wantText, MessageID: "ts1"}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestGatingSignals(t *testing.T) {
	// DM (channel_type im) → IsDirect.
	dm := &slackevents.MessageEvent{User: "U7", Channel: "D1", Text: "hi", TimeStamp: "1", ChannelType: "im"}
	if inb, _ := toInbound(dm, "BOT"); !inb.IsDirect || inb.Mentioned {
		t.Errorf("DM: IsDirect=%v Mentioned=%v, want true/false", inb.IsDirect, inb.Mentioned)
	}
	// Channel message, no mention → not direct, not mentioned.
	plain := &slackevents.MessageEvent{User: "U7", Channel: "C1", Text: "hello team", TimeStamp: "2", ChannelType: "channel"}
	if inb, _ := toInbound(plain, "BOT"); inb.IsDirect || inb.Mentioned {
		t.Errorf("channel plain: IsDirect=%v Mentioned=%v, want false/false", inb.IsDirect, inb.Mentioned)
	}
	// Channel message mentioning the bot → mentioned.
	mentioned := &slackevents.MessageEvent{User: "U7", Channel: "C1", Text: "<@BOT> do it", TimeStamp: "3", ChannelType: "channel"}
	if inb, _ := toInbound(mentioned, "BOT"); !inb.Mentioned {
		t.Error("channel mention not detected")
	}
	// Unknown bot id → can't detect a mention (safe: won't trigger in a channel).
	if inb, _ := toInbound(mentioned, ""); inb.Mentioned {
		t.Error("mention should not be detected without a known bot id")
	}
}
