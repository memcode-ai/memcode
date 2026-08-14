// Package channels defines the gateway's channel-adapter contract: a normalized
// inbound message and the interface each external surface (Telegram, Discord,
// Slack, …) implements. Adapters own their own connection to their platform;
// the gateway router (internal/gateway/server) maps inbound messages to agent
// work and posts replies back through Send. Keeping the contract this thin is
// what lets a new surface be "one more adapter" rather than a new subsystem.
package channels

import "context"

// Inbound is a normalized message arriving from a channel.
type Inbound struct {
	Channel      string // adapter name, matches Channel.Name() ("telegram", …)
	Conversation string // opaque per-channel chat/thread id the reply routes back to
	Principal    string // who sent it (id or @handle) — for authz + audit
	Text         string // the message body: the task handed to the agent
	// MessageID is the platform's stable, unique id for this delivery (Telegram
	// update_id, Discord message id, Slack event ts, GitHub delivery, WhatsApp
	// wamid). The router dedups on (Channel, MessageID) so a redelivery — after a
	// restart, reconnect, or provider retry — never re-runs as a fresh agent turn.
	// Empty means the adapter couldn't supply one; the router then can't dedup it.
	MessageID string
	// Trusted marks an inbound whose SENDER is already cryptographically
	// authenticated by the transport (a signature-verified webhook), so the
	// router's per-channel allow-list doesn't apply. Chat messages leave this
	// false and are gated by the allow-list; a signed GitHub delivery sets it.
	Trusted bool
	// IsDirect is true for a 1:1 direct message. A DM always triggers the agent;
	// a message in a group/channel triggers only when the bot is addressed (see
	// Mentioned) or the channel is configured to respond to all.
	IsDirect bool
	// Mentioned is true when the bot was explicitly addressed — @mentioned, or
	// replied-to — so a group message meant for it triggers even without
	// respond_to_all. Detected structurally by each adapter, never by substring.
	Mentioned bool
}

// Outbound is a reply to post back to a conversation.
type Outbound struct {
	Text string
}

// Channel is a bidirectional chat surface.
type Channel interface {
	// Name is the adapter's stable identifier (matches Inbound.Channel).
	Name() string
	// Start owns the connection and delivers inbound messages on the channel
	// until ctx is cancelled, returning ctx.Err() on clean shutdown. It must
	// NOT return on transient network errors — reconnect/back off instead, so a
	// flaky platform never takes the gateway down.
	Start(ctx context.Context, inbound chan<- Inbound) error
	// Send posts a reply to the given conversation. Safe to call while Start runs.
	Send(ctx context.Context, conversation string, msg Outbound) error
}
