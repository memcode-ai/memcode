package email

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
)

// parsedMessage is the slice of an inbound email the gateway acts on.
type parsedMessage struct {
	from        string // lowercased address (the principal + conversation)
	subject     string
	msgID       string   // RFC Message-ID, threading metadata
	inReplyTo   string   // parent Message-ID
	references  []string // thread chain, oldest first
	autoSubmit  string   // Auto-Submitted header (loop/bounce detection)
	text        string   // best-effort plain text body
	attachments []rawAttachment
}

type rawAttachment struct {
	name string
	mime string
	data []byte
}

// maxParsedAttachment caps a single decoded email attachment.
const maxParsedAttachment = 25 << 20

// parseMessage extracts what the gateway needs from a raw RFC 5322 message.
// Best-effort: an unparseable message returns ok=false and is skipped (and
// marked seen) rather than wedging the poll loop.
func parseMessage(raw []byte) (parsedMessage, bool) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return parsedMessage{}, false
	}
	var p parsedMessage
	if addr, err := mail.ParseAddress(m.Header.Get("From")); err == nil {
		p.from = strings.ToLower(addr.Address)
	}
	if p.from == "" {
		return parsedMessage{}, false
	}
	dec := &mime.WordDecoder{}
	if s, err := dec.DecodeHeader(m.Header.Get("Subject")); err == nil {
		p.subject = s
	} else {
		p.subject = m.Header.Get("Subject")
	}
	p.msgID = strings.TrimSpace(m.Header.Get("Message-Id"))
	p.inReplyTo = firstMsgID(m.Header.Get("In-Reply-To"))
	for _, id := range strings.Fields(m.Header.Get("References")) {
		if id = strings.TrimSpace(id); id != "" {
			p.references = append(p.references, id)
		}
	}
	p.autoSubmit = strings.ToLower(strings.TrimSpace(m.Header.Get("Auto-Submitted")))
	walkPart(mailHeader(m.Header), m.Body, &p, 0)
	p.text = strings.TrimSpace(p.text)
	return p, true
}

func firstMsgID(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// header adapts the two header types (mail.Header, textproto.MIMEHeader) the
// walk sees to one getter.
type header func(key string) string

func (h header) get(key string) string { return h(key) }

func mailHeader(h mail.Header) header { return func(k string) string { return h.Get(k) } }

func partHeader(p *multipart.Part) header { return func(k string) string { return p.Header.Get(k) } }

// walkPart recurses a MIME tree, collecting the first text/plain body (falling
// back to a stripped-tags-free text/html is deliberately NOT attempted — plain
// text or nothing, like the wire) and every attachment. Depth-capped against
// pathological nesting.
func walkPart(h header, body io.Reader, p *parsedMessage, depth int) {
	if depth > 8 {
		return
	}
	ctype := h.get("Content-Type")
	if ctype == "" {
		ctype = "text/plain"
	}
	mediaType, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if err != nil {
				return
			}
			walkPart(partHeader(part), part, p, depth+1)
		}
	}

	disposition, dparams, _ := mime.ParseMediaType(h.get("Content-Disposition"))
	isAttachment := disposition == "attachment" || dparams["filename"] != "" || params["name"] != ""

	data, err := io.ReadAll(io.LimitReader(decodeTransfer(h.get("Content-Transfer-Encoding"), body), maxParsedAttachment+1))
	if err != nil || int64(len(data)) > maxParsedAttachment {
		return
	}

	switch {
	case !isAttachment && mediaType == "text/plain":
		if p.text == "" {
			p.text = string(data)
		}
	case isAttachment || !strings.HasPrefix(mediaType, "text/"):
		name := dparams["filename"]
		if name == "" {
			name = params["name"]
		}
		if name == "" {
			name = "attachment"
		}
		if dec, err := (&mime.WordDecoder{}).DecodeHeader(name); err == nil {
			name = dec
		}
		if len(data) > 0 {
			p.attachments = append(p.attachments, rawAttachment{name: name, mime: mediaType, data: data})
		}
	}
}

// decodeTransfer wraps body with the declared content-transfer decoding.
func decodeTransfer(encoding string, body io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		return quotedprintable.NewReader(body)
	default:
		return body
	}
}

// shouldIgnore filters non-actionable mail: automated senders, bounces, and
// our own messages (loop prevention). self is the bot's own address.
func shouldIgnore(p parsedMessage, self string) bool {
	if p.from == "" || p.from == self {
		return true
	}
	if p.autoSubmit != "" && p.autoSubmit != "no" {
		return true // auto-generated / auto-replied (RFC 3834)
	}
	local := p.from
	if i := strings.IndexByte(local, '@'); i > 0 {
		local = local[:i]
	}
	for _, bad := range []string{"noreply", "no-reply", "no_reply", "donotreply", "mailer-daemon", "postmaster", "bounce"} {
		if strings.Contains(local, bad) {
			return true
		}
	}
	return false
}
