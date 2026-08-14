// Package whatsapp is the gateway's WhatsApp adapter over the Meta Cloud API.
// Like GitHub it receives inbound messages by webhook (a GET verification
// handshake plus POSTed message events) and, unlike GitHub, can reply — so it
// exposes Send, posting through the Graph API. It stays INERT until the Meta
// business is verified: the gateway only mounts it when whatsapp.active is set
// in gateway.yaml (see internal/gateway/config), because Meta verification is an
// external account state the code can't observe. The user stores the access and
// verify tokens in the global .env (WHATSAPP_ACCESS_TOKEN,
// WHATSAPP_VERIFY_TOKEN); the phone number id is a non-secret setting.
package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
)

// graphVersion pins the Meta Graph API version we call.
const graphVersion = "v21.0"

const defaultBase = "https://graph.facebook.com"

const maxBody = 2 << 20 // 2 MiB

// whatsappMaxMessage caps one outbound text; WhatsApp rejects bodies past 4096.
const whatsappMaxMessage = 4096

// Channel is a WhatsApp Cloud API connection.
type Channel struct {
	phoneNumberID string
	accessToken   string
	verifyToken   string
	appSecret     string // Meta app secret; verifies inbound POST signatures
	base          string // Graph API base; overridable in tests
	client        *http.Client
	mediaDir      string // media spool; "" disables inbound media downloads
}

// New builds a WhatsApp channel from the phone number id and its tokens. appSecret
// is the Meta app secret used to verify inbound message signatures; it must be
// non-empty for the handler to accept POSTed messages. mediaDir is the gateway
// media spool inbound images/voice notes/documents are downloaded into; ""
// disables media handling (text messages still flow).
func New(phoneNumberID, accessToken, verifyToken, appSecret, mediaDir string) *Channel {
	return &Channel{
		phoneNumberID: phoneNumberID,
		accessToken:   accessToken,
		verifyToken:   verifyToken,
		appSecret:     appSecret,
		base:          defaultBase,
		client:        &http.Client{Timeout: 30 * time.Second},
		mediaDir:      mediaDir,
	}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "whatsapp" }

// Handler returns the webhook HTTP handler: GET performs Meta's verification
// handshake; POST parses inbound messages and forwards them as Inbound.
func (c *Channel) Handler(sink channels.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			challenge, ok := verifyChallenge(r.URL.Query(), c.verifyToken)
			if !ok {
				http.Error(w, "verification failed", http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, challenge)
		case http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
			if err != nil {
				http.Error(w, "read error", http.StatusBadRequest)
				return
			}
			// Meta signs the raw body with the app secret. Without a configured
			// secret we cannot authenticate the sender, so we reject rather than
			// trust an unsigned POST.
			if !verifySignature(c.appSecret, r.Header.Get("X-Hub-Signature-256"), body) {
				http.Error(w, "bad signature", http.StatusUnauthorized)
				return
			}
			for _, pm := range toInbounds(body) {
				inb := pm.inb
				inb.Attachments = c.download(r.Context(), pm.media)
				if err := sink.Deliver(r.Context(), inb); err != nil {
					w.WriteHeader(http.StatusServiceUnavailable) // not recorded — Meta retries
					return
				}
			}
			w.WriteHeader(http.StatusOK) // Meta expects a prompt 200 or it retries
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// verifySignature checks Meta's "sha256=<hex>" HMAC header (app secret over the
// raw body). An empty secret can never verify — an unsigned inbound is rejected.
func verifySignature(secret, header string, body []byte) bool {
	if secret == "" {
		return false
	}
	want, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	wantMAC, err := hex.DecodeString(want)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(wantMAC, mac.Sum(nil))
}

// verifyChallenge implements Meta's subscription handshake: echo hub.challenge
// when the mode is "subscribe" and the verify token matches.
func verifyChallenge(q map[string][]string, verifyToken string) (string, bool) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if get("hub.mode") != "subscribe" || get("hub.verify_token") != verifyToken || verifyToken == "" {
		return "", false
	}
	return get("hub.challenge"), true
}

// waMedia references one piece of media on a message (resolved via the Graph
// media endpoint at download time).
type waMedia struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	Filename string `json:"filename"`
	Caption  string `json:"caption"`
}

// inboundPayload is the subset of a WhatsApp webhook payload we read.
type inboundPayload struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Messages []struct {
					ID   string `json:"id"`
					From string `json:"from"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
					Image    *waMedia `json:"image"`
					Audio    *waMedia `json:"audio"`
					Document *waMedia `json:"document"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// parsedMessage pairs a normalized Inbound with the media it references; the
// caller downloads the media (needs the access token) before delivering.
type parsedMessage struct {
	inb   channels.Inbound
	media []waMedia
}

// toInbounds extracts each text/image/audio/document message from a webhook
// payload. Status updates and unsupported types are skipped.
func toInbounds(body []byte) []parsedMessage {
	var p inboundPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	var out []parsedMessage
	for _, e := range p.Entry {
		for _, ch := range e.Changes {
			for _, m := range ch.Value.Messages {
				if m.From == "" {
					continue
				}
				text := m.Text.Body
				var media []waMedia
				for _, w := range []*waMedia{m.Image, m.Audio, m.Document} {
					if w != nil && w.ID != "" {
						media = append(media, *w)
						if text == "" {
							text = w.Caption // media caption is the task text
						}
					}
				}
				if text == "" && len(media) == 0 {
					continue
				}
				out = append(out, parsedMessage{
					inb: channels.Inbound{
						Channel:      "whatsapp",
						Conversation: m.From,
						Principal:    m.From,
						Text:         text,
						MessageID:    m.ID,
						IsDirect:     true, // WhatsApp Cloud messages are 1:1 with the sender
					},
					media: media,
				})
			}
		}
	}
	return out
}

// download fetches referenced media into the spool: GET /<media-id> resolves a
// short-lived URL, then the bytes are fetched with the same bearer. Best-effort —
// a failed download drops that attachment, the message still flows.
func (c *Channel) download(ctx context.Context, media []waMedia) []channels.Attachment {
	if c.mediaDir == "" || len(media) == 0 {
		return nil
	}
	var out []channels.Attachment
	for _, m := range media {
		att, err := c.downloadOne(ctx, m)
		if err != nil {
			continue
		}
		out = append(out, att)
	}
	return out
}

func (c *Channel) downloadOne(ctx context.Context, m waMedia) (channels.Attachment, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/%s/%s", c.base, graphVersion, m.ID), nil)
	if err != nil {
		return channels.Attachment{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	resp, err := c.client.Do(req)
	if err != nil {
		return channels.Attachment{}, err
	}
	defer resp.Body.Close()
	var meta struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil || meta.URL == "" {
		return channels.Attachment{}, fmt.Errorf("whatsapp media lookup failed")
	}
	dreq, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.URL, nil)
	if err != nil {
		return channels.Attachment{}, err
	}
	dreq.Header.Set("Authorization", "Bearer "+c.accessToken)
	dresp, err := c.client.Do(dreq)
	if err != nil {
		return channels.Attachment{}, err
	}
	defer dresp.Body.Close()
	if dresp.StatusCode/100 != 2 {
		return channels.Attachment{}, fmt.Errorf("whatsapp media download: status %d", dresp.StatusCode)
	}
	name := m.Filename
	if name == "" {
		name = "media"
	}
	return channels.SaveToSpool(c.mediaDir, dresp.Body, m.MimeType, name)
}

// Send posts a text reply to a conversation (the recipient's phone number),
// split with the shared chunker — WhatsApp rejects over-long bodies, and this
// was the one adapter bypassing the shared splitter.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	for _, part := range channels.Chunk(msg.Text, whatsappMaxMessage) {
		if err := c.sendOne(ctx, conversation, part); err != nil {
			return err
		}
	}
	return nil
}

func (c *Channel) sendOne(ctx context.Context, conversation, text string) error {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                conversation,
		"type":              "text",
		"text":              map[string]string{"body": text},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/%s/%s/messages", c.base, graphVersion, c.phoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("whatsapp send: status %d", resp.StatusCode)
	}
	return nil
}
