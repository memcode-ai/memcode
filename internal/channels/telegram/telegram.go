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
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

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
	token    string
	base     string // API base; overridable in tests
	client   *http.Client
	store    OffsetStore
	mediaDir string // media spool; "" disables attachment downloads
}

// New builds a Telegram channel for the given bot token. store may be nil, in
// which case the poll offset lives only in memory (and a restart re-reads the
// backlog, which the router's dedup then discards). mediaDir is the gateway
// media spool photos/voice notes/documents are downloaded into; "" disables
// attachment handling (messages still flow as text).
func New(token string, store OffsetStore, mediaDir string) *Channel {
	return &Channel{
		token: token,
		base:  defaultBase,
		// The HTTP timeout must exceed the long-poll timeout so getUpdates can
		// block server-side for the full window without the client giving up.
		client:   &http.Client{Timeout: 65 * time.Second},
		store:    store,
		mediaDir: mediaDir,
	}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "telegram" }

// update mirrors the fields we use from a Telegram Update. Named sub-types (not
// anonymous structs) so they're straightforward to build in tests.
type update struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgMessage struct {
	From           *tgUser       `json:"from"`
	Chat           *tgChat       `json:"chat"`
	Text           string        `json:"text"`
	Caption        string        `json:"caption"`
	Entities       []tgEntity    `json:"entities"`
	ReplyToMessage *tgMessage    `json:"reply_to_message"`
	Photo          []tgPhotoSize `json:"photo"`
	Voice          *tgFileMeta   `json:"voice"`
	Audio          *tgFileMeta   `json:"audio"`
	Document       *tgFileMeta   `json:"document"`
}

type tgPhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
}

type tgFileMeta struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type"`
	FileName string `json:"file_name"`
}

type tgUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private" for a DM; "group"/"supergroup"/… otherwise
}

type tgEntity struct {
	Type   string `json:"type"` // "mention", "bot_command", …
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// Start long-polls getUpdates and forwards each text message as an Inbound until
// ctx is cancelled. The ack cursor is loaded from (and saved to) the offset store
// so a restart resumes where it left off. Transient errors back off with jitter
// rather than returning, so a flaky network never takes the gateway down.
func (c *Channel) Start(ctx context.Context, sink channels.Sink) error {
	// Learn our own id and username so we can detect being addressed in a group.
	// If getMe fails the bot still serves DMs; group messages just won't be seen
	// as mentions (so they won't trigger unless respond_to_all) — the safe default.
	botID, botUsername := c.getMe(ctx)

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
			if inb, refs, ok := toInbound(u, botID, botUsername); ok {
				inb.Attachments = c.download(ctx, refs)
				if err := sink.Deliver(ctx, inb); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					// Not durably recorded — don't advance past this update; the
					// next poll re-fetches it from the un-advanced offset.
					break
				}
			}
			// Advance the ack cursor only after the message was durably recorded
			// (or it wasn't a message). Persisted so a restart resumes here.
			offset = u.UpdateID + 1
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

// fileRef names one downloadable piece of media on a message.
type fileRef struct {
	fileID string
	mime   string
	name   string
}

// toInbound converts a Telegram update to a normalized Inbound plus the media
// it references, or ok=false if it carries neither text nor media.
// botID/botUsername identify this bot so a group message can be recognized as
// addressed to it.
func toInbound(u update, botID int64, botUsername string) (channels.Inbound, []fileRef, bool) {
	if u.Message == nil || u.Message.Chat == nil {
		return channels.Inbound{}, nil, false
	}
	m := u.Message
	text := m.Text
	if text == "" {
		text = m.Caption // a photo/voice with a caption: the caption is the task text
	}
	var refs []fileRef
	if len(m.Photo) > 0 {
		best := m.Photo[0]
		for _, p := range m.Photo[1:] { // Telegram lists sizes ascending; take the largest
			if p.FileSize >= best.FileSize {
				best = p
			}
		}
		refs = append(refs, fileRef{fileID: best.FileID, mime: "image/jpeg", name: "photo.jpg"})
	}
	if v := m.Voice; v != nil && v.FileID != "" {
		refs = append(refs, fileRef{fileID: v.FileID, mime: orMime(v.MimeType, "audio/ogg"), name: "voice.ogg"})
	}
	if a := m.Audio; a != nil && a.FileID != "" {
		refs = append(refs, fileRef{fileID: a.FileID, mime: orMime(a.MimeType, "audio/mpeg"), name: orName(a.FileName, "audio")})
	}
	if d := m.Document; d != nil && d.FileID != "" {
		refs = append(refs, fileRef{fileID: d.FileID, mime: d.MimeType, name: orName(d.FileName, "document")})
	}
	if text == "" && len(refs) == 0 {
		return channels.Inbound{}, nil, false
	}
	// Principal is the STABLE numeric user id, never the mutable @username — the
	// allow-list authorizes on ids so a username change (or a lookalike handle)
	// can't grant or revoke access.
	principal := ""
	if f := m.From; f != nil {
		principal = strconv.FormatInt(f.ID, 10)
	}
	return channels.Inbound{
		Channel:      "telegram",
		Conversation: strconv.FormatInt(m.Chat.ID, 10),
		Principal:    principal,
		Text:         text,
		MessageID:    strconv.FormatInt(u.UpdateID, 10),
		IsDirect:     m.Chat.Type == "private",
		Mentioned:    mentionsBot(u, botID, botUsername),
	}, refs, true
}

func orMime(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func orName(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// download fetches each referenced file into the media spool (getFile → file
// download endpoint), best-effort: a failed download drops that attachment, the
// message itself still flows.
func (c *Channel) download(ctx context.Context, refs []fileRef) []channels.Attachment {
	if c.mediaDir == "" || len(refs) == 0 {
		return nil
	}
	var out []channels.Attachment
	for _, ref := range refs {
		att, err := c.downloadOne(ctx, ref)
		if err != nil {
			continue
		}
		out = append(out, att)
	}
	return out
}

func (c *Channel) downloadOne(ctx context.Context, ref fileRef) (channels.Attachment, error) {
	// getFile resolves the file_id to a downloadable path.
	endpoint := fmt.Sprintf("%s/bot%s/getFile?file_id=%s", c.base, c.token, url.QueryEscape(ref.fileID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return channels.Attachment{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return channels.Attachment{}, err
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.OK || out.Result.FilePath == "" {
		return channels.Attachment{}, fmt.Errorf("telegram getFile failed")
	}
	dl := fmt.Sprintf("%s/file/bot%s/%s", c.base, c.token, out.Result.FilePath)
	dreq, err := http.NewRequestWithContext(ctx, http.MethodGet, dl, nil)
	if err != nil {
		return channels.Attachment{}, err
	}
	dresp, err := c.client.Do(dreq)
	if err != nil {
		return channels.Attachment{}, err
	}
	defer dresp.Body.Close()
	if dresp.StatusCode/100 != 2 {
		return channels.Attachment{}, fmt.Errorf("telegram file download: status %d", dresp.StatusCode)
	}
	return channels.SaveToSpool(c.mediaDir, dresp.Body, ref.mime, ref.name)
}

// mentionsBot reports whether the message addresses this bot: a reply to one of
// its messages, a @mention entity for its username, or a /command@botusername.
// Entity text is sliced with UTF-16 offsets (Telegram's unit), not bytes.
func mentionsBot(u update, botID int64, botUsername string) bool {
	m := u.Message
	if botID != 0 && m.ReplyToMessage != nil && m.ReplyToMessage.From != nil && m.ReplyToMessage.From.ID == botID {
		return true
	}
	if botUsername == "" {
		return false
	}
	want := "@" + strings.ToLower(botUsername)
	for _, e := range m.Entities {
		switch e.Type {
		case "mention":
			if strings.ToLower(entityText(m.Text, e.Offset, e.Length)) == want {
				return true
			}
		case "bot_command":
			if strings.Contains(strings.ToLower(entityText(m.Text, e.Offset, e.Length)), want) {
				return true
			}
		}
	}
	return false
}

// entityText extracts the substring a Telegram entity covers. Offsets/lengths are
// in UTF-16 code units, so we encode to UTF-16 before slicing.
func entityText(text string, offset, length int) string {
	u := utf16.Encode([]rune(text))
	if offset < 0 || length < 0 || offset+length > len(u) {
		return ""
	}
	return string(utf16.Decode(u[offset : offset+length]))
}

// getMe fetches this bot's id and username. On any error it returns zero values,
// and the caller degrades safely (DMs still work; group mentions won't match).
func (c *Channel) getMe(ctx context.Context) (int64, string) {
	endpoint := fmt.Sprintf("%s/bot%s/getMe", c.base, c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, ""
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.OK {
		return 0, ""
	}
	return out.Result.ID, out.Result.Username
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
// error (any other non-2xx) fails fast instead of retrying forever. A
// synthesized voice rendition (VoicePath, OGG/Opus) is sent first as a native
// voice bubble, best-effort — the text always follows.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	if msg.VoicePath != "" {
		_ = c.sendVoice(ctx, conversation, msg.VoicePath) // best-effort; text is the reply of record
	}
	for _, part := range channels.Chunk(msg.Text, telegramMaxMessage) {
		if err := c.sendOne(ctx, conversation, part); err != nil {
			return err
		}
	}
	return nil
}

// sendVoice uploads an OGG/Opus file as a voice message (multipart sendVoice).
func (c *Channel) sendVoice(ctx context.Context, conversation, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("chat_id", conversation); err != nil {
		return err
	}
	fw, err := mw.CreateFormFile("voice", "voice.ogg")
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendVoice", c.base, c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram sendVoice: status %d", resp.StatusCode)
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
