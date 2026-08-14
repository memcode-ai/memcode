package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/memcode-ai/memcode/internal/channels"
)

func msg(content, chanID, authorID, username string, bot bool) *discordgo.MessageCreate {
	// A guild message by default (GuildID set) so the parse tests aren't also
	// exercising DM detection; gating is covered separately below.
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:        "m1",
		GuildID:   "g1",
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
		{"stable id, not username", msg("do it", "c1", "u7", "tim", false), "self", true, "c1", "u7", "do it"},
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
			want := channels.Inbound{Channel: "discord", Conversation: tt.wantConvo, Principal: tt.wantPrincipal, Text: tt.wantText, MessageID: "m1"}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestGatingSignals(t *testing.T) {
	const self = "botself"

	// DM (no guild) → IsDirect, not gated on mention.
	dm := &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "m1", ChannelID: "c1", Content: "hi", Author: &discordgo.User{ID: "u7"},
	}}
	if inb, _ := toInbound(dm, self); !inb.IsDirect || inb.Mentioned {
		t.Errorf("DM: IsDirect=%v Mentioned=%v, want true/false", inb.IsDirect, inb.Mentioned)
	}

	// Guild message, no mention → not direct, not mentioned.
	plain := msg("hello", "c1", "u7", "", false)
	if inb, _ := toInbound(plain, self); inb.IsDirect || inb.Mentioned {
		t.Errorf("guild plain: IsDirect=%v Mentioned=%v, want false/false", inb.IsDirect, inb.Mentioned)
	}

	// Guild message mentioning the bot → mentioned.
	mentioned := msg("hey do it", "c1", "u7", "", false)
	mentioned.Mentions = []*discordgo.User{{ID: self}}
	if inb, _ := toInbound(mentioned, self); !inb.Mentioned {
		t.Error("guild mention not detected")
	}

	// Guild reply to one of the bot's messages → mentioned.
	reply := msg("thanks", "c1", "u7", "", false)
	reply.ReferencedMessage = &discordgo.Message{Author: &discordgo.User{ID: self}}
	if inb, _ := toInbound(reply, self); !inb.Mentioned {
		t.Error("reply-to-bot not treated as a mention")
	}
}
