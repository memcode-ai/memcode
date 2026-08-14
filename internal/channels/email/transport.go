package email

import (
	"fmt"
	"net/smtp"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// dialIMAP opens a logged-in TLS session on the IMAP host.
func (c *Channel) dialIMAP() (imapSession, error) {
	cl, err := imapclient.DialTLS(c.imapHost, nil)
	if err != nil {
		return nil, fmt.Errorf("imap dial %s: %w", c.imapHost, err)
	}
	if err := cl.Login(c.address, c.password).Wait(); err != nil {
		_ = cl.Close()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	return &liveSession{cl: cl}, nil
}

// liveSession adapts imapclient to the poll loop's seam.
type liveSession struct {
	cl *imapclient.Client
}

func (s *liveSession) SelectInbox() (uint32, error) {
	data, err := s.cl.Select(mailbox, nil).Wait()
	if err != nil {
		return 0, fmt.Errorf("imap select: %w", err)
	}
	return data.UIDValidity, nil
}

func (s *liveSession) UnseenUIDs() ([]imap.UID, error) {
	data, err := s.cl.UIDSearch(&imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
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
	// alone never sets \Seen (only a durable Deliver does, via MarkSeen).
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
			if len(bs.Bytes) > 0 {
				return bs.Bytes, nil
			}
		}
	}
	return nil, fmt.Errorf("imap fetch uid %d: empty body", uid)
}

func (s *liveSession) MarkSeen(uid imap.UID) error {
	cmd := s.cl.Store(imap.UIDSetNum(uid), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagSeen},
	}, nil)
	return cmd.Close()
}

func (s *liveSession) Close() error {
	_ = s.cl.Logout().Wait()
	return s.cl.Close()
}

// smtpSend delivers one composed message via SMTP with STARTTLS (net/smtp
// negotiates it automatically when the server advertises it) and the same
// app-password auth as IMAP.
func (c *Channel) smtpSend(to string, raw []byte) error {
	host := c.smtpHost
	bare := host
	if i := strings.IndexByte(bare, ':'); i >= 0 {
		bare = bare[:i]
	}
	auth := smtp.PlainAuth("", c.address, c.password, bare)
	if err := smtp.SendMail(host, auth, c.address, []string{to}, raw); err != nil {
		return fmt.Errorf("smtp send to %s: %w", to, err)
	}
	return nil
}
