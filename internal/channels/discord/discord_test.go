package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/memcode-ai/memcode/internal/channels"
)

func msg(content, chanID, authorID, username string, bot bool) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: chanID,
		Content:   content,
		Author:    &discordgo.User{ID: authorID, Username: username, Bot: bot},
	}}
}

func TestToInbound(t *testing.T) {
	tests := []struct {
		name          string
		m             *discordgo.MessageCreate
		self          string
		wantOK        bool
		wantConvo     string
		wantPrincipal string
		wantText      string
	}{
		{"username", msg("do it", "c1", "u7", "tim", false), "self", true, "c1", "@tim", "do it"},
		{"no username uses id", msg("hey", "c2", "u7", "", false), "self", true, "c2", "u7", "hey"},
		{"own message skipped", msg("hi", "c1", "self", "me", false), "self", false, "", "", ""},
		{"other bot skipped", msg("hi", "c1", "u9", "botto", true), "self", false, "", "", ""},
		{"empty content skipped", msg("   ", "c1", "u7", "tim", false), "self", false, "", "", ""},
		{"nil author skipped", &discordgo.MessageCreate{Message: &discordgo.Message{ChannelID: "c1", Content: "hi"}}, "self", false, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toInbound(tt.m, tt.self)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			want := channels.Inbound{Channel: "discord", Conversation: tt.wantConvo, Principal: tt.wantPrincipal, Text: tt.wantText}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestChunk(t *testing.T) {
	// Short strings pass through as one piece.
	if got := chunk("hello", 2000); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("short: got %v", got)
	}
	// Empty string still yields one (empty) piece.
	if got := chunk("", 2000); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty: got %v", got)
	}
	// Over-limit input splits into pieces each within the limit.
	long := strings.Repeat("a", 4500)
	parts := chunk(long, 2000)
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(parts))
	}
	total := 0
	for _, p := range parts {
		if len([]rune(p)) > 2000 {
			t.Errorf("part exceeds limit: %d", len([]rune(p)))
		}
		total += len([]rune(p))
	}
	if total != 4500 {
		t.Errorf("lost content: total %d", total)
	}
	// Prefers a newline break near the limit over a hard cut.
	withNL := strings.Repeat("x", 1500) + "\n" + strings.Repeat("y", 1500)
	got := chunk(withNL, 2000)
	if len(got) != 2 || !strings.HasSuffix(got[0], "\n") {
		t.Errorf("newline break: got pieces %d, first ends nl=%v", len(got), strings.HasSuffix(got[0], "\n"))
	}
}
