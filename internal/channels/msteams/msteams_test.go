package msteams

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/memcode-ai/memcode/internal/channels"
)

const testAppID = "app-id-123"

// recordingSink captures delivered inbounds; err (when set) makes Deliver fail.
type recordingSink struct {
	got []channels.Inbound
	err error
}

func (s *recordingSink) Deliver(_ context.Context, inb channels.Inbound) error {
	if s.err != nil {
		return s.err
	}
	s.got = append(s.got, inb)
	return nil
}

// jwksServer serves a fake Bot Framework OpenID config + JWKS for key. Returns
// the metadata URL to plug into the verifier.
func jwksServer(t *testing.T, kid string, key *rsa.PrivateKey) string {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		pub := key.Public().(*rsa.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA",
				"kid": kid,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/openid"
}

// signToken mints an RS256 token with the given kid/iss/aud, expiring in 1h.
func signToken(t *testing.T, key *rsa.PrivateKey, kid, iss, aud string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": iss,
		"aud": aud,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

func newTestChannel(t *testing.T, metadataURL string) *Channel {
	t.Helper()
	c := New(testAppID, "secret", "tenant", "")
	c.verify.MetadataURL = metadataURL
	return c
}

func postActivity(t *testing.T, h http.Handler, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/teams", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestHandlerMapsActivity(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestChannel(t, jwksServer(t, "kid-1", key))
	sink := &recordingSink{}
	bearer := signToken(t, key, "kid-1", botFrameworkIssuer, testAppID)

	body := `{
		"type": "message",
		"id": "act-1",
		"text": "<at>memcode</at> fix the build",
		"from": {"id": "aad-user-1", "name": "Tim"},
		"recipient": {"id": "28:bot-id"},
		"conversation": {"id": "19:thread@thread.v2", "conversationType": "channel"},
		"serviceUrl": "https://smba.example.com/emea/",
		"entities": [{"type": "mention", "mentioned": {"id": "28:bot-id"}}]
	}`
	w := postActivity(t, c.Handler(sink), bearer, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if len(sink.got) != 1 {
		t.Fatalf("delivered %d inbounds, want 1", len(sink.got))
	}
	inb := sink.got[0]
	if inb.Channel != "msteams" {
		t.Errorf("Channel = %q", inb.Channel)
	}
	if want := "19:thread@thread.v2|https://smba.example.com/emea/"; inb.Conversation != want {
		t.Errorf("Conversation = %q, want %q", inb.Conversation, want)
	}
	if inb.Principal != "aad-user-1" {
		t.Errorf("Principal = %q", inb.Principal)
	}
	if inb.Text != "fix the build" {
		t.Errorf("Text = %q, want at-tag stripped", inb.Text)
	}
	if inb.MessageID != "act-1" {
		t.Errorf("MessageID = %q", inb.MessageID)
	}
	if inb.IsDirect {
		t.Error("IsDirect = true for a channel conversation")
	}
	if !inb.Mentioned {
		t.Error("Mentioned = false despite a mention entity for the bot")
	}

	// A personal conversation without a mention is a DM.
	dm := `{
		"type": "message",
		"id": "act-2",
		"text": "hello",
		"from": {"id": "aad-user-1"},
		"recipient": {"id": "28:bot-id"},
		"conversation": {"id": "a:dm", "conversationType": "personal"},
		"serviceUrl": "https://smba.example.com/emea/"
	}`
	if w := postActivity(t, c.Handler(sink), bearer, dm); w.Code != http.StatusOK {
		t.Fatalf("dm status = %d", w.Code)
	}
	got := sink.got[len(sink.got)-1]
	if !got.IsDirect || got.Mentioned {
		t.Errorf("dm mapping: IsDirect=%v Mentioned=%v, want true/false", got.IsDirect, got.Mentioned)
	}

	// Non-message activities are acked and dropped.
	before := len(sink.got)
	if w := postActivity(t, c.Handler(sink), bearer, `{"type":"conversationUpdate","conversation":{"id":"x"}}`); w.Code != http.StatusOK {
		t.Fatalf("conversationUpdate status = %d", w.Code)
	}
	if len(sink.got) != before {
		t.Error("non-message activity was delivered")
	}
}

func TestHandlerRejectsBadTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestChannel(t, jwksServer(t, "kid-1", key))
	sink := &recordingSink{}
	h := c.Handler(sink)
	body := `{"type":"message","id":"a","text":"hi","conversation":{"id":"x"},"serviceUrl":"https://s"}`

	// Signed by a key the JWKS doesn't know: signature can't verify.
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if w := postActivity(t, h, signToken(t, otherKey, "kid-1", botFrameworkIssuer, testAppID), body); w.Code != http.StatusUnauthorized {
		t.Errorf("bad signature: status = %d, want 401", w.Code)
	}
	// Right key, wrong audience: token minted for some other bot.
	if w := postActivity(t, h, signToken(t, key, "kid-1", botFrameworkIssuer, "someone-else"), body); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong aud: status = %d, want 401", w.Code)
	}
	// Right key, wrong issuer.
	if w := postActivity(t, h, signToken(t, key, "kid-1", "https://evil.example", testAppID), body); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong iss: status = %d, want 401", w.Code)
	}
	// No token at all.
	if w := postActivity(t, h, "", body); w.Code != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", w.Code)
	}
	if len(sink.got) != 0 {
		t.Fatalf("unauthenticated requests delivered %d inbounds, want 0", len(sink.got))
	}
}

func TestHandlerDeliverFailure(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestChannel(t, jwksServer(t, "kid-1", key))
	sink := &recordingSink{err: errors.New("db down")}
	bearer := signToken(t, key, "kid-1", botFrameworkIssuer, testAppID)
	body := `{"type":"message","id":"a","text":"hi","from":{"id":"u"},"conversation":{"id":"x"},"serviceUrl":"https://s"}`
	if w := postActivity(t, c.Handler(sink), bearer, body); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so the connector retries", w.Code)
	}
}

func TestStripAtTags(t *testing.T) {
	cases := map[string]string{
		"<at>Bot</at> do it":              "do it",
		"do <at>Bot</at> it <at>Two</at>": "do  it",
		"no tags here":                    "no tags here",
		"<at>unclosed do it":              "<at>unclosed do it",
		"a < b and </at> stray":           "a < b and </at> stray",
	}
	for in, want := range cases {
		if got := stripAtTags(in); got != want {
			t.Errorf("stripAtTags(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSendChunksAndCachesToken(t *testing.T) {
	var tokenHits atomic.Int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if g := r.Form.Get("grant_type"); g != "client_credentials" {
			t.Errorf("grant_type = %q", g)
		}
		if s := r.Form.Get("scope"); s != botFrameworkScope {
			t.Errorf("scope = %q", s)
		}
		if id := r.Form.Get("client_id"); id != testAppID {
			t.Errorf("client_id = %q", id)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-abc", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	type sent struct {
		bearer, path, text string
	}
	var posts []sent
	svcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var act struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(body, &act); err != nil {
			t.Errorf("bad activity body: %v", err)
		}
		if act.Type != "message" {
			t.Errorf("activity type = %q", act.Type)
		}
		posts = append(posts, sent{bearer: r.Header.Get("Authorization"), path: r.URL.Path, text: act.Text})
		w.WriteHeader(http.StatusCreated)
	}))
	defer svcSrv.Close()

	c := New(testAppID, "secret", "tenant-1", "")
	c.tokenBase = tokenSrv.URL
	c.trustHost = func(string) bool { return true } // test seam: allow the httptest serviceUrl

	long := strings.Repeat("a", teamsMaxMessage) + " tail"
	conv := "19:conv-1|" + svcSrv.URL
	if err := c.Send(context.Background(), conv, channels.Outbound{Text: long}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("posted %d activities, want 2 chunks", len(posts))
	}
	for _, p := range posts {
		if p.bearer != "Bearer tok-abc" {
			t.Errorf("bearer = %q", p.bearer)
		}
		if want := "/v3/conversations/19:conv-1/activities"; p.path != want {
			t.Errorf("path = %q, want %q", p.path, want)
		}
	}
	// Chunk is loss-free: the concatenation must equal the input.
	if got := posts[0].text + posts[1].text; got != long {
		t.Errorf("chunked text lost content: %d+%d runes", len(posts[0].text), len(posts[1].text))
	}

	// Second send reuses the cached token — the token endpoint is hit once.
	if err := c.Send(context.Background(), conv, channels.Outbound{Text: "again"}); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if n := tokenHits.Load(); n != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 (cached)", n)
	}
	if len(posts) != 3 || posts[2].text != "again" {
		t.Fatalf("second send posts = %+v", posts)
	}
}

func TestSendMalformedConversation(t *testing.T) {
	c := New(testAppID, "secret", "tenant", "")
	if err := c.Send(context.Background(), "no-service-url", channels.Outbound{Text: "x"}); err == nil {
		t.Fatal("expected error for conversation without a serviceUrl")
	}
}

// A reply is refused when the serviceUrl host isn't a first-party Bot Framework
// host — a forged serviceUrl can't redirect the agent's output (and its bearer).
func TestSendRefusesUntrustedServiceURL(t *testing.T) {
	c := New(testAppID, "secret", "tenant-1", "")
	// No trustHost seam here → the real allowlist applies.
	err := c.Send(context.Background(), "19:conv|https://attacker.example.com", channels.Outbound{Text: "hi"})
	if err == nil {
		t.Fatal("reply to untrusted serviceUrl must be refused")
	}
	// A genuine Bot Framework host passes the allowlist (it will fail later at
	// the network layer, which is fine — we only assert the host gate here).
	err = c.Send(context.Background(), "19:conv|https://smba.trafficmanager.net/amer/", channels.Outbound{Text: "hi"})
	if err != nil && strings.Contains(err.Error(), "untrusted serviceUrl") {
		t.Fatalf("trusted host wrongly rejected: %v", err)
	}
}

func TestHostTrustedGate(t *testing.T) {
	c := New("app", "s", "t", "")
	trusted := []string{"https://smba.trafficmanager.net/x", "https://foo.botframework.com/y", "https://team.sharepoint.com/z"}
	for _, u := range trusted {
		pu, _ := url.Parse(u)
		if !c.hostTrusted(pu) {
			t.Errorf("%s should be trusted", u)
		}
	}
	untrusted := []string{"http://smba.trafficmanager.net/x", "https://169.254.169.254/", "https://evil.com/", "https://smba.trafficmanager.net.evil.com/"}
	for _, u := range untrusted {
		pu, _ := url.Parse(u)
		if c.hostTrusted(pu) {
			t.Errorf("%s should NOT be trusted", u)
		}
	}
}
