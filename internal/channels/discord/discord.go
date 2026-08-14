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
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/memcode-ai/memcode/internal/channels"
)

// discordMaxMessage is Discord's hard per-message character limit. Longer agent
// replies are split across several messages.
const discordMaxMessage = 2000

// Channel is a Discord bot connection.
type Channel struct {
	session  *discordgo.Session
	mediaDir string // media spool; "" disables attachment downloads
	dl       *http.Client
}

// New builds a Discord channel for the given bot token. It requests the message
// intents (Message Content is privileged — the user must enable it on the bot).
// mediaDir is the gateway media spool attachments are downloaded into; ""
// disables attachment handling.
func New(token, mediaDir string) (*Channel, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentMessageContent
	return &Channel{session: s, mediaDir: mediaDir, dl: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "discord" }

// Start opens the gateway websocket, forwards each user message as an Inbound,
// and blocks until ctx is cancelled. discordgo reconnects internally, so a
// dropped socket doesn't return an error and take the gateway down.
func (c *Channel) Start(ctx context.Context, sink channels.Sink) error {
	remove := c.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		self := ""
		if s.State != nil && s.State.User != nil {
			self = s.State.User.ID
		}
		inb, ok := toInbound(m, self)
		if !ok {
			return
		}
		inb.Attachments = c.download(ctx, m.Attachments)
		// The Discord gateway has no per-message replay, so a Deliver failure can't
		// be retried — the durable record is best-effort here.
		_ = sink.Deliver(ctx, inb)
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
	if strings.TrimSpace(m.Content) == "" && len(m.Attachments) == 0 {
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

// download fetches message attachments (CDN URLs) into the media spool,
// best-effort: a failed download drops that attachment, the message still flows.
func (c *Channel) download(ctx context.Context, atts []*discordgo.MessageAttachment) []channels.Attachment {
	if c.mediaDir == "" || len(atts) == 0 {
		return nil
	}
	var out []channels.Attachment
	for _, a := range atts {
		if a == nil || a.URL == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			continue
		}
		resp, err := c.dl.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode/100 != 2 {
			resp.Body.Close()
			continue
		}
		att, err := channels.SaveToSpool(c.mediaDir, resp.Body, a.ContentType, a.Filename)
		resp.Body.Close()
		if err != nil {
			continue
		}
		out = append(out, att)
	}
	return out
}

// Send posts a reply to a channel, splitting it with the shared chunker to
// respect Discord's per-message length limit. A synthesized voice rendition
// (VoicePath) is uploaded first as a file, best-effort — the text always follows.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	if msg.VoicePath != "" {
		if f, err := os.Open(msg.VoicePath); err == nil {
			_, _ = c.session.ChannelFileSend(conversation, "voice-reply.ogg", f)
			f.Close()
		}
	}
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
