package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// maxRawMessage caps a single fetched message; a larger one is skipped (the
// caller advances the cursor past it) rather than buffered whole.
const maxRawMessage = 60 << 20

// errTooLarge marks an oversized message so the poll loop can skip it durably
// instead of retrying it forever.
var errTooLarge = errors.New("message too large")

// Network bounds. A mail server that accepts the TCP connection and then goes
// silent must cost one bounded poll, never a wedged channel.
const (
	dialTimeout   = 30 * time.Second
	ioIdleTimeout = 90 * time.Second // per read/write: a stalled server, not a slow big fetch
	smtpTimeout   = 2 * time.Minute  // whole SMTP transaction
)

// idleConn re-arms a deadline before every read and write, so a session can
// run as long as it keeps making progress but a silent peer times out.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c idleConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c idleConn) Write(p []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}

// dialIMAP opens a logged-in TLS session on the IMAP host. The dial is bounded
// and ctx-aware, every read/write has an idle deadline, and cancelling ctx
// closes the connection so an in-flight command returns at once.
func (c *Channel) dialIMAP(ctx context.Context) (imapSession, error) {
	host, _, err := net.SplitHostPort(c.imapHost)
	if err != nil {
		return nil, fmt.Errorf("imap dial %s: %w", c.imapHost, err)
	}
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout},
		Config:    &tls.Config{ServerName: host, NextProtos: []string{"imap"}},
	}
	conn, err := d.DialContext(ctx, "tcp", c.imapHost)
	if err != nil {
		return nil, fmt.Errorf("imap dial %s: %w", c.imapHost, err)
	}
	cl := imapclient.New(idleConn{Conn: conn, idle: ioIdleTimeout}, nil)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := cl.Login(c.address, c.password).Wait(); err != nil {
		stop()
		_ = cl.Close()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	return &liveSession{cl: cl, stop: stop}, nil
}

// liveSession adapts imapclient to the poll loop's seam.
type liveSession struct {
	cl   *imapclient.Client
	stop func() bool // detaches the ctx-cancel closer
}

func (s *liveSession) SelectInbox() (uint32, imap.UID, error) {
	data, err := s.cl.Select(mailbox, nil).Wait()
	if err != nil {
		return 0, 0, fmt.Errorf("imap select: %w", err)
	}
	return data.UIDValidity, data.UIDNext, nil
}

// UIDsAfter searches UIDs strictly greater than last. The (last+1):* range is
// how IMAP expresses "everything newer"; note the server returns the newest
// message even when nothing is new (the caller filters uid <= last).
func (s *liveSession) UIDsAfter(last imap.UID) ([]imap.UID, error) {
	var set imap.UIDSet
	set.AddRange(last+1, 0) // 0 = *
	data, err := s.cl.UIDSearch(&imap.SearchCriteria{
		UID: []imap.UIDSet{set},
	}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap search: %w", err)
	}
	uidSet, ok := data.All.(imap.UIDSet)
	if !ok {
		return nil, nil
	}
	uids, _ := uidSet.Nums()
	return uids, nil
}

func (s *liveSession) FetchRaw(uid imap.UID) ([]byte, error) {
	// BODY.PEEK[] — the empty section is the whole message; Peek so fetching
	// never sets \Seen (the channel acks by advancing its cursor, never by
	// touching the mailbox's read state).
	section := &imap.FetchItemBodySection{Peek: true}
	msgs, err := s.cl.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap fetch uid %d: %w", uid, err)
	}
	for _, m := range msgs {
		for _, bs := range m.BodySection {
			if len(bs.Bytes) == 0 {
				continue
			}
			if len(bs.Bytes) > maxRawMessage {
				return nil, fmt.Errorf("imap fetch uid %d (%d bytes): %w", uid, len(bs.Bytes), errTooLarge)
			}
			return bs.Bytes, nil
		}
	}
	return nil, fmt.Errorf("imap fetch uid %d: empty body", uid)
}

func (s *liveSession) Close() error {
	if s.stop != nil {
		s.stop()
	}
	_ = s.cl.Logout().Wait() // bounded by the idle deadline
	return s.cl.Close()
}

// smtpSend delivers one composed message via SMTP with STARTTLS (when the
// server advertises it) and the same app-password auth as IMAP. It is
// net/smtp.SendMail plus what SendMail lacks: a bounded dial, a deadline on the
// whole transaction, and ctx cancellation (closing the connection).
func (c *Channel) smtpSend(ctx context.Context, to string, raw []byte) error {
	if err := c.smtpTransact(ctx, to, raw); err != nil {
		return fmt.Errorf("smtp send to %s: %w", to, err)
	}
	return nil
}

func (c *Channel) smtpTransact(ctx context.Context, to string, raw []byte) error {
	host := c.smtpHost
	bare := host
	if i := strings.IndexByte(bare, ':'); i >= 0 {
		bare = bare[:i]
	}
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", host)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(smtpTimeout))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	cl, err := smtp.NewClient(conn, bare)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer cl.Close()
	if ok, _ := cl.Extension("STARTTLS"); ok {
		if err := cl.StartTLS(&tls.Config{ServerName: bare}); err != nil {
			return err
		}
	}
	if ok, _ := cl.Extension("AUTH"); !ok {
		return errors.New("smtp: server doesn't support AUTH")
	}
	if err := cl.Auth(smtp.PlainAuth("", c.address, c.password, bare)); err != nil {
		return err
	}
	if err := cl.Mail(c.address); err != nil { // Mail/Rcpt reject CR/LF injection
		return err
	}
	if err := cl.Rcpt(to); err != nil {
		return err
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}
