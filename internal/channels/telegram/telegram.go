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
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
)

const (
	defaultBase        = "https://api.telegram.org"
	telegramMaxMessage = 4096 // Telegram's per-message character limit
	maxPollBackoff     = 60 * time.Second
)

// OffsetStore persists the getUpdates ack cursor so a restart resumes where it
// left off instead of re-fetching (and re-running) the whole backlog. Satisfied
// by the gateway's state store; nil in tests / when no persistence is wired.
type OffsetStore interface {
	Offset(ctx context.Context, channel string) (int64, error)
	SetOffset(ctx context.Context, channel string, offset int64) error
}

// Channel is a Telegram bot connection.
type Channel struct {
	token  string
	base   string // API base; overridable in tests
	client *http.Client
	store  OffsetStore
}

// New builds a Telegram channel for the given bot token. store may be nil, in
// which case the poll offset lives only in memory (and a restart re-reads the
// backlog, which the router's dedup then discards).
func New(token string, store OffsetStore) *Channel {
	return &Channel{
		token: token,
		base:  defaultBase,
		// The HTTP timeout must exceed the long-poll timeout so getUpdates can
		// block server-side for the full window without the client giving up.
		client: &http.Client{Timeout: 65 * time.Second},
		store:  store,
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
// ctx is cancelled. The ack cursor is loaded from (and saved to) the offset store
// so a restart resumes where it left off. Transient errors back off with jitter
// rather than returning, so a flaky network never takes the gateway down.
func (c *Channel) Start(ctx context.Context, inbound chan<- channels.Inbound) error {
	var offset int64
	if c.store != nil {
		if v, err := c.store.Offset(ctx, "telegram"); err == nil {
			offset = v
		}
	}
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ups, err := c.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Exponential backoff with jitter, capped. The jitter matters: a fixed
			// backoff can resonate with Telegram's ~30s server-side session TTL and
			// keep 409-conflicting with a stale poll forever.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jitter(backoff)):
			}
			backoff = min(backoff*2, maxPollBackoff)
			continue
		}
		backoff = time.Second // recovered — reset the ladder
		for _, u := range ups {
			offset = u.UpdateID + 1 // ack: next poll starts past this update
			if inb, ok := toInbound(u); ok {
				select {
				case inbound <- inb:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			// Persist AFTER forwarding: on a crash we re-fetch rather than skip,
			// and the router's dedup discards the re-delivery.
			if c.store != nil {
				_ = c.store.SetOffset(ctx, "telegram", offset)
			}
		}
	}
}

// jitter returns d scaled by a random factor in [0.75, 1.25) so concurrent
// pollers don't retry in lockstep and no fixed period resonates with a server
// session TTL.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.75 + rand.Float64()*0.5))
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
		MessageID:    strconv.FormatInt(u.UpdateID, 10),
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

// Send posts a text reply to a chat, split with the shared chunker to respect
// Telegram's per-message limit. Each part honors a 429 flood-wait; a permanent
// error (any other non-2xx) fails fast instead of retrying forever.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	for _, part := range channels.Chunk(msg.Text, telegramMaxMessage) {
		if err := c.sendOne(ctx, conversation, part); err != nil {
			return err
		}
	}
	return nil
}

// sendOne posts a single (already length-bounded) message, retrying only on a
// 429 for the flood-wait Telegram asks for. Never spawn a fallback send on a
// rate limit — that's the burst that escalates the penalty.
func (c *Channel) sendOne(ctx context.Context, conversation, text string) error {
	const maxAttempts = 3
	for attempt := 1; ; attempt++ {
		status, retryAfter, err := c.doSend(ctx, conversation, text)
		if err != nil {
			return err
		}
		if status/100 == 2 {
			return nil
		}
		if status == http.StatusTooManyRequests && attempt < maxAttempts {
			wait := time.Duration(retryAfter) * time.Second
			if wait <= 0 {
				wait = time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return fmt.Errorf("telegram sendMessage: status %d", status)
	}
}

// doSend performs one sendMessage call, returning the HTTP status and, on a 429,
// the flood-wait seconds Telegram reports in parameters.retry_after.
func (c *Channel) doSend(ctx context.Context, conversation, text string) (status, retryAfter int, err error) {
	body, err := json.Marshal(map[string]any{"chat_id": conversation, "text": text})
	if err != nil {
		return 0, 0, err
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", c.base, c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		var out struct {
			Parameters struct {
				RetryAfter int `json:"retry_after"`
			} `json:"parameters"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Parameters.RetryAfter, nil
	}
	return resp.StatusCode, 0, nil
}
