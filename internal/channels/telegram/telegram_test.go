package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
)

// fakeOffsetStore is an in-memory OffsetStore for tests.
type fakeOffsetStore struct {
	offset int64
	saved  int64
}

func (f *fakeOffsetStore) Offset(ctx context.Context, channel string) (int64, error) {
	return f.offset, nil
}
func (f *fakeOffsetStore) SetOffset(ctx context.Context, channel string, offset int64) error {
	f.saved = offset
	return nil
}

func TestToInbound(t *testing.T) {
	mk := func(text string, chatID int64, hasChat bool, username string, fromID int64, hasFrom bool) update {
		u := update{UpdateID: 1, Message: &tgMessage{Text: text}}
		if hasChat {
			u.Message.Chat = &tgChat{ID: chatID}
		}
		if hasFrom {
			u.Message.From = &tgUser{ID: fromID, Username: username}
		}
		return u
	}

	tests := []struct {
		name          string
		u             update
		wantOK        bool
		wantConvo     string
		wantPrincipal string
		wantText      string
	}{
		{"stable id, not username", mk("do it", 42, true, "tim", 7, true), true, "42", "7", "do it"},
		{"no username uses id", mk("hey", 9, true, "", 7, true), true, "9", "7", "hey"},
		{"no from", mk("hi", 5, true, "", 0, false), true, "5", "", "hi"},
		{"empty text", mk("", 5, true, "tim", 7, true), false, "", "", ""},
		{"no chat", mk("hi", 0, false, "tim", 7, true), false, "", "", ""},
		{"nil message", update{UpdateID: 1}, false, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toInbound(tt.u, 0, "")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			want := channels.Inbound{Channel: "telegram", Conversation: tt.wantConvo, Principal: tt.wantPrincipal, Text: tt.wantText, MessageID: "1"}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestGetUpdates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/botTOKEN/getUpdates") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, `{"ok":true,"result":[{"update_id":5,"message":{"text":"hi","chat":{"id":42},"from":{"id":7,"username":"tim"}}}]}`)
	}))
	defer srv.Close()

	c := New("TOKEN", nil)
	c.base = srv.URL
	ups, err := c.getUpdates(context.Background(), 0)
	if err != nil {
		t.Fatalf("getUpdates: %v", err)
	}
	if len(ups) != 1 || ups[0].UpdateID != 5 || ups[0].Message == nil || ups[0].Message.Text != "hi" {
		t.Fatalf("unexpected updates: %+v", ups)
	}
}

func TestGetUpdatesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":false,"description":"unauthorized"}`)
	}))
	defer srv.Close()

	c := New("TOKEN", nil)
	c.base = srv.URL
	if _, err := c.getUpdates(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("want unauthorized error, got %v", err)
	}
}

func TestStartLoadsPersistedOffset(t *testing.T) {
	gotOffset := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getUpdates") {
			select {
			case gotOffset <- r.URL.Query().Get("offset"):
			default:
			}
			io.WriteString(w, `{"ok":true,"result":[]}`)
		}
	}))
	defer srv.Close()

	c := New("TOKEN", &fakeOffsetStore{offset: 100})
	c.base = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Start(ctx, make(chan channels.Inbound, 1))

	select {
	case off := <-gotOffset:
		if off != "100" {
			t.Errorf("first poll used offset %q, want 100 (loaded from store)", off)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start never polled getUpdates")
	}
}

func TestDoSendRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":7}}`)
	}))
	defer srv.Close()

	c := New("TOKEN", nil)
	c.base = srv.URL
	status, retryAfter, err := c.doSend(context.Background(), "42", "hi")
	if err != nil {
		t.Fatalf("doSend: %v", err)
	}
	if status != http.StatusTooManyRequests || retryAfter != 7 {
		t.Errorf("got status=%d retryAfter=%d, want 429/7", status, retryAfter)
	}
}

func TestSend(t *testing.T) {
	var gotChat, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN/sendMessage" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotChat, gotText = body.ChatID, body.Text
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New("TOKEN", nil)
	c.base = srv.URL
	if err := c.Send(context.Background(), "42", channels.Outbound{Text: "yo"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotChat != "42" || gotText != "yo" {
		t.Errorf("server got chat=%q text=%q", gotChat, gotText)
	}
}

func TestGatingSignals(t *testing.T) {
	const botID = int64(555)
	const botUser = "memcodebot"

	// Private chat → IsDirect, no mention needed.
	priv := update{UpdateID: 1, Message: &tgMessage{
		Text: "do it", Chat: &tgChat{ID: 1, Type: "private"}, From: &tgUser{ID: 7},
	}}
	if inb, _ := toInbound(priv, botID, botUser); !inb.IsDirect || inb.Mentioned {
		t.Errorf("private: IsDirect=%v Mentioned=%v, want true/false", inb.IsDirect, inb.Mentioned)
	}

	// Group message, no mention → not direct, not mentioned.
	plain := update{UpdateID: 2, Message: &tgMessage{
		Text: "hi all", Chat: &tgChat{ID: -100, Type: "supergroup"}, From: &tgUser{ID: 7},
	}}
	if inb, _ := toInbound(plain, botID, botUser); inb.IsDirect || inb.Mentioned {
		t.Errorf("group plain: IsDirect=%v Mentioned=%v, want false/false", inb.IsDirect, inb.Mentioned)
	}

	// Group @mention of the bot → mentioned (entity-based).
	text := "@memcodebot do it"
	mentioned := update{UpdateID: 3, Message: &tgMessage{
		Text: text, Chat: &tgChat{ID: -100, Type: "supergroup"}, From: &tgUser{ID: 7},
		Entities: []tgEntity{{Type: "mention", Offset: 0, Length: len([]rune("@memcodebot"))}},
	}}
	if inb, _ := toInbound(mentioned, botID, botUser); !inb.Mentioned {
		t.Error("group @mention not detected")
	}

	// /command@botusername addressed to the bot → mentioned.
	cmd := "/start@memcodebot"
	command := update{UpdateID: 4, Message: &tgMessage{
		Text: cmd, Chat: &tgChat{ID: -100, Type: "group"}, From: &tgUser{ID: 7},
		Entities: []tgEntity{{Type: "bot_command", Offset: 0, Length: len([]rune(cmd))}},
	}}
	if inb, _ := toInbound(command, botID, botUser); !inb.Mentioned {
		t.Error("/command@bot not detected")
	}

	// Reply to one of the bot's messages → mentioned.
	reply := update{UpdateID: 5, Message: &tgMessage{
		Text: "thanks", Chat: &tgChat{ID: -100, Type: "group"}, From: &tgUser{ID: 7},
		ReplyToMessage: &tgMessage{From: &tgUser{ID: botID}},
	}}
	if inb, _ := toInbound(reply, botID, botUser); !inb.Mentioned {
		t.Error("reply-to-bot not treated as a mention")
	}

	// A mention of a DIFFERENT bot must not trigger.
	other := "@someoneelse hi"
	othermention := update{UpdateID: 6, Message: &tgMessage{
		Text: other, Chat: &tgChat{ID: -100, Type: "group"}, From: &tgUser{ID: 7},
		Entities: []tgEntity{{Type: "mention", Offset: 0, Length: len([]rune("@someoneelse"))}},
	}}
	if inb, _ := toInbound(othermention, botID, botUser); inb.Mentioned {
		t.Error("mention of another user should not count as addressing this bot")
	}
}
