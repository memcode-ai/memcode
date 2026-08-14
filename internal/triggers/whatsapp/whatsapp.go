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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
)

// graphVersion pins the Meta Graph API version we call.
const graphVersion = "v21.0"

const defaultBase = "https://graph.facebook.com"

const maxBody = 2 << 20 // 2 MiB

// Channel is a WhatsApp Cloud API connection.
type Channel struct {
	phoneNumberID string
	accessToken   string
	verifyToken   string
	base          string // Graph API base; overridable in tests
	client        *http.Client
}

// New builds a WhatsApp channel from the phone number id and its tokens.
func New(phoneNumberID, accessToken, verifyToken string) *Channel {
	return &Channel{
		phoneNumberID: phoneNumberID,
		accessToken:   accessToken,
		verifyToken:   verifyToken,
		base:          defaultBase,
		client:        &http.Client{Timeout: 30 * time.Second},
	}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "whatsapp" }

// Handler returns the webhook HTTP handler: GET performs Meta's verification
// handshake; POST parses inbound messages and forwards them as Inbound.
func (c *Channel) Handler(inbound chan<- channels.Inbound) http.Handler {
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
			for _, inb := range toInbounds(body) {
				select {
				case inbound <- inb:
				case <-r.Context().Done():
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
			w.WriteHeader(http.StatusOK) // Meta expects a prompt 200 or it retries
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
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
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// toInbounds extracts each text message from a webhook payload as an Inbound.
// Non-text messages (status updates, media, etc.) are skipped.
func toInbounds(body []byte) []channels.Inbound {
	var p inboundPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	var out []channels.Inbound
	for _, e := range p.Entry {
		for _, ch := range e.Changes {
			for _, m := range ch.Value.Messages {
				if m.Type != "text" || m.From == "" || m.Text.Body == "" {
					continue
				}
				out = append(out, channels.Inbound{
					Channel:      "whatsapp",
					Conversation: m.From,
					Principal:    m.From,
					Text:         m.Text.Body,
					MessageID:    m.ID,
				})
			}
		}
	}
	return out
}

// Send posts a text reply to a conversation (the recipient's phone number).
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                conversation,
		"type":              "text",
		"text":              map[string]string{"body": msg.Text},
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
