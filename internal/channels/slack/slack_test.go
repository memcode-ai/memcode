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
		{"plain user message", &slackevents.MessageEvent{User: "U7", Channel: "C1", Text: "do it"}, true, "C1", "U7", "do it"},
		{"bot message skipped", &slackevents.MessageEvent{User: "U7", Channel: "C1", Text: "hi", BotID: "B9"}, false, "", "", ""},
		{"subtype skipped", &slackevents.MessageEvent{User: "U7", Channel: "C1", Text: "hi", SubType: "message_changed"}, false, "", "", ""},
		{"empty text skipped", &slackevents.MessageEvent{User: "U7", Channel: "C1", Text: "  "}, false, "", "", ""},
		{"no user skipped", &slackevents.MessageEvent{Channel: "C1", Text: "hi"}, false, "", "", ""},
		{"nil skipped", nil, false, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toInbound(tt.me)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			want := channels.Inbound{Channel: "slack", Conversation: tt.wantConvo, Principal: tt.wantWho, Text: tt.wantText}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}
