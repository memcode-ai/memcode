// Package slack is the gateway's Slack channel adapter. It uses Socket Mode (an
// outbound websocket, no public inbound URL needed) via the slack-go SDK, kept
// isolated to this package (guarded by TestVendorSDKsOnlyInTheirAdapters). The
// user creates a Slack app with an app-level token (xapp-…, Socket Mode) and a
// bot token (xoxb-…), storing them in the global .env as SLACK_APP_TOKEN
// and SLACK_BOT_TOKEN.
package slack

import (
	"context"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/memcode-ai/memcode/internal/channels"
)

// Channel is a Slack Socket Mode connection.
type Channel struct {
	api    *slack.Client
	client *socketmode.Client
	botID  string // this bot's own user id (U…), for mention detection
}

// New builds a Slack channel from an app-level token (Socket Mode) and a bot
// token (Web API for posting replies).
func New(appToken, botToken string) *Channel {
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	return &Channel{api: api, client: socketmode.New(api)}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "slack" }

// Start runs the Socket Mode loop and forwards each user message as an Inbound
// until ctx is cancelled. socketmode reconnects internally; RunContext only
// returns on ctx cancellation or a fatal error.
func (c *Channel) Start(ctx context.Context, sink channels.Sink) error {
	// Learn our own user id so we can detect being @mentioned. If this fails the
	// bot still serves DMs; group messages just won't be seen as mentions (so they
	// won't trigger unless the channel is set to respond to all) — the safe default.
	if resp, err := c.api.AuthTestContext(ctx); err == nil {
		c.botID = resp.UserID
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-c.client.Events:
				if !ok {
					return
				}
				if evt.Type != socketmode.EventTypeEventsAPI {
					continue
				}
				ack := func() {
					if evt.Request != nil {
						_ = c.client.Ack(*evt.Request)
					}
				}
				api, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					ack()
					continue
				}
				me, ok := api.InnerEvent.Data.(*slackevents.MessageEvent)
				if !ok {
					ack()
					continue
				}
				inb, ok := toInbound(me, c.botID)
				if !ok {
					ack()
					continue
				}
				// Ack (which advances Slack's delivery) ONLY after the message is
				// durably recorded; on failure leave it unacked so Slack redelivers.
				if err := sink.Deliver(ctx, inb); err != nil {
					if ctx.Err() != nil {
						return
					}
					continue
				}
				ack()
			}
		}
	}()
	return c.client.RunContext(ctx)
}

// toInbound converts a Slack message event to a normalized Inbound. It skips bot
// messages (including our own replies, which carry a bot id), message subtypes
// (edits/joins/etc.), and empty or userless messages.
func toInbound(me *slackevents.MessageEvent, botID string) (channels.Inbound, bool) {
	if me == nil || me.BotID != "" || me.SubType != "" {
		return channels.Inbound{}, false
	}
	if me.User == "" || strings.TrimSpace(me.Text) == "" {
		return channels.Inbound{}, false
	}
	// A 1:1 DM is channel_type "im". In a channel the bot acts only when its user
	// id appears as a mention token (<@BOTID>) — structural, not a name substring.
	isDirect := me.ChannelType == "im"
	mentioned := botID != "" && strings.Contains(me.Text, "<@"+botID+">")
	return channels.Inbound{
		Channel:      "slack",
		Conversation: me.Channel,
		Principal:    me.User,
		Text:         me.Text,
		MessageID:    me.TimeStamp, // Slack's per-message ts, unique within a channel
		IsDirect:     isDirect,
		Mentioned:    mentioned,
	}, true
}

// slackMaxMessage keeps each posted message well under Slack's hard limit so a
// long reply is split rather than truncated.
const slackMaxMessage = 3900

// Send posts a reply to a channel or DM, splitting long text with the shared
// chunker so it goes through the same egress as every other channel.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	for _, part := range channels.Chunk(msg.Text, slackMaxMessage) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, _, err := c.api.PostMessageContext(ctx, conversation, slack.MsgOptionText(part, false)); err != nil {
			return err
		}
	}
	return nil
}
