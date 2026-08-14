package googlechat

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/memcode-ai/memcode/internal/channels"
)

const testAudience = "123456789"

type captureSink struct {
	got []channels.Inbound
	err error
}

func (s *captureSink) Deliver(_ context.Context, inb channels.Inbound) error {
	if s.err != nil {
		return s.err
	}
	s.got = append(s.got, inb)
	return nil
}

// signJWT builds an RS256 JWT signed with key, mimicking Google's event token.
func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// jwksServer serves the key's public half as a JWKS document under kid.
func jwksServer(t *testing.T, key *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{"keys": []map[string]string{{
			"kty": "RSA",
			"kid": kid,
			"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}}}
		_ = json.NewEncoder(w).Encode(doc)
	}))
}

func testChannel(t *testing.T, jwks *httptest.Server) *Channel {
	t.Helper()
	c := New(nil, testAudience, "")
	c.jwksURL = jwks.URL
	return c
}

func validClaims() map[string]any {
	return map[string]any{"iss": chatIssuer, "aud": testAudience, "exp": 4102444800} // year 2100
}

func postEvent(t *testing.T, h http.Handler, bearer string, ev any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/googlechat", strings.NewReader(string(body)))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func messageEvent(spaceType, text, argumentText string, mention bool) map[string]any {
	msg := map[string]any{
		"name":   "spaces/AAA/messages/BBB.CCC",
		"text":   text,
		"sender": map[string]any{"name": "users/42", "type": "HUMAN"},
	}
	if argumentText != "" {
		msg["argumentText"] = argumentText
	}
	if mention {
		msg["annotations"] = []map[string]any{{"type": "USER_MENTION"}}
	}
	return map[string]any{
		"type":    "MESSAGE",
		"message": msg,
		"space":   map[string]any{"name": "spaces/AAA", "spaceType": spaceType},
	}
}

func TestHandlerSignedRoundTrip(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := jwksServer(t, key, "k1")
	defer jwks.Close()
	c := testChannel(t, jwks)
	sink := &captureSink{}
	h := c.Handler(sink)
	tok := signJWT(t, key, "k1", validClaims())

	t.Run("dm", func(t *testing.T) {
		w := postEvent(t, h, tok, messageEvent("DIRECT_MESSAGE", "hello there", "", false))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Body.String(); got != "{}" {
			t.Fatalf("body = %q, want {}", got)
		}
		if len(sink.got) != 1 {
			t.Fatalf("delivered %d messages, want 1", len(sink.got))
		}
		inb := sink.got[0]
		want := channels.Inbound{
			Channel:      "googlechat",
			Conversation: "spaces/AAA",
			Principal:    "users/42",
			Text:         "hello there",
			MessageID:    "spaces/AAA/messages/BBB.CCC",
			IsDirect:     true,
		}
		if inb.Channel != want.Channel || inb.Conversation != want.Conversation ||
			inb.Principal != want.Principal || inb.Text != want.Text ||
			inb.MessageID != want.MessageID || inb.IsDirect != want.IsDirect ||
			inb.Mentioned || inb.Trusted {
			t.Fatalf("inbound = %+v, want %+v", inb, want)
		}
	})

	t.Run("space mention prefers argumentText", func(t *testing.T) {
		sink.got = nil
		w := postEvent(t, h, tok, messageEvent("SPACE", "@membot do the thing", "do the thing", true))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if len(sink.got) != 1 {
			t.Fatalf("delivered %d messages, want 1", len(sink.got))
		}
		inb := sink.got[0]
		if inb.Text != "do the thing" {
			t.Fatalf("text = %q, want argumentText preferred", inb.Text)
		}
		if inb.IsDirect || !inb.Mentioned {
			t.Fatalf("isDirect=%v mentioned=%v, want false/true", inb.IsDirect, inb.Mentioned)
		}
	})

	t.Run("legacy DM type field", func(t *testing.T) {
		sink.got = nil
		ev := messageEvent("", "hi", "", false)
		ev["space"] = map[string]any{"name": "spaces/AAA", "type": "DM"}
		postEvent(t, h, tok, ev)
		if len(sink.got) != 1 || !sink.got[0].IsDirect {
			t.Fatalf("legacy DM not mapped: %+v", sink.got)
		}
	})

	t.Run("non-message event acked without delivery", func(t *testing.T) {
		sink.got = nil
		w := postEvent(t, h, tok, map[string]any{"type": "ADDED_TO_SPACE"})
		if w.Code != http.StatusOK || len(sink.got) != 0 {
			t.Fatalf("status=%d delivered=%d, want 200/0", w.Code, len(sink.got))
		}
	})
}

func TestHandlerRejectsBadAuth(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := jwksServer(t, key, "k1")
	defer jwks.Close()
	c := testChannel(t, jwks)
	sink := &captureSink{}
	h := c.Handler(sink)
	ev := messageEvent("DIRECT_MESSAGE", "hi", "", false)

	cases := map[string]string{
		"missing token": "",
		"wrong aud":     signJWT(t, key, "k1", map[string]any{"iss": chatIssuer, "aud": "999", "exp": 4102444800}),
		"wrong issuer":  signJWT(t, key, "k1", map[string]any{"iss": "evil@example.com", "aud": testAudience, "exp": 4102444800}),
		"expired":       signJWT(t, key, "k1", map[string]any{"iss": chatIssuer, "aud": testAudience, "exp": 1000}),
		"bad signature": signJWT(t, key, "k1", validClaims()) + "x",
	}
	// A token signed by a key Google never published must also fail.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases["unpublished key"] = signJWT(t, other, "k1", validClaims())

	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			w := postEvent(t, h, tok, ev)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			if len(sink.got) != 0 {
				t.Fatalf("delivered %d messages, want 0", len(sink.got))
			}
		})
	}
}

func TestHandlerDeliverFailure(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := jwksServer(t, key, "k1")
	defer jwks.Close()
	c := testChannel(t, jwks)
	sink := &captureSink{err: fmt.Errorf("inbox down")}
	tok := signJWT(t, key, "k1", validClaims())
	w := postEvent(t, c.Handler(sink), tok, messageEvent("DIRECT_MESSAGE", "hi", "", false))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestHandlerSkipsBotSender(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := jwksServer(t, key, "k1")
	defer jwks.Close()
	c := testChannel(t, jwks)
	sink := &captureSink{}
	tok := signJWT(t, key, "k1", validClaims())
	ev := messageEvent("SPACE", "bot chatter", "", false)
	ev["message"].(map[string]any)["sender"] = map[string]any{"name": "users/bot", "type": "BOT"}
	w := postEvent(t, c.Handler(sink), tok, ev)
	if w.Code != http.StatusOK || len(sink.got) != 0 {
		t.Fatalf("status=%d delivered=%d, want 200/0", w.Code, len(sink.got))
	}
}

func TestSendChunksWithBearer(t *testing.T) {
	type sent struct {
		path, bearer, text string
	}
	var got []sent
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(body, &msg)
		got = append(got, sent{r.URL.Path, r.Header.Get("Authorization"), msg.Text})
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	c := New(nil, testAudience, "")
	c.apiBase = api.URL
	c.tokenSource = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"})

	long := strings.Repeat("a", chatMaxMessage+10)
	if err := c.Send(context.Background(), "spaces/AAA", channels.Outbound{Text: long}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("sent %d messages, want 2 chunks", len(got))
	}
	var rejoined string
	for _, s := range got {
		if s.path != "/v1/spaces/AAA/messages" {
			t.Fatalf("path = %q", s.path)
		}
		if s.bearer != "Bearer test-token" {
			t.Fatalf("bearer = %q", s.bearer)
		}
		if len([]rune(s.text)) > chatMaxMessage {
			t.Fatalf("chunk of %d runes exceeds cap %d", len([]rune(s.text)), chatMaxMessage)
		}
		rejoined += s.text
	}
	if rejoined != long {
		t.Fatal("chunking lost content")
	}
}

func TestSendFailsFastOnAPIError(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer api.Close()
	c := New(nil, testAudience, "")
	c.apiBase = api.URL
	c.tokenSource = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t"})
	if err := c.Send(context.Background(), "spaces/AAA", channels.Outbound{Text: "hi"}); err == nil {
		t.Fatal("want error on non-2xx send")
	}
}
