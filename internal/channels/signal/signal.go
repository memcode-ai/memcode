// Package signal is the gateway's Signal channel adapter. It talks to a
// signal-cli daemon running in native HTTP mode (`signal-cli daemon --http`):
// inbound messages arrive over the daemon's SSE event stream, replies go out as
// JSON-RPC `send` calls. Nobody embeds libsignal — both Hermes and OpenClaw
// require the same companion daemon; we spawn nothing and just connect to it
// (SIGNAL_CLI_URL, default http://127.0.0.1:8080). The account is a LINKED
// DEVICE (`signal-cli link`) — use a dedicated number, not your main one.
//
// Attachments: the daemon writes them to its own data directory; the adapter
// reads them from there (attachmentsDir) and copies them into the media spool.
package signal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
)

const (
	maxBackoff = 60 * time.Second
	// signalMaxMessage keeps outbound parts well under Signal's practical cap.
	signalMaxMessage = 4000
	// groupPrefix marks a group conversation id (a DM conversation is the peer's
	// number/uuid directly, so replies know which JSON-RPC shape to use).
	groupPrefix = "group:"
)

// Channel is a connection to a signal-cli HTTP daemon.
type Channel struct {
	baseURL        string // daemon base, e.g. http://127.0.0.1:8080
	account        string // our own E.164 number (loop prevention + mention detection)
	attachmentsDir string // signal-cli's attachment store; "" disables media
	mediaDir       string // gateway media spool; "" disables media
	client         *http.Client
	sse            *http.Client // no timeout: the event stream is long-lived
}

// New builds a Signal channel. baseURL "" uses the local daemon default.
func New(baseURL, account, attachmentsDir, mediaDir string) *Channel {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080"
	}
	return &Channel{
		baseURL:        strings.TrimRight(baseURL, "/"),
		account:        strings.TrimSpace(account),
		attachmentsDir: attachmentsDir,
		mediaDir:       mediaDir,
		client:         &http.Client{Timeout: 30 * time.Second},
		sse:            &http.Client{},
	}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "signal" }

// envelope mirrors the signal-cli receive envelope fields we use.
type envelope struct {
	Envelope struct {
		SourceNumber string `json:"sourceNumber"`
		SourceUUID   string `json:"sourceUuid"`
		Timestamp    int64  `json:"timestamp"`
		DataMessage  *struct {
			Message   string `json:"message"`
			GroupInfo *struct {
				GroupID string `json:"groupId"`
			} `json:"groupInfo"`
			Mentions []struct {
				Number string `json:"number"`
				UUID   string `json:"uuid"`
			} `json:"mentions"`
			Quote *struct {
				Author string `json:"author"`
			} `json:"quote"`
			Attachments []struct {
				ContentType string `json:"contentType"`
				Filename    string `json:"filename"`
				ID          string `json:"id"`
			} `json:"attachments"`
		} `json:"dataMessage"`
	} `json:"envelope"`
}

// Start consumes the daemon's SSE event stream until ctx is cancelled,
// reconnecting with jittered backoff on any error. Delivery is best-effort
// like Discord's websocket: the daemon has no per-message replay, so a failed
// durable record can't be re-driven from the provider side.
func (c *Channel) Start(ctx context.Context, sink channels.Sink) error {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.stream(ctx, sink)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// stream opens one SSE connection and processes events until it breaks.
func (c *Channel) stream(ctx context.Context, sink channels.Sink) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.sse.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("signal events: status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "" && data.Len() > 0:
			c.handleEvent(ctx, sink, data.Bytes())
			data.Reset()
		}
	}
	return sc.Err()
}

// handleEvent parses one SSE payload and forwards a usable message.
func (c *Channel) handleEvent(ctx context.Context, sink channels.Sink, raw []byte) {
	inb, refs, ok := parseEnvelope(raw, c.account)
	if !ok {
		return
	}
	inb.Attachments = c.collect(refs)
	_ = sink.Deliver(ctx, inb) // best-effort; see Start
}

// attachmentRef names one attachment in the daemon's store.
type attachmentRef struct {
	id   string
	mime string
	name string
}

// parseEnvelope normalizes a signal-cli receive envelope, or ok=false for
// receipts/typing/own messages/empty events. account is our own number.
func parseEnvelope(raw []byte, account string) (channels.Inbound, []attachmentRef, bool) {
	var ev envelope
	if err := json.Unmarshal(raw, &ev); err != nil {
		return channels.Inbound{}, nil, false
	}
	env := ev.Envelope
	dm := env.DataMessage
	if dm == nil {
		return channels.Inbound{}, nil, false // receipt/typing/sync — not a message
	}
	// Principal: the account UUID — Signal's STABLE identity. A phone number can
	// change hands or be re-registered, so it is only the fallback when the
	// daemon didn't surface a uuid. The pairing flow makes uuids painless to
	// allow-list (nobody has to type one). Our own messages are skipped (loops).
	principal := env.SourceUUID
	if principal == "" {
		principal = env.SourceNumber
	}
	if principal == "" || env.SourceNumber == account || principal == account {
		return channels.Inbound{}, nil, false
	}
	var refs []attachmentRef
	for _, a := range dm.Attachments {
		if a.ID == "" {
			continue
		}
		refs = append(refs, attachmentRef{id: a.ID, mime: a.ContentType, name: a.Filename})
	}
	if strings.TrimSpace(dm.Message) == "" && len(refs) == 0 {
		return channels.Inbound{}, nil, false
	}
	isDirect := dm.GroupInfo == nil || dm.GroupInfo.GroupID == ""
	conversation := principal
	if !isDirect {
		conversation = groupPrefix + dm.GroupInfo.GroupID
	}
	// Mentioned: an explicit @mention of our number, or a quote-reply to us.
	mentioned := false
	if account != "" {
		for _, m := range dm.Mentions {
			if m.Number == account {
				mentioned = true
			}
		}
		if dm.Quote != nil && dm.Quote.Author == account {
			mentioned = true
		}
	}
	return channels.Inbound{
		Channel:      "signal",
		Conversation: conversation,
		Principal:    principal,
		Text:         dm.Message,
		// Signal's message identity is (sender, timestamp) — that pair is what
		// receipts and quotes reference, so it's the stable dedup key.
		MessageID: fmt.Sprintf("%s:%d", principal, env.Timestamp),
		IsDirect:  isDirect,
		Mentioned: mentioned,
	}, refs, true
}

// collect copies referenced attachments from the daemon's store into the media
// spool, best-effort. IDs are treated as bare filenames — anything else is
// skipped rather than resolved outside the store.
func (c *Channel) collect(refs []attachmentRef) []channels.Attachment {
	if c.mediaDir == "" || c.attachmentsDir == "" || len(refs) == 0 {
		return nil
	}
	var out []channels.Attachment
	for _, ref := range refs {
		if ref.id == "" || ref.id != filepath.Base(ref.id) || strings.HasPrefix(ref.id, ".") {
			continue
		}
		f, err := os.Open(filepath.Join(c.attachmentsDir, ref.id))
		if err != nil {
			continue
		}
		name := ref.name
		if name == "" {
			name = ref.id
		}
		att, err := channels.SaveToSpool(c.mediaDir, f, ref.mime, name)
		f.Close()
		if err != nil {
			continue
		}
		out = append(out, att)
	}
	return out
}

// rpcRequest is one JSON-RPC 2.0 call to the daemon.
type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int64          `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

// Send posts a reply — to a peer (DM) or a group — as JSON-RPC send calls,
// split with the shared chunker. A synthesized voice note (VoicePath) rides
// the first part as an attachment path; the daemon runs on the same machine,
// so the spool path is directly readable.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	parts := channels.Chunk(msg.Text, signalMaxMessage)
	for i, part := range parts {
		params := map[string]any{"message": part}
		if gid, ok := strings.CutPrefix(conversation, groupPrefix); ok {
			params["groupId"] = gid
		} else {
			params["recipient"] = []string{conversation}
		}
		if i == 0 && msg.VoicePath != "" {
			params["attachments"] = []string{msg.VoicePath}
		}
		if err := c.rpc(ctx, "send", params); err != nil {
			return err
		}
	}
	return nil
}

func (c *Channel) rpc(ctx context.Context, method string, params map[string]any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: time.Now().UnixNano(), Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/rpc", bytes.NewReader(body))
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
		return fmt.Errorf("signal %s: status %d", method, resp.StatusCode)
	}
	var out struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err == nil && out.Error != nil {
		return fmt.Errorf("signal %s: %s", method, out.Error.Message)
	}
	return nil
}

// jitter scales d by [0.75, 1.25) so reconnects don't resonate.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.75 + rand.Float64()*0.5))
}
