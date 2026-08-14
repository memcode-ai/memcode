package email

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"strings"
	"time"
)

// composeReply builds a plain-text RFC 5322 reply. Threading: In-Reply-To
// names the peer's last message, References carries the thread root + last, so
// every client threads it correctly. The subject gets exactly one "Re:".
func composeReply(from, to string, th threadInfo, body string) []byte {
	subject := strings.TrimSpace(th.subject)
	if subject == "" {
		subject = "Your memcode task"
	}
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", stripCRLF(to))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-Id: %s\r\n", newMessageID(from))
	// Threading ids came from inbound mail. mail.ReadMessage already rejects
	// CRLF-bearing headers, but strip line breaks anyway (defense in depth): a
	// Message-ID must never be able to smuggle extra headers into our reply.
	if last := stripCRLF(th.last); last != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", last)
	}
	if refs := stripCRLF(threadReferences(th)); refs != "" {
		fmt.Fprintf(&b, "References: %s\r\n", refs)
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	// Normalize newlines to CRLF; net/smtp handles dot-stuffing itself.
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}

// stripCRLF removes line breaks from a header value (header-injection guard).
// mail.ReadMessage already rejects CRLF-bearing inbound headers; this is the
// cheap second layer so no future caller can regress it.
func stripCRLF(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

func threadReferences(th threadInfo) string {
	switch {
	case th.root == "" && th.last == "":
		return ""
	case th.root == th.last || th.root == "":
		return th.last
	case th.last == "":
		return th.root
	default:
		return th.root + " " + th.last
	}
}

// newMessageID mints a unique Message-ID under the sender's domain.
func newMessageID(from string) string {
	domain := "memcode.local"
	if i := strings.IndexByte(from, '@'); i >= 0 && i+1 < len(from) {
		domain = from[i+1:]
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(b), domain)
}
