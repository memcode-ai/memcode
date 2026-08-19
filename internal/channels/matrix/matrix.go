// Package matrix is the gateway's Matrix channel adapter. It talks to the
// Matrix client-server API directly over net/http (long-poll /sync + room
// send) — no SDK, matching the repo's thin-dependency ethos. The user points
// it at their homeserver with MATRIX_HOMESERVER + MATRIX_ACCESS_TOKEN in the
// global .env (a dedicated bot account's access token; this package never
// reads the environment itself — the gateway wires the values in).
//
// NO END-TO-END ENCRYPTION in v1. This adapter speaks PLAIN rooms only:
// events in encrypted rooms arrive as m.room.encrypted and are silently
// ignored, because E2EE requires Olm/Megolm session state that a raw HTTP
// client does not carry. Invite the bot to unencrypted rooms.
package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
)

const (
	// Matrix has no hard body-size limit like Telegram's, but the total event
	// must stay under the federation's 65 KiB cap; 4000 chars leaves ample room.
	matrixMaxMessage = 4000
	maxSyncBackoff   = 60 * time.Second
)

// CursorStore persists the /sync since-token so a restart resumes where it
// left off instead of replaying (and re-running) the backlog. Satisfied by the
// gateway's state store; nil in tests / when no persistence is wired.
type CursorStore interface {
	Cursor(ctx context.Context, channel string) (string, error)
	SetCursor(ctx context.Context, channel, cursor string) error
}

// Channel is a Matrix client connection.
type Channel struct {
	homeserver string // base URL, e.g. https://matrix.example.org; overridable in tests
	token      string
	client     *http.Client
	store      CursorStore
	mediaDir   string // media spool; "" disables attachment downloads

	userID string          // own mxid, learned via /whoami at Start
	direct map[string]bool // room ids marked as DMs in m.direct account data
}

// New builds a Matrix channel for the given homeserver and access token.
// store may be nil, in which case the since-token lives only in memory (and a
// restart re-reads recent history, which the router's dedup then discards).
// mediaDir is the gateway media spool images/audio/files are downloaded into;
// "" disables attachment handling (messages still flow as text).
func New(homeserver, accessToken string, store CursorStore, mediaDir string) *Channel {
	return &Channel{
		homeserver: strings.TrimRight(homeserver, "/"),
		token:      accessToken,
		// The HTTP timeout must exceed the long-poll timeout so /sync can block
		// server-side for the full window without the client giving up.
		client:   &http.Client{Timeout: 65 * time.Second},
		store:    store,
		mediaDir: mediaDir,
	}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "matrix" }

// syncResponse mirrors the fields we use from a /sync response. Named
// sub-types (not anonymous structs) so they're straightforward to build in
// tests.
type syncResponse struct {
	NextBatch string    `json:"next_batch"`
	Rooms     syncRooms `json:"rooms"`
}

type syncRooms struct {
	Join map[string]joinedRoom `json:"join"`
}

type joinedRoom struct {
	Timeline roomTimeline `json:"timeline"`
}

type roomTimeline struct {
	Events []roomEvent `json:"events"`
}

type roomEvent struct {
	Type    string       `json:"type"`
	EventID string       `json:"event_id"`
	Sender  string       `json:"sender"`
	Content eventContent `json:"content"`
}

type eventContent struct {
	MsgType       string     `json:"msgtype"`
	Body          string     `json:"body"`
	FormattedBody string     `json:"formatted_body"`
	Filename      string     `json:"filename"`
	URL           string     `json:"url"` // mxc:// URI for media events
	Info          eventInfo  `json:"info"`
	Mentions      mentions   `json:"m.mentions"`
	RelatesTo     *relatesTo `json:"m.relates_to"`
}

type eventInfo struct {
	MimeType string `json:"mimetype"`
}

type mentions struct {
	UserIDs []string `json:"user_ids"`
}

type relatesTo struct {
	InReplyTo *struct {
		EventID string `json:"event_id"`
	} `json:"m.in_reply_to"`
}

// Start long-polls /sync and forwards each message event as an Inbound until
// ctx is cancelled. The since-token is loaded from (and saved to) the cursor
// store so a restart resumes where it left off. Transient errors back off with
// jitter rather than returning, so a flaky homeserver never takes the gateway
// down.
func (c *Channel) Start(ctx context.Context, sink channels.Sink) error {
	// Learn our own mxid so we can skip our own echoes and detect being
	// addressed. If whoami fails the adapter still flows messages; without an
	// own id we also can't fetch m.direct (its URL needs the user id), so DM
	// detection and group mentions degrade to false — the safe default (group
	// messages won't trigger unless respond_to_all).
	c.userID = c.whoami(ctx)
	c.direct = c.fetchDirectRooms(ctx)

	var since string
	if c.store != nil {
		if v, err := c.store.Cursor(ctx, "matrix"); err == nil {
			since = v
		}
	}
	backoff := channels.NewBackoff(time.Second, maxSyncBackoff)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := c.sync(ctx, since)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Exponential backoff with jitter, capped, so a homeserver outage
			// doesn't turn into a synchronized hammer when it comes back.
			if err := backoff.Sleep(ctx); err != nil {
				return err
			}
			continue
		}
		backoff.Reset() // recovered — reset the ladder

		// First-ever sync (no since-token): deliver nothing. The filtered
		// request limited each timeline to one event, and even that one is
		// history from before the gateway existed — just record where "now" is.
		if since == "" {
			since = resp.NextBatch
			if c.store != nil {
				_ = c.store.SetCursor(ctx, "matrix", since)
			}
			continue
		}

		if err := c.deliverBatch(ctx, sink, resp); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Not durably recorded — do NOT advance since. The next sync from
			// the old token replays the batch; the router's dedup discards the
			// events that did land.
			if err := backoff.Sleep(ctx); err != nil {
				return err
			}
			continue
		}
		// Every delivery in the batch was durably recorded — only now may the
		// since-token advance (persisted, so a restart resumes here).
		since = resp.NextBatch
		if c.store != nil {
			_ = c.store.SetCursor(ctx, "matrix", since)
		}
	}
}

// deliverBatch hands every deliverable event in a sync response to the sink,
// room by room in timeline order. The first Deliver error aborts the batch so
// the caller can retry from the un-advanced since-token.
func (c *Channel) deliverBatch(ctx context.Context, sink channels.Sink, resp *syncResponse) error {
	for roomID, room := range resp.Rooms.Join {
		for _, ev := range room.Timeline.Events {
			inb, ok := c.toInbound(roomID, ev)
			if !ok {
				continue
			}
			if att, ok := c.download(ctx, ev.Content); ok {
				inb.Attachments = append(inb.Attachments, att)
			}
			if err := sink.Deliver(ctx, inb); err != nil {
				return err
			}
		}
	}
	return nil
}

// toInbound converts one timeline event to a normalized Inbound, or ok=false
// if it isn't a message meant for us: wrong type, our own echo (every /sync
// includes the events we ourselves send), an m.notice (bot-output convention —
// skipping them is the loop breaker between bots), or an empty body.
func (c *Channel) toInbound(roomID string, ev roomEvent) (channels.Inbound, bool) {
	if ev.Type != "m.room.message" {
		return channels.Inbound{}, false
	}
	if c.userID != "" && ev.Sender == c.userID {
		return channels.Inbound{}, false
	}
	if ev.Content.MsgType == "m.notice" {
		return channels.Inbound{}, false
	}
	if ev.Content.Body == "" {
		return channels.Inbound{}, false
	}
	text := ""
	switch ev.Content.MsgType {
	case "m.text":
		text = ev.Content.Body
	case "m.image", "m.audio", "m.file":
		// For media events the body is the filename by convention; only when a
		// separate content.filename is present AND differs is the body a caption.
		if ev.Content.Filename != "" && ev.Content.Body != ev.Content.Filename {
			text = ev.Content.Body
		}
	default:
		return channels.Inbound{}, false
	}
	return channels.Inbound{
		Channel:      "matrix",
		Conversation: roomID,
		// The mxid IS the stable id in Matrix (unlike a display name, it never
		// changes), so it's safe to authorize on directly.
		Principal: ev.Sender,
		Text:      text,
		MessageID: ev.EventID,
		IsDirect:  c.direct[roomID],
		Mentioned: c.mentionsMe(ev.Content),
	}, true
}

// mentionsMe reports whether the event addresses this account: the modern
// intentional-mentions field (m.mentions.user_ids), the mxid appearing in the
// body (how clients without intentional mentions render a pill), or a reply
// whose quoted fallback in formatted_body names the mxid. Never a bare
// display-name substring — display names are neither stable nor unique.
func (c *Channel) mentionsMe(content eventContent) bool {
	if c.userID == "" {
		return false
	}
	for _, id := range content.Mentions.UserIDs {
		if id == c.userID {
			return true
		}
	}
	if strings.Contains(content.Body, c.userID) {
		return true
	}
	if content.RelatesTo != nil && content.RelatesTo.InReplyTo != nil &&
		strings.Contains(content.FormattedBody, c.userID) {
		return true
	}
	return false
}

// download fetches a media event's mxc:// content into the media spool,
// best-effort: a failed download drops the attachment, the message itself
// still flows.
func (c *Channel) download(ctx context.Context, content eventContent) (channels.Attachment, bool) {
	if c.mediaDir == "" || content.URL == "" {
		return channels.Attachment{}, false
	}
	switch content.MsgType {
	case "m.image", "m.audio", "m.file":
	default:
		return channels.Attachment{}, false
	}
	server, mediaID, ok := parseMXC(content.URL)
	if !ok {
		return channels.Attachment{}, false
	}
	// The authenticated media endpoint (v1.11+) — the old unauthenticated
	// /_matrix/media path is deprecated and increasingly 404s.
	endpoint := fmt.Sprintf("%s/_matrix/client/v1/media/download/%s/%s",
		c.homeserver, url.PathEscape(server), url.PathEscape(mediaID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return channels.Attachment{}, false
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return channels.Attachment{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return channels.Attachment{}, false
	}
	att, err := channels.SaveToSpool(c.mediaDir, resp.Body, content.Info.MimeType, content.Body)
	if err != nil {
		return channels.Attachment{}, false
	}
	return att, true
}

// parseMXC splits an mxc://server/mediaId content URI.
func parseMXC(uri string) (server, mediaID string, ok bool) {
	rest, found := strings.CutPrefix(uri, "mxc://")
	if !found {
		return "", "", false
	}
	server, mediaID, found = strings.Cut(rest, "/")
	if !found || server == "" || mediaID == "" {
		return "", "", false
	}
	return server, mediaID, true
}

// whoami fetches this account's mxid. On any error it returns "", and the
// caller degrades safely (see Start).
func (c *Channel) whoami(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.homeserver+"/_matrix/client/v3/account/whoami", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	return out.UserID
}

// fetchDirectRooms reads the m.direct account data — the client convention
// that marks which rooms are DMs — and returns the set of DM room ids.
// Best-effort and fetched once: a room becoming a DM mid-run is rare enough
// that a restart picking it up is fine for v1.
func (c *Channel) fetchDirectRooms(ctx context.Context) map[string]bool {
	if c.userID == "" {
		return nil
	}
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/user/%s/account_data/m.direct",
		c.homeserver, url.PathEscape(c.userID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil
	}
	// m.direct maps peer mxid → list of room ids; we only need the room set.
	var byUser map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&byUser); err != nil {
		return nil
	}
	direct := make(map[string]bool)
	for _, rooms := range byUser {
		for _, id := range rooms {
			direct[id] = true
		}
	}
	return direct
}

// sync performs one /sync long-poll. An empty since means the first-ever sync,
// which carries a filter capping each room timeline at one event so a fresh
// gateway doesn't pull (and replay) the full history.
func (c *Channel) sync(ctx context.Context, since string) (*syncResponse, error) {
	q := url.Values{}
	q.Set("timeout", "30000")
	if since != "" {
		q.Set("since", since)
	} else {
		q.Set("filter", `{"room":{"timeline":{"limit":1}}}`)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.homeserver+"/_matrix/client/v3/sync?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("matrix sync: status %d", resp.StatusCode)
	}
	var out syncResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.NextBatch == "" {
		return nil, fmt.Errorf("matrix sync: response missing next_batch")
	}
	return &out, nil
}

// Send posts a reply to a room, split with the shared chunker. Replies go out
// as m.notice — the Matrix bot convention — so other bots (including a second
// gateway) skip them and no reply loop can form. A voice rendition, when
// present, is uploaded and sent as m.audio first, best-effort: any failure
// falls back silently to the text.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	if msg.VoicePath != "" {
		c.sendVoice(ctx, conversation, msg.VoicePath)
	}
	for _, part := range channels.Chunk(msg.Text, matrixMaxMessage) {
		body := map[string]any{"msgtype": "m.notice", "body": part}
		if err := c.putEvent(ctx, conversation, body); err != nil {
			return err
		}
	}
	return nil
}

// sendVoice uploads the OGG at path and posts it as an m.audio event.
// Best-effort by design: voice is an embellishment, the text notice that
// follows is the actual reply, so every error here is swallowed.
func (c *Channel) sendVoice(ctx context.Context, conversation, path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.homeserver+"/_matrix/media/v3/upload?filename=voice.ogg", f)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "audio/ogg")
	resp, err := c.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var out struct {
		ContentURI string `json:"content_uri"`
	}
	if resp.StatusCode/100 != 2 || json.NewDecoder(resp.Body).Decode(&out) != nil || out.ContentURI == "" {
		return
	}
	_ = c.putEvent(ctx, conversation, map[string]any{
		"msgtype": "m.audio",
		"body":    "voice message",
		"url":     out.ContentURI,
		"info":    map[string]any{"mimetype": "audio/ogg"},
	})
}

// putEvent sends one m.room.message event. The transaction id makes the PUT
// idempotent on the homeserver side, so a retried request can't double-post.
func (c *Channel) putEvent(ctx context.Context, roomID string, content map[string]any) error {
	body, err := json.Marshal(content)
	if err != nil {
		return err
	}
	txnID := fmt.Sprintf("memcode%d", time.Now().UnixNano())
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		c.homeserver, url.PathEscape(roomID), txnID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("matrix send: status %d", resp.StatusCode)
	}
	return nil
}
