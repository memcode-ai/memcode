package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/memcode-ai/memcode/internal/channels"
)

// postedEvent builds a raw "posted" websocket frame with the post payload
// double-encoded, exactly as Mattermost sends it.
func postedEvent(t *testing.T, p post, channelType, senderName string) []byte {
	t.Helper()
	pj, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	ev := map[string]any{
		"event": "posted",
		"data": map[string]any{
			"post":         string(pj), // the double-encoding under test
			"channel_type": channelType,
			"sender_name":  senderName,
		},
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseEventPosted(t *testing.T) {
	raw := postedEvent(t, post{
		ID:        "post1",
		UserID:    "user1",
		ChannelID: "chan1",
		Message:   "hey @membot fix the build",
	}, "O", "@alice")
	inb, files, ok := parseEvent(raw, "botid", "membot")
	if !ok {
		t.Fatal("expected ok")
	}
	want := channels.Inbound{
		Channel:      "mattermost",
		Conversation: "chan1",
		Principal:    "user1",
		Text:         "hey @membot fix the build",
		MessageID:    "post1",
		IsDirect:     false,
		Mentioned:    true,
	}
	if fmt.Sprintf("%+v", inb) != fmt.Sprintf("%+v", want) {
		t.Fatalf("inbound = %+v, want %+v", inb, want)
	}
	if len(files) != 0 {
		t.Fatalf("files = %v, want none", files)
	}

	// A DM is direct regardless of mention, and file ids flow through.
	raw = postedEvent(t, post{
		ID: "post2", UserID: "user1", ChannelID: "dm1",
		Message: "here you go", FileIDs: []string{"f1", "f2"},
	}, "D", "@alice")
	inb, files, ok = parseEvent(raw, "botid", "membot")
	if !ok {
		t.Fatal("expected ok")
	}
	if !inb.IsDirect {
		t.Fatal("channel_type D must map to IsDirect")
	}
	if inb.Mentioned {
		t.Fatal("no @membot in text — Mentioned must be false")
	}
	if len(files) != 2 || files[0] != "f1" || files[1] != "f2" {
		t.Fatalf("files = %v, want [f1 f2]", files)
	}
}

func TestParseEventSkips(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"own post", postedEvent(t, post{ID: "p", UserID: "botid", ChannelID: "c", Message: "echo"}, "D", "@membot")},
		{"system message", postedEvent(t, post{ID: "p", UserID: "u", ChannelID: "c", Message: "u joined", Type: "system_join_channel"}, "O", "@u")},
		{"empty with no files", postedEvent(t, post{ID: "p", UserID: "u", ChannelID: "c", Message: "  "}, "D", "@u")},
		{"other event type", []byte(`{"event":"typing","data":{}}`)},
		{"malformed post payload", []byte(`{"event":"posted","data":{"post":"{not json"}}`)},
	}
	for _, tc := range cases {
		if _, _, ok := parseEvent(tc.raw, "botid", "membot"); ok {
			t.Errorf("%s: expected skip", tc.name)
		}
	}

	// A file-only post (empty message, files attached) must still flow.
	raw := postedEvent(t, post{ID: "p", UserID: "u", ChannelID: "c", FileIDs: []string{"f1"}}, "D", "@u")
	if _, _, ok := parseEvent(raw, "botid", "membot"); !ok {
		t.Error("file-only post: expected ok")
	}
}

func TestMentionsUser(t *testing.T) {
	cases := []struct {
		text, username string
		want           bool
	}{
		{"hey @membot do it", "membot", true},
		{"@membot", "membot", true},                       // end of string is a boundary
		{"@membot, please", "membot", true},               // punctuation is a boundary
		{"@membot: ship it", "membot", true},              // colon after a mention
		{"ping @MemBot now", "membot", true},              // case-insensitive
		{"@membots are cool", "membot", false},            // longer handle, not us
		{"@membot2 is someone else", "membot", false},     // trailing digit extends the handle
		{"mail me at a@membot", "membot", true},           // no leading-boundary requirement
		{"@membots yes but also @membot", "membot", true}, // later occurrence still matches
		{"no mention here", "membot", false},
		{"@ membot spaced out", "membot", false},
		{"anything", "", false}, // unknown own username never matches
	}
	for _, tc := range cases {
		if got := mentionsUser(tc.text, tc.username); got != tc.want {
			t.Errorf("mentionsUser(%q, %q) = %v, want %v", tc.text, tc.username, got, tc.want)
		}
	}
}

func TestDeriveWSURL(t *testing.T) {
	cases := []struct {
		base, want string
	}{
		{"https://mm.example.com", "wss://mm.example.com/api/v4/websocket"},
		{"http://127.0.0.1:8065", "ws://127.0.0.1:8065/api/v4/websocket"},
		{"https://example.com/mattermost", "wss://example.com/mattermost/api/v4/websocket"},
	}
	for _, tc := range cases {
		got, err := deriveWSURL(tc.base)
		if err != nil {
			t.Fatalf("deriveWSURL(%q): %v", tc.base, err)
		}
		if got != tc.want {
			t.Errorf("deriveWSURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
	if _, err := deriveWSURL("ftp://example.com"); err == nil {
		t.Error("ftp scheme: expected error")
	}
}

func TestSendChunksWithBearer(t *testing.T) {
	var mu sync.Mutex
	var messages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v4/posts" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer tok")
		}
		var body struct {
			ChannelID string `json:"channel_id"`
			Message   string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.ChannelID != "chan1" {
			t.Errorf("channel_id = %q, want chan1", body.ChannelID)
		}
		mu.Lock()
		messages = append(messages, body.Message)
		mu.Unlock()
		fmt.Fprint(w, `{"id":"newpost"}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", "")
	long := strings.Repeat("a", 2*mattermostMaxMessage+100)
	if err := c.Send(context.Background(), "chan1", channels.Outbound{Text: long}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 3 {
		t.Fatalf("got %d posts, want 3", len(messages))
	}
	for i, m := range messages {
		if len(m) > mattermostMaxMessage {
			t.Errorf("part %d is %d chars, over the cap", i, len(m))
		}
	}
	if strings.Join(messages, "") != long {
		t.Error("chunked parts don't reassemble to the original text")
	}
}

func TestSendFailsFastOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer srv.Close()
	c := New(srv.URL, "tok", "")
	if err := c.Send(context.Background(), "chan1", channels.Outbound{Text: "hi"}); err == nil {
		t.Fatal("expected error on 403")
	}
}

// sinkFunc adapts a func to channels.Sink.
type sinkFunc func(ctx context.Context, inb channels.Inbound) error

func (f sinkFunc) Deliver(ctx context.Context, inb channels.Inbound) error { return f(ctx, inb) }

// TestStartWebSocketRoundTrip runs the full path: users/me, the websocket
// upgrade with bearer auth, the in-band authentication challenge, one posted
// event delivered to the sink, then a clean ctx.Err() shutdown.
func TestStartWebSocketRoundTrip(t *testing.T) {
	upgrader := websocket.Upgrader{}
	event := postedEvent(t, post{
		ID: "p1", UserID: "u1", ChannelID: "dm1", Message: "hello @membot",
	}, "D", "@alice")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer tok")
		}
		switch r.URL.Path {
		case "/api/v4/users/me":
			fmt.Fprint(w, `{"id":"botid","username":"membot"}`)
		case "/api/v4/websocket":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer conn.Close()
			// Expect the documented in-band auth challenge before any events.
			var chal struct {
				Action string `json:"action"`
				Data   struct {
					Token string `json:"token"`
				} `json:"data"`
			}
			if err := conn.ReadJSON(&chal); err != nil {
				t.Errorf("read challenge: %v", err)
				return
			}
			if chal.Action != "authentication_challenge" || chal.Data.Token != "tok" {
				t.Errorf("challenge = %+v, want authentication_challenge with token", chal)
			}
			if err := conn.WriteMessage(websocket.TextMessage, event); err != nil {
				return
			}
			// Hold the socket open until the client hangs up on ctx cancel.
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got := make(chan channels.Inbound, 1)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	c := New(srv.URL, "tok", "")
	go func() {
		errCh <- c.Start(ctx, sinkFunc(func(_ context.Context, inb channels.Inbound) error {
			got <- inb
			return nil
		}))
	}()

	select {
	case inb := <-got:
		if inb.Principal != "u1" || inb.Conversation != "dm1" || !inb.IsDirect || !inb.Mentioned {
			t.Errorf("inbound = %+v", inb)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for inbound")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Errorf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}
