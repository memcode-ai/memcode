// Package discord is the gateway's Discord channel adapter. It uses the
// maintained bwmarrin/discordgo gateway client (a real-time websocket, unlike
// Telegram's long-poll) — the SDK is isolated here so it can't grow a second
// implementation elsewhere (guarded by TestVendorSDKsOnlyInTheirAdapters). The
// user creates their own bot in the Discord developer portal, enables the
// Message Content intent, and puts the token in the global .env as
// DISCORD_BOT_TOKEN.
package discord

import (
	"context"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/memcode-ai/memcode/internal/channels"
)

// discordMaxMessage is Discord's hard per-message character limit. Longer agent
// replies are split across several messages.
const discordMaxMessage = 2000

// Channel is a Discord bot connection.
type Channel struct {
	session *discordgo.Session
}

// New builds a Discord channel for the given bot token. It requests the message
// intents (Message Content is privileged — the user must enable it on the bot).
func New(token string) (*Channel, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentMessageContent
	return &Channel{session: s}, nil
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "discord" }

// Start opens the gateway websocket, forwards each user message as an Inbound,
// and blocks until ctx is cancelled. discordgo reconnects internally, so a
// dropped socket doesn't return an error and take the gateway down.
func (c *Channel) Start(ctx context.Context, inbound chan<- channels.Inbound) error {
	remove := c.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		self := ""
		if s.State != nil && s.State.User != nil {
			self = s.State.User.ID
		}
		inb, ok := toInbound(m, self)
		if !ok {
			return
		}
		select {
		case inbound <- inb:
		case <-ctx.Done():
		}
	})
	defer remove()

	if err := c.session.Open(); err != nil {
		return err
	}
	defer c.session.Close()

	<-ctx.Done()
	return ctx.Err()
}

// toInbound converts a Discord message-create event to a normalized Inbound,
// skipping our own messages, other bots, and empty content. selfID is the bot's
// own user id.
func toInbound(m *discordgo.MessageCreate, selfID string) (channels.Inbound, bool) {
	if m == nil || m.Message == nil || m.Author == nil {
		return channels.Inbound{}, false
	}
	if m.Author.ID == selfID || m.Author.Bot {
		return channels.Inbound{}, false
	}
	if strings.TrimSpace(m.Content) == "" {
		return channels.Inbound{}, false
	}
	// A message with no guild is a DM. In a guild the bot only acts when addressed:
	// mentioned in the mentions array, or a reply to one of its own messages.
	// Detection is structural (ids), never substring.
	isDirect := m.GuildID == ""
	mentioned := false
	for _, u := range m.Mentions {
		if u != nil && u.ID == selfID {
			mentioned = true
			break
		}
	}
	if !mentioned && m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil && m.ReferencedMessage.Author.ID == selfID {
		mentioned = true
	}
	// Principal is the stable user id (snowflake), never the mutable username, so
	// the allow-list authorizes on a stable identity.
	return channels.Inbound{
		Channel:      "discord",
		Conversation: m.ChannelID,
		Principal:    m.Author.ID,
		Text:         m.Content,
		MessageID:    m.ID,
		IsDirect:     isDirect,
		Mentioned:    mentioned,
	}, true
}

// Send posts a reply to a channel, splitting it with the shared chunker to
// respect Discord's per-message length limit.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	for _, part := range channels.Chunk(msg.Text, discordMaxMessage) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := c.session.ChannelMessageSend(conversation, part); err != nil {
			return err
		}
	}
	return nil
}
