// Package telegram is the gateway's Telegram channel adapter. It talks to the
// Bot API directly over net/http (long-poll getUpdates + sendMessage) — no SDK,
// matching the repo's thin-dependency ethos. The user creates their own bot via
// @BotFather and puts the token in the global .env as TELEGRAM_BOT_TOKEN;
// messages and the token never leave the machine running the gateway.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
)

const defaultBase = "https://api.telegram.org"

// Channel is a Telegram bot connection.
type Channel struct {
	token  string
	base   string // API base; overridable in tests
	client *http.Client
}

// New builds a Telegram channel for the given bot token.
func New(token string) *Channel {
	return &Channel{
		token: token,
		base:  defaultBase,
		// The HTTP timeout must exceed the long-poll timeout so getUpdates can
		// block server-side for the full window without the client giving up.
		client: &http.Client{Timeout: 65 * time.Second},
	}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "telegram" }

// update mirrors the fields we use from a Telegram Update.
type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		From *struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"from"`
		Chat *struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

// Start long-polls getUpdates and forwards each text message as an Inbound until
// ctx is cancelled. Transient errors back off and retry rather than returning.
func (c *Channel) Start(ctx context.Context, inbound chan<- channels.Inbound) error {
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ups, err := c.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1 // ack: next poll starts past this update
			inb, ok := toInbound(u)
			if !ok {
				continue
			}
			select {
			case inbound <- inb:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// toInbound converts a Telegram update to a normalized Inbound, or ok=false if
// it carries no usable text message.
func toInbound(u update) (channels.Inbound, bool) {
	if u.Message == nil || u.Message.Chat == nil || u.Message.Text == "" {
		return channels.Inbound{}, false
	}
	principal := ""
	if f := u.Message.From; f != nil {
		if f.Username != "" {
			principal = "@" + f.Username
		} else {
			principal = strconv.FormatInt(f.ID, 10)
		}
	}
	return channels.Inbound{
		Channel:      "telegram",
		Conversation: strconv.FormatInt(u.Message.Chat.ID, 10),
		Principal:    principal,
		Text:         u.Message.Text,
	}, true
}

func (c *Channel) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	q := url.Values{}
	q.Set("timeout", "30")
	if offset > 0 {
		q.Set("offset", strconv.FormatInt(offset, 10))
	}
	endpoint := fmt.Sprintf("%s/bot%s/getUpdates?%s", c.base, c.token, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool     `json:"ok"`
		Result      []update `json:"result"`
		Description string   `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("telegram getUpdates: %s", out.Description)
	}
	return out.Result, nil
}

// Send posts a text reply to a chat.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	body, err := json.Marshal(map[string]any{"chat_id": conversation, "text": msg.Text})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", c.base, c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram sendMessage: status %d", resp.StatusCode)
	}
	return nil
}
