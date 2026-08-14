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
	// Attachments are media the sender included, already downloaded into the
	// gateway's media spool by the adapter (see SaveToSpool). The spool is the
	// trust boundary: downstream code addresses an attachment by its spool ID,
	// never by an arbitrary path.
	Attachments []Attachment
}

// Attachment kinds — a coarse content class, mapped by MIME type.
const (
	KindImage = "image"
	KindAudio = "audio"
	KindPDF   = "pdf"
	KindFile  = "file"
)

// Attachment is one piece of inbound media, stored in the gateway media spool.
type Attachment struct {
	Path string // absolute path inside the media spool
	Kind string // image | audio | pdf | file
	Mime string // as reported by the platform (best-effort)
	Name string // original filename, display only
}

// ID returns the attachment's spool ID — the bare spool filename. IDs, not
// paths, are what cross process boundaries (durable inbox, job context); the
// consumer re-resolves an ID strictly inside the spool directory.
func (a Attachment) ID() string { return filepathBase(a.Path) }

// Outbound is a reply to post back to a conversation.
type Outbound struct {
	Text string
	// VoicePath optionally points at a synthesized speech rendition of Text
	// (OGG/Opus in the media spool). Adapters that can send voice notes send it
	// alongside/instead of the text; adapters that can't simply ignore it — Text
	// is always present as the fallback.
	VoicePath string
}

// Sink receives inbound messages from an adapter. Deliver applies the gateway's
// gating and authorization and, for a message that should run, durably records
// it for processing. A nil return means the adapter may acknowledge the provider
// (the message was recorded, was a duplicate, or was intentionally dropped); a
// non-nil error means it was NOT durably recorded, so the adapter must NOT ack —
// the provider will redeliver. Acking only after a nil return is what makes
// delivery durable: a crash before the record simply causes a redelivery.
type Sink interface {
	Deliver(ctx context.Context, inb Inbound) error
}

// Channel is a bidirectional chat surface.
type Channel interface {
	// Name is the adapter's stable identifier (matches Inbound.Channel).
	Name() string
	// Start owns the connection and hands each inbound message to the sink until
	// ctx is cancelled, returning ctx.Err() on clean shutdown. It must NOT return
	// on transient network errors — reconnect/back off instead, so a flaky
	// platform never takes the gateway down. It acknowledges the provider only
	// after Deliver returns nil.
	Start(ctx context.Context, sink Sink) error
	// Send posts a reply to the given conversation. Safe to call while Start runs.
	Send(ctx context.Context, conversation string, msg Outbound) error
}
