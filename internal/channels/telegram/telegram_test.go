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
		var u update
		u.UpdateID = 1
		u.Message = &struct {
			From *struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"from"`
			Chat *struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			Text string `json:"text"`
		}{Text: text}
		if hasChat {
			u.Message.Chat = &struct {
				ID int64 `json:"id"`
			}{ID: chatID}
		}
		if hasFrom {
			u.Message.From = &struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			}{ID: fromID, Username: username}
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
		{"username", mk("do it", 42, true, "tim", 7, true), true, "42", "@tim", "do it"},
		{"no username uses id", mk("hey", 9, true, "", 7, true), true, "9", "7", "hey"},
		{"no from", mk("hi", 5, true, "", 0, false), true, "5", "", "hi"},
		{"empty text", mk("", 5, true, "tim", 7, true), false, "", "", ""},
		{"no chat", mk("hi", 0, false, "tim", 7, true), false, "", "", ""},
		{"nil message", update{UpdateID: 1}, false, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toInbound(tt.u)
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
