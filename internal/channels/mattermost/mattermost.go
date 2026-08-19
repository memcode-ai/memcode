// Package mattermost is the gateway's Mattermost channel adapter, aimed at
// self-hosted servers. It listens on the server's WebSocket event stream
// (api/v4/websocket) for new posts and replies over the REST v4 API — no SDK,
// just net/http plus gorilla/websocket for the socket (the dependency is
// isolated here, guarded by TestVendorSDKsOnlyInTheirAdapters). The user
// creates a bot account, generates a bot access token, and puts the server
// base URL and token in the global .env as MATTERMOST_URL (e.g.
// https://mm.example.com) and MATTERMOST_TOKEN; messages and the token never
// leave the machines the user already runs.
package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/memcode-ai/memcode/internal/channels"
)

const (
	// mattermostMaxMessage stays under Mattermost's default ~4000-character post
	// cap with headroom, since the cap is server-configurable downward too.
	mattermostMaxMessage = 3800
	maxReconnectBackoff  = 60 * time.Second
)

// Channel is a Mattermost bot connection.
type Channel struct {
	base     string // server base URL, no trailing slash
	token    string
	client   *http.Client
	mediaDir string // media spool; "" disables attachment downloads
}

// New builds a Mattermost channel for the given server base URL and bot access
// token. mediaDir is the gateway media spool file attachments are downloaded
// into; "" disables attachment handling (messages still flow as text).
func New(serverURL, token, mediaDir string) *Channel {
	return &Channel{
		base:     strings.TrimRight(serverURL, "/"),
		token:    token,
		client:   &http.Client{Timeout: 30 * time.Second},
		mediaDir: mediaDir,
	}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "mattermost" }

// wsEvent mirrors the fields we use from a Mattermost websocket event. The
// "post" payload arrives double-encoded: a JSON string holding the post's own
// JSON, so it stays a string here and is unmarshalled a second time.
type wsEvent struct {
	Event string `json:"event"`
	Data  struct {
		Post        string `json:"post"`         // JSON-encoded post (double-encoded)
		ChannelType string `json:"channel_type"` // "D" = DM, "O"/"P" = channels
		SenderName  string `json:"sender_name"`
	} `json:"data"`
}

// post mirrors the fields we use from a Mattermost post.
type post struct {
	ID        string   `json:"id"`
	UserID    string   `json:"user_id"`
	ChannelID string   `json:"channel_id"`
	Message   string   `json:"message"`
	RootID    string   `json:"root_id"`
	FileIDs   []string `json:"file_ids"`
	Type      string   `json:"type"` // non-empty = system message
}

// Start connects to the websocket event stream and forwards each user post as
// an Inbound until ctx is cancelled. A dropped socket reconnects with jittered
// exponential backoff rather than returning (each failure logged, never
// swallowed), so a flaky server never takes the gateway down. A permanently bad
// MATTERMOST_URL (unparseable, or not http/https) IS fatal: retrying can never
// fix config, so the error is returned and the gateway reports the channel as
// stopped instead of silently spinning forever.
func (c *Channel) Start(ctx context.Context, sink channels.Sink) error {
	wsURL, err := deriveWSURL(c.base)
	if err != nil {
		return err // config error — fatal, never retried
	}

	// Learn our own id and username so we can skip our own posts and detect being
	// addressed in a channel. If it fails the bot still serves DMs (recognized by
	// channel type); channel messages just won't match as mentions — the safe
	// default.
	selfID, selfUsername := c.getMe(ctx)

	backoff := channels.NewBackoff(time.Second, maxReconnectBackoff)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.runConn(ctx, sink, wsURL, selfID, selfUsername, backoff); err != nil && ctx.Err() == nil {
			log.Printf("mattermost: connection error: %v (reconnecting)", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Exponential backoff with jitter, capped, so many gateways don't hammer a
		// recovering server in lockstep. Reset happens inside runConn once the
		// socket proves healthy (a frame actually arrives).
		if err := backoff.Sleep(ctx); err != nil {
			return err
		}
	}
}

// runConn owns one websocket connection: dial, authenticate, and pump events
// until the socket errors or ctx is cancelled. backoff is reset to the floor
// only after a frame arrives, so a server that accepts dials and instantly
// drops them still walks the backoff ladder.
func (c *Channel) runConn(ctx context.Context, sink channels.Sink, wsURL, selfID, selfUsername string, backoff *channels.Backoff) error {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	hdr := http.Header{"Authorization": {"Bearer " + c.token}}
	conn, resp, err := dialer.DialContext(ctx, wsURL, hdr)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer conn.Close()

	// Unblock the ReadMessage loop on shutdown: closing the conn is the only way
	// to interrupt a blocked read.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	// The header auth above is accepted on the upgrade, but the documented flow
	// is the in-band authentication challenge — send it too, belt and braces.
	challenge := map[string]any{
		"seq":    1,
		"action": "authentication_challenge",
		"data":   map[string]any{"token": c.token},
	}
	if err := conn.WriteJSON(challenge); err != nil {
		return fmt.Errorf("authentication challenge: %w", err)
	}

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("event stream: %w", err)
		}
		backoff.Reset() // the socket is live — reset the ladder
		inb, fileIDs, ok := parseEvent(raw, selfID, selfUsername)
		if !ok {
			continue
		}
		inb.Attachments = c.download(ctx, fileIDs)
		// The websocket has no per-message replay, so a Deliver failure can't be
		// retried — the durable record is best-effort here.
		_ = sink.Deliver(ctx, inb)
	}
}

// deriveWSURL maps the server base URL to its websocket endpoint: https becomes
// wss, http becomes ws, path api/v4/websocket.
func deriveWSURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("mattermost server url %q: %w", base, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("mattermost server url %q: unsupported scheme %q", base, u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v4/websocket"
	return u.String(), nil
}

// parseEvent converts a raw websocket frame to a normalized Inbound plus the
// file ids it references, or ok=false for anything that shouldn't flow: other
// event types, our own posts, system messages, and empty posts. selfID and
// selfUsername identify this bot; either may be empty when users/me failed.
func parseEvent(raw []byte, selfID, selfUsername string) (channels.Inbound, []string, bool) {
	var ev wsEvent
	if err := json.Unmarshal(raw, &ev); err != nil || ev.Event != "posted" {
		return channels.Inbound{}, nil, false
	}
	// The post rides inside the event as a JSON-encoded string — unmarshal again.
	var p post
	if err := json.Unmarshal([]byte(ev.Data.Post), &p); err != nil {
		return channels.Inbound{}, nil, false
	}
	if selfID != "" && p.UserID == selfID {
		return channels.Inbound{}, nil, false
	}
	if p.Type != "" { // system message (joins, headers, …), never a task
		return channels.Inbound{}, nil, false
	}
	if strings.TrimSpace(p.Message) == "" && len(p.FileIDs) == 0 {
		return channels.Inbound{}, nil, false
	}
	// Principal is the stable user id, never the mutable username, so the
	// allow-list authorizes on a stable identity.
	return channels.Inbound{
		Channel:      "mattermost",
		Conversation: p.ChannelID,
		Principal:    p.UserID,
		Text:         p.Message,
		MessageID:    p.ID,
		IsDirect:     ev.Data.ChannelType == "D",
		Mentioned:    mentionsUser(p.Message, selfUsername),
	}, p.FileIDs, true
}

// mentionsUser reports whether text contains "@username" as a whole mention.
// Mattermost mentions are plain text (no entity metadata on the event), so
// this is the one place a substring check is the structural check: the match
// must end at a word boundary so "@tim" never fires on "@timothy".
func mentionsUser(text, username string) bool {
	if username == "" {
		return false
	}
	want := "@" + strings.ToLower(username)
	lower := strings.ToLower(text)
	for from := 0; ; {
		i := strings.Index(lower[from:], want)
		if i < 0 {
			return false
		}
		end := from + i + len(want)
		if end >= len(lower) {
			return true
		}
		r, _ := utf8.DecodeRuneInString(lower[end:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return true
		}
		from = end
	}
}

// getMe fetches this bot's id and username. On any error it returns zero
// values, and the caller degrades safely (DMs still work; channel mentions
// won't match).
func (c *Channel) getMe(ctx context.Context) (id, username string) {
	resp, err := c.get(ctx, "/api/v4/users/me")
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", ""
	}
	var out struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", ""
	}
	return out.ID, out.Username
}

// download fetches each referenced file into the media spool (files/{id}/info
// for the name and MIME type, then the file bytes), best-effort: a failed
// download drops that attachment, the message itself still flows.
func (c *Channel) download(ctx context.Context, fileIDs []string) []channels.Attachment {
	if c.mediaDir == "" || len(fileIDs) == 0 {
		return nil
	}
	var out []channels.Attachment
	for _, id := range fileIDs {
		att, err := c.downloadOne(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, att)
	}
	return out
}

func (c *Channel) downloadOne(ctx context.Context, fileID string) (channels.Attachment, error) {
	name, mime := "file", ""
	if resp, err := c.get(ctx, "/api/v4/files/"+url.PathEscape(fileID)+"/info"); err == nil {
		var info struct {
			Name     string `json:"name"`
			MimeType string `json:"mime_type"`
		}
		if resp.StatusCode/100 == 2 && json.NewDecoder(resp.Body).Decode(&info) == nil {
			if info.Name != "" {
				name = info.Name
			}
			mime = info.MimeType
		}
		resp.Body.Close()
	}
	resp, err := c.get(ctx, "/api/v4/files/"+url.PathEscape(fileID))
	if err != nil {
		return channels.Attachment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return channels.Attachment{}, fmt.Errorf("mattermost file download: status %d", resp.StatusCode)
	}
	return channels.SaveToSpool(c.mediaDir, resp.Body, mime, name)
}

// get performs one authenticated GET against the REST v4 API.
func (c *Channel) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.client.Do(req)
}

// Send posts a reply to a channel, split with the shared chunker to stay under
// Mattermost's per-post length cap. A non-2xx fails fast rather than retrying.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	for _, part := range channels.Chunk(msg.Text, mattermostMaxMessage) {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := json.Marshal(map[string]string{"channel_id": conversation, "message": part})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v4/posts", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("mattermost create post: status %d", resp.StatusCode)
		}
	}
	return nil
}
