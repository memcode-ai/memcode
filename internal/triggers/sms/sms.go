// Package sms is the gateway's SMS adapter over Twilio: inbound messages
// arrive as form-encoded webhooks (signature-verified), replies go out through
// the Messages API. SMS is the tactical lane — alerts, approvals, short tasks
// from a phone — not a long-form chat surface. A2P registration (US 10DLC) is
// the operator's responsibility with Twilio.
//
// Signature validation needs the EXACT public URL Twilio posts to (scheme,
// host, path — byte for byte), which a proxied server can't reliably
// reconstruct, so it is explicit config: sms.webhook_url in gateway.yaml.
package sms

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
)

// smsMaxMessage keeps outbound parts within one concatenated-SMS budget;
// Twilio splits further on the wire, but 1500 keeps cost and ordering sane.
const smsMaxMessage = 1500

// Channel is a Twilio SMS connection.
type Channel struct {
	accountSID string
	authToken  string
	from       string // our E.164 sending number
	webhookURL string // the exact public URL Twilio posts to (signature input)
	base       string // API base; overridable in tests
	client     *http.Client
	mediaDir   string // media spool; "" disables MMS media download
}

// New builds an SMS channel. webhookURL must be the exact public URL configured
// on the Twilio number; with it empty the handler rejects everything (fail
// closed — unsigned/unverifiable inbound SMS is never delivered).
func New(accountSID, authToken, from, webhookURL, mediaDir string) *Channel {
	return &Channel{
		accountSID: accountSID,
		authToken:  authToken,
		from:       from,
		webhookURL: strings.TrimSpace(webhookURL),
		base:       "https://api.twilio.com",
		client:     &http.Client{Timeout: 30 * time.Second},
		mediaDir:   mediaDir,
	}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "sms" }

// Handler returns the inbound webhook handler. Twilio signs each request with
// HMAC-SHA1 over the exact URL plus the sorted form params; a request that
// doesn't verify is rejected before anything is parsed.
func (c *Channel) Handler(sink channels.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if !c.verifySignature(r.Header.Get("X-Twilio-Signature"), r.PostForm) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		inb, refs, ok := toInbound(r.PostForm)
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		inb.Attachments = c.download(r.Context(), refs)
		if err := sink.Deliver(r.Context(), inb); err != nil {
			http.Error(w, "not recorded", http.StatusServiceUnavailable) // Twilio retries
			return
		}
		// An empty TwiML response = no synchronous reply; ours goes out through
		// the Messages API when the job finishes.
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Response></Response>`))
	})
}

// verifySignature implements Twilio's scheme: base64(HMAC-SHA1(authToken,
// url + k1v1k2v2… with keys sorted)). Fails closed when the webhook URL isn't
// configured.
func (c *Channel) verifySignature(header string, form url.Values) bool {
	if c.webhookURL == "" || c.authToken == "" || header == "" {
		return false
	}
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(c.webhookURL)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(form.Get(k))
	}
	mac := hmac.New(sha1.New, []byte(c.authToken))
	mac.Write([]byte(b.String()))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(header))
}

// mediaRef is one MMS media item.
type mediaRef struct {
	url  string
	mime string
}

// toInbound normalizes a Twilio inbound-message form.
func toInbound(form url.Values) (channels.Inbound, []mediaRef, bool) {
	from := strings.TrimSpace(form.Get("From"))
	sid := strings.TrimSpace(form.Get("MessageSid"))
	body := form.Get("Body")
	var refs []mediaRef
	if n, err := strconv.Atoi(form.Get("NumMedia")); err == nil {
		for i := 0; i < n && i < 10; i++ {
			u := form.Get(fmt.Sprintf("MediaUrl%d", i))
			if u == "" {
				continue
			}
			refs = append(refs, mediaRef{url: u, mime: form.Get(fmt.Sprintf("MediaContentType%d", i))})
		}
	}
	if from == "" || sid == "" || (strings.TrimSpace(body) == "" && len(refs) == 0) {
		return channels.Inbound{}, nil, false
	}
	return channels.Inbound{
		Channel:      "sms",
		Conversation: from, // SMS is 1:1; the sender's number is the reply route
		Principal:    from, // E.164 — the stable id carriers authenticate
		Text:         body,
		MessageID:    sid,
		IsDirect:     true,
	}, refs, true
}

// download fetches MMS media (Twilio media URLs need basic auth; they also
// expire quickly, which is why this happens at receipt). Best-effort.
func (c *Channel) download(ctx context.Context, refs []mediaRef) []channels.Attachment {
	if c.mediaDir == "" || len(refs) == 0 {
		return nil
	}
	var out []channels.Attachment
	for _, ref := range refs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.url, nil)
		if err != nil {
			continue
		}
		req.SetBasicAuth(c.accountSID, c.authToken)
		resp, err := c.client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode/100 != 2 {
			resp.Body.Close()
			continue
		}
		att, err := channels.SaveToSpool(c.mediaDir, resp.Body, ref.mime, "mms")
		resp.Body.Close()
		if err != nil {
			continue
		}
		out = append(out, att)
	}
	return out
}

// Send posts a reply through the Messages API, split with the shared chunker.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	for _, part := range channels.Chunk(msg.Text, smsMaxMessage) {
		if err := c.sendOne(ctx, conversation, part); err != nil {
			return err
		}
	}
	return nil
}

func (c *Channel) sendOne(ctx context.Context, to, body string) error {
	form := url.Values{}
	form.Set("To", to)
	form.Set("From", c.from)
	form.Set("Body", body)
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json", c.base, c.accountSID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.accountSID, c.authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("twilio send: status %d", resp.StatusCode)
	}
	return nil
}
