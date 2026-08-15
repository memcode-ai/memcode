// Package email is the gateway's email channel: a mailbox the agent answers.
// IMAP (SSL :993) is polled for NEW mail past a durable UID cursor; replies go
// out over SMTP (STARTTLS :587) with proper threading headers. The mailbox can
// be the agent's own dedicated account OR a personal inbox: the channel reads
// with BODY.PEEK and acks by advancing its cursor, NEVER by setting \Seen — it
// leaves flags, folders, and read state exactly as it found them, and the
// gateway's allow-list decides which senders it answers at all.
//
// Ack semantics match the gateway contract: the cursor advances ONLY after
// Deliver returns nil, so a crash between fetch and durable record just
// re-fetches it. On first connect (or a UIDVALIDITY reset) the cursor
// initializes to the mailbox's current end — pre-existing mail, including a
// personal inbox's whole history, is never read, never answered. The durable
// dedup key is <mailbox>/<UIDVALIDITY>/<UID> — the provider-side identity,
// robust against malformed or duplicated Message-IDs (which serve threading,
// not dedup).
//
// IDENTITY CAVEAT: the principal is the RFC From address — weaker than the
// other channels' platform-authenticated ids, since From can be spoofed by
// mail that evades the provider's SPF/DKIM/DMARC filtering. The mailbox
// provider's authentication is the real gate (a mainstream provider rejects or
// junks spoofed mail before we poll it). Enforcing
// Authentication-Results=pass explicitly is a tracked follow-up.
package email

import (
	"bytes"
	"context"
	"errors"
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
	store    CursorStore

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
	SelectInbox() (uidValidity uint32, uidNext imap.UID, err error)
	UIDsAfter(last imap.UID) ([]imap.UID, error)
	FetchRaw(uid imap.UID) ([]byte, error)
	Close() error
}

// CursorStore persists the mailbox position (UIDVALIDITY/last-UID) so a
// restart resumes where it left off. The gateway state store implements it.
type CursorStore interface {
	Cursor(ctx context.Context, channel string) (string, error)
	SetCursor(ctx context.Context, channel, cursor string) error
}

// New builds an email channel. store persists the mailbox cursor. poll <= 0
// uses the default (15s). mediaDir is the gateway media spool inbound
// attachments are saved into; "" disables attachment handling.
func New(address, password, imapHost, smtpHost string, poll time.Duration, mediaDir string, store CursorStore) *Channel {
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
		store:    store,
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

// pollOnce runs one fetch cycle: select, read the durable UID cursor, deliver
// everything newer, advance the cursor on durable record. The cursor — never
// the \Seen flag — is the ack, so the channel leaves the mailbox's read state
// untouched (safe on a personal inbox).
func (c *Channel) pollOnce(ctx context.Context, sink channels.Sink) error {
	sess, err := c.dial()
	if err != nil {
		return err
	}
	defer sess.Close()
	uidValidity, uidNext, err := sess.SelectInbox()
	if err != nil {
		return err
	}
	last, ok := c.loadCursor(ctx, uidValidity)
	if !ok {
		// First connect, or the server reset UIDVALIDITY: start from NOW.
		// Pre-existing mail — a personal inbox's whole history — is never
		// fetched, never answered.
		return c.saveCursor(ctx, uidValidity, endOfMailbox(sess, uidNext))
	}
	uids, err := sess.UIDsAfter(last)
	if err != nil {
		return err
	}
	for _, uid := range uids {
		if uid <= last {
			continue // an n:* range returns the newest message even when nothing is new
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := sess.FetchRaw(uid)
		if err != nil {
			if errors.Is(err, errTooLarge) {
				// Oversized message: skip it durably instead of retrying forever.
				if err := c.saveCursor(ctx, uidValidity, uid); err != nil {
					return err
				}
				continue
			}
			return err
		}
		msg, ok := parseMessage(raw)
		if !ok || shouldIgnore(msg, c.address) {
			// Not actionable mail (bounce, auto-reply, self, unparseable) —
			// advance past it and move on.
			if err := c.saveCursor(ctx, uidValidity, uid); err != nil {
				return err
			}
			continue
		}
		inb := c.toInbound(msg, uidValidity, uid)
		if err := sink.Deliver(ctx, inb); err != nil {
			// NOT durably recorded — cursor stays put; the next poll retries it.
			return err
		}
		c.rememberThread(msg)
		if err := c.saveCursor(ctx, uidValidity, uid); err != nil {
			return err
		}
	}
	return nil
}

// loadCursor reads the persisted "uidvalidity/uid" position; ok=false when it
// is absent, unparseable, or from a different UIDVALIDITY generation.
func (c *Channel) loadCursor(ctx context.Context, uidValidity uint32) (imap.UID, bool) {
	cur, err := c.store.Cursor(ctx, "email")
	if err != nil || strings.TrimSpace(cur) == "" {
		return 0, false
	}
	var v, u uint32
	if _, err := fmt.Sscanf(cur, "%d/%d", &v, &u); err != nil || v != uidValidity {
		return 0, false
	}
	return imap.UID(u), true
}

func (c *Channel) saveCursor(ctx context.Context, uidValidity uint32, uid imap.UID) error {
	return c.store.SetCursor(ctx, "email", fmt.Sprintf("%d/%d", uidValidity, uint32(uid)))
}

// endOfMailbox resolves the current last UID for cursor initialization.
// UIDNEXT from SELECT is authoritative; the rare server that omits it gets a
// search for the newest message instead.
func endOfMailbox(sess imapSession, uidNext imap.UID) imap.UID {
	if uidNext > 0 {
		return uidNext - 1
	}
	uids, err := sess.UIDsAfter(0)
	if err != nil || len(uids) == 0 {
		return 0
	}
	maxUID := uids[0]
	for _, u := range uids {
		if u > maxUID {
			maxUID = u
		}
	}
	return maxUID
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
		att, err := channels.SaveToSpool(c.mediaDir, bytes.NewReader(a.data), a.mime, a.name)
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
