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
func (c *Channel) Start(ctx context.Context, inbound chan<- channels.Inbound) error {
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
				if evt.Request != nil {
					_ = c.client.Ack(*evt.Request) // Slack requires prompt ack of each request
				}
				api, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				me, ok := api.InnerEvent.Data.(*slackevents.MessageEvent)
				if !ok {
					continue
				}
				inb, ok := toInbound(me)
				if !ok {
					continue
				}
				select {
				case inbound <- inb:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return c.client.RunContext(ctx)
}

// toInbound converts a Slack message event to a normalized Inbound. It skips bot
// messages (including our own replies, which carry a bot id), message subtypes
// (edits/joins/etc.), and empty or userless messages.
func toInbound(me *slackevents.MessageEvent) (channels.Inbound, bool) {
	if me == nil || me.BotID != "" || me.SubType != "" {
		return channels.Inbound{}, false
	}
	if me.User == "" || strings.TrimSpace(me.Text) == "" {
		return channels.Inbound{}, false
	}
	return channels.Inbound{
		Channel:      "slack",
		Conversation: me.Channel,
		Principal:    me.User,
		Text:         me.Text,
	}, true
}

// Send posts a reply to a channel or DM.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	_, _, err := c.api.PostMessageContext(ctx, conversation, slack.MsgOptionText(msg.Text, false))
	return err
}
