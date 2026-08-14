// Package email is the gateway's email channel: a dedicated mailbox the agent
// answers. IMAP (SSL :993) is polled for unseen mail; replies go out over SMTP
// (STARTTLS :587) with proper threading headers. The model is a DEDICATED
// account (an app password for Gmail/Outlook), never your personal inbox — the
// bot answers everything its allow-list admits.
//
// Ack semantics match the gateway contract: a message is marked \Seen ONLY
// after Deliver returns nil, so a crash between fetch and durable record just
// re-fetches it. The durable dedup key is <mailbox>/<UIDVALIDITY>/<UID> — the
// provider-side identity, robust against malformed or duplicated Message-IDs
// (which serve threading, not dedup).
package email

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/memcode-ai/memcode/internal/channels"
)

const (
	defaultPoll    = 15 * time.Second
	maxPollBackoff = 5 * time.Minute
	mailbox        = "INBOX"
)

// Channel is an email (IMAP+SMTP) connection.
type Channel struct {
	address  string // the dedicated account, also the SMTP From
	password string // app password
	imapHost string // host[:port]; port defaults to 993
	smtpHost string // host[:port]; port defaults to 587
	poll     time.Duration
	mediaDir string // media spool; "" disables attachment downloads

	// threads remembers, per peer, how to thread the next reply (root id,
	// last inbound id, subject). In-memory: after a restart a reply still
	// reaches the peer, it may just start a fresh thread.
	mu      sync.Mutex
	threads map[string]threadInfo

	// dial/send are test seams.
	dial func() (imapSession, error)
	send func(to string, raw []byte) error
}

type threadInfo struct {
	root    string
	last    string
	subject string
}

// imapSession is the slice of the IMAP client the poll loop uses (a seam so
// tests can script it).
type imapSession interface {
	SelectInbox() (uidValidity uint32, err error)
	UnseenUIDs() ([]imap.UID, error)
	FetchRaw(uid imap.UID) ([]byte, error)
	MarkSeen(uid imap.UID) error
	Close() error
}

// New builds an email channel. poll <= 0 uses the default (15s). mediaDir is
// the gateway media spool inbound attachments are saved into; "" disables
// attachment handling.
func New(address, password, imapHost, smtpHost string, poll time.Duration, mediaDir string) *Channel {
	if poll <= 0 {
		poll = defaultPoll
	}
	c := &Channel{
		address:  strings.ToLower(strings.TrimSpace(address)),
		password: password,
		imapHost: withDefaultPort(imapHost, "993"),
		smtpHost: withDefaultPort(smtpHost, "587"),
		poll:     poll,
		mediaDir: mediaDir,
		threads:  map[string]threadInfo{},
	}
	c.dial = c.dialIMAP
	c.send = c.smtpSend
	return c
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "email" }

func withDefaultPort(host, port string) string {
	host = strings.TrimSpace(host)
	if host == "" || strings.Contains(host, ":") {
		return host
	}
	return host + ":" + port
}

// Start polls the mailbox until ctx is cancelled. Connection errors back off
// exponentially (capped) instead of returning, so a flaky mail server never
// takes the gateway down.
func (c *Channel) Start(ctx context.Context, sink channels.Sink) error {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.pollOnce(ctx, sink); err != nil && ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxPollBackoff)
			continue
		}
		backoff = time.Second
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.poll):
		}
	}
}

// pollOnce runs one fetch cycle: select, search unseen, deliver each, mark
// seen only on durable record.
func (c *Channel) pollOnce(ctx context.Context, sink channels.Sink) error {
	sess, err := c.dial()
	if err != nil {
		return err
	}
	defer sess.Close()
	uidValidity, err := sess.SelectInbox()
	if err != nil {
		return err
	}
	uids, err := sess.UnseenUIDs()
	if err != nil {
		return err
	}
	for _, uid := range uids {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := sess.FetchRaw(uid)
		if err != nil {
			return err
		}
		msg, ok := parseMessage(raw)
		if !ok || shouldIgnore(msg, c.address) {
			// Not actionable mail (bounce, auto-reply, self, unparseable) — mark
			// seen so it isn't re-fetched forever, and move on.
			_ = sess.MarkSeen(uid)
			continue
		}
		inb := c.toInbound(msg, uidValidity, uid)
		if err := sink.Deliver(ctx, inb); err != nil {
			// NOT durably recorded — leave unseen; the next poll retries it.
			return err
		}
		c.rememberThread(msg)
		_ = sess.MarkSeen(uid)
	}
	return nil
}

// toInbound normalizes a parsed message. The conversation is the peer address
// (the reply route — like a phone number on SMS/WhatsApp); threading metadata
// is remembered per peer for Send.
func (c *Channel) toInbound(msg parsedMessage, uidValidity uint32, uid imap.UID) channels.Inbound {
	var atts []channels.Attachment
	if c.mediaDir != "" {
		atts = c.spoolAttachments(msg)
	}
	text := msg.text
	if strings.TrimSpace(text) == "" && msg.subject != "" {
		text = msg.subject // subject-only mail: the subject is the task
	}
	return channels.Inbound{
		Channel:      "email",
		Conversation: msg.from,
		Principal:    msg.from,
		Text:         text,
		MessageID:    fmt.Sprintf("%s/%d/%d", mailbox, uidValidity, uid),
		IsDirect:     true,
		Attachments:  atts,
	}
}

func (c *Channel) spoolAttachments(msg parsedMessage) []channels.Attachment {
	var out []channels.Attachment
	for _, a := range msg.attachments {
		att, err := channels.SaveToSpool(c.mediaDir, strings.NewReader(string(a.data)), a.mime, a.name)
		if err != nil {
			continue
		}
		out = append(out, att)
	}
	return out
}

// rememberThread records how to thread the next reply to this peer.
func (c *Channel) rememberThread(msg parsedMessage) {
	root := msg.msgID
	if len(msg.references) > 0 {
		root = msg.references[0]
	} else if msg.inReplyTo != "" {
		root = msg.inReplyTo
	}
	c.mu.Lock()
	c.threads[msg.from] = threadInfo{root: root, last: msg.msgID, subject: msg.subject}
	c.mu.Unlock()
}

// Send replies to a peer over SMTP, threading into the remembered conversation
// when one is known. Email has no length cap, so no chunking.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	th := c.threads[conversation]
	c.mu.Unlock()
	raw := composeReply(c.address, conversation, th, msg.Text)
	return c.send(conversation, raw)
}
