package email

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/memcode-ai/memcode/internal/channels"
)

const simpleMail = "From: Tim <Tim@Example.com>\r\n" +
	"To: bot@example.com\r\n" +
	"Subject: Fix the build\r\n" +
	"Message-Id: <m1@example.com>\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"The CI is red, investigate.\r\n"

func TestParseSimple(t *testing.T) {
	p, ok := parseMessage([]byte(simpleMail))
	if !ok {
		t.Fatal("parse failed")
	}
	if p.from != "tim@example.com" || p.subject != "Fix the build" || p.msgID != "<m1@example.com>" {
		t.Errorf("parsed %+v", p)
	}
	if !strings.Contains(p.text, "CI is red") {
		t.Errorf("text = %q", p.text)
	}
}

func TestParseMultipartWithAttachment(t *testing.T) {
	raw := "From: a@b.com\r\n" +
		"Subject: =?utf-8?q?see_attached?=\r\n" +
		"Message-Id: <m2@b.com>\r\n" +
		"In-Reply-To: <root@b.com>\r\n" +
		"References: <root@b.com> <mid@b.com>\r\n" +
		"Content-Type: multipart/mixed; boundary=XX\r\n" +
		"\r\n" +
		"--XX\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"caf=C3=A9 plans attached\r\n" +
		"--XX\r\n" +
		"Content-Type: application/pdf; name=plan.pdf\r\n" +
		"Content-Disposition: attachment; filename=plan.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"JVBERi0xLjQ=\r\n" +
		"--XX--\r\n"
	p, ok := parseMessage([]byte(raw))
	if !ok {
		t.Fatal("parse failed")
	}
	if p.subject != "see attached" {
		t.Errorf("subject = %q", p.subject)
	}
	if !strings.Contains(p.text, "café plans") {
		t.Errorf("text = %q", p.text)
	}
	if len(p.attachments) != 1 || p.attachments[0].name != "plan.pdf" || p.attachments[0].mime != "application/pdf" {
		t.Fatalf("attachments = %+v", p.attachments)
	}
	if string(p.attachments[0].data) != "%PDF-1.4" {
		t.Errorf("decoded data = %q", p.attachments[0].data)
	}
	if len(p.references) != 2 || p.references[0] != "<root@b.com>" {
		t.Errorf("references = %v", p.references)
	}
}

func TestShouldIgnore(t *testing.T) {
	ok := func(raw string) parsedMessage {
		p, _ := parseMessage([]byte(raw))
		return p
	}
	if !shouldIgnore(ok("From: noreply@svc.com\r\n\r\nx"), "bot@x.com") {
		t.Error("noreply not ignored")
	}
	if !shouldIgnore(ok("From: mailer-daemon@x.com\r\n\r\nx"), "bot@x.com") {
		t.Error("bounce not ignored")
	}
	if !shouldIgnore(ok("From: a@b.com\r\nAuto-Submitted: auto-replied\r\n\r\nx"), "bot@x.com") {
		t.Error("auto-submitted not ignored")
	}
	if !shouldIgnore(ok("From: bot@x.com\r\n\r\nx"), "bot@x.com") {
		t.Error("self not ignored (loop!)")
	}
	if shouldIgnore(ok("From: tim@b.com\r\nAuto-Submitted: no\r\n\r\nx"), "bot@x.com") {
		t.Error("normal sender ignored")
	}
}

func TestComposeReplyThreading(t *testing.T) {
	th := threadInfo{root: "<root@b.com>", last: "<m9@b.com>", subject: "Re: build"}
	raw := string(composeReply("bot@x.com", "tim@b.com", th, "done\nall green"))
	for _, want := range []string{
		"To: tim@b.com\r\n",
		"In-Reply-To: <m9@b.com>\r\n",
		"References: <root@b.com> <m9@b.com>\r\n",
		"done\r\nall green",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q in:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "Re: Re:") {
		t.Error("stacked Re:")
	}
	// No remembered thread → still a valid standalone message.
	raw = string(composeReply("bot@x.com", "tim@b.com", threadInfo{}, "hi"))
	if strings.Contains(raw, "In-Reply-To") || !strings.Contains(raw, "Subject: ") {
		t.Errorf("standalone compose wrong:\n%s", raw)
	}
}

// fakeSession scripts one poll cycle.
type fakeSession struct {
	uidValidity uint32
	msgs        map[imap.UID][]byte
	seen        []imap.UID
}

func (f *fakeSession) SelectInbox() (uint32, error) { return f.uidValidity, nil }
func (f *fakeSession) UnseenUIDs() ([]imap.UID, error) {
	var out []imap.UID
	for uid := range f.msgs {
		out = append(out, uid)
	}
	return out, nil
}
func (f *fakeSession) FetchRaw(uid imap.UID) ([]byte, error) { return f.msgs[uid], nil }
func (f *fakeSession) MarkSeen(uid imap.UID) error           { f.seen = append(f.seen, uid); return nil }
func (f *fakeSession) Close() error                          { return nil }

type sinkFn func(channels.Inbound) error

func (s sinkFn) Deliver(_ context.Context, inb channels.Inbound) error { return s(inb) }

func TestPollOnceAckSemantics(t *testing.T) {
	fake := &fakeSession{uidValidity: 7, msgs: map[imap.UID][]byte{42: []byte(simpleMail)}}
	c := New("bot@example.com", "pw", "imap.example.com", "smtp.example.com", 0, "")
	c.dial = func() (imapSession, error) { return fake, nil }

	var got channels.Inbound
	if err := c.pollOnce(context.Background(), sinkFn(func(inb channels.Inbound) error {
		got = inb
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	// Durable dedup key is mailbox/UIDVALIDITY/UID, not the Message-ID.
	if got.MessageID != "INBOX/7/42" {
		t.Errorf("MessageID = %q", got.MessageID)
	}
	if got.Principal != "tim@example.com" || !got.IsDirect || got.Channel != "email" {
		t.Errorf("inbound = %+v", got)
	}
	if len(fake.seen) != 1 || fake.seen[0] != 42 {
		t.Errorf("seen = %v (must mark seen after durable record)", fake.seen)
	}

	// A Deliver failure leaves the message UNSEEN so the next poll retries it.
	fake2 := &fakeSession{uidValidity: 7, msgs: map[imap.UID][]byte{43: []byte(simpleMail)}}
	c.dial = func() (imapSession, error) { return fake2, nil }
	if err := c.pollOnce(context.Background(), sinkFn(func(channels.Inbound) error {
		return errors.New("db down")
	})); err == nil {
		t.Fatal("pollOnce should surface the failure")
	}
	if len(fake2.seen) != 0 {
		t.Errorf("failed delivery must not mark seen, got %v", fake2.seen)
	}
}

func TestSendUsesThread(t *testing.T) {
	c := New("bot@example.com", "pw", "imap.example.com", "smtp.example.com", 0, "")
	var sentTo string
	var sentRaw []byte
	c.send = func(to string, raw []byte) error { sentTo, sentRaw = to, raw; return nil }
	// Simulate having seen a message from tim first.
	p, _ := parseMessage([]byte(simpleMail))
	c.rememberThread(p)
	if err := c.Send(context.Background(), "tim@example.com", channels.Outbound{Text: "on it"}); err != nil {
		t.Fatal(err)
	}
	if sentTo != "tim@example.com" || !strings.Contains(string(sentRaw), "In-Reply-To: <m1@example.com>") {
		t.Errorf("sent to %q raw:\n%s", sentTo, sentRaw)
	}
	if !strings.Contains(string(sentRaw), "Subject: Re: Fix the build") {
		t.Errorf("subject wrong:\n%s", sentRaw)
	}
}
