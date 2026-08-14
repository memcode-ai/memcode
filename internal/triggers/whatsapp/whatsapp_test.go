package whatsapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/channels"
)

func TestVerifyChallenge(t *testing.T) {
	q := func(mode, token, challenge string) url.Values {
		v := url.Values{}
		v.Set("hub.mode", mode)
		v.Set("hub.verify_token", token)
		v.Set("hub.challenge", challenge)
		return v
	}
	if got, ok := verifyChallenge(q("subscribe", "vt", "42"), "vt"); !ok || got != "42" {
		t.Errorf("valid handshake: got (%q,%v)", got, ok)
	}
	if _, ok := verifyChallenge(q("subscribe", "wrong", "42"), "vt"); ok {
		t.Error("wrong token accepted")
	}
	if _, ok := verifyChallenge(q("unsubscribe", "vt", "42"), "vt"); ok {
		t.Error("wrong mode accepted")
	}
	if _, ok := verifyChallenge(q("subscribe", "", "42"), ""); ok {
		t.Error("empty verify token accepted")
	}
}

func TestToInbounds(t *testing.T) {
	payload := `{"entry":[{"changes":[{"value":{"messages":[
		{"id":"wamid.1","from":"15551230000","type":"text","text":{"body":"do it"}},
		{"id":"wamid.2","from":"15551230000","type":"image","text":{"body":""}},
		{"id":"wamid.3","from":"15559990000","type":"text","text":{"body":"hi"}}
	]}}]}]}`
	got := toInbounds([]byte(payload))
	if len(got) != 2 {
		t.Fatalf("want 2 text messages, got %d: %+v", len(got), got)
	}
	want := channels.Inbound{Channel: "whatsapp", Conversation: "15551230000", Principal: "15551230000", Text: "do it", MessageID: "wamid.1", IsDirect: true}
	if got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
	if n := len(toInbounds([]byte("not json"))); n != 0 {
		t.Errorf("bad json yielded %d inbounds", n)
	}
}

func TestSend(t *testing.T) {
	var gotAuth, gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New("PN123", "TOKEN", "vt", "sekret")
	c.base = srv.URL
	if err := c.Send(context.Background(), "15551230000", channels.Outbound{Text: "yo"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth != "Bearer TOKEN" {
		t.Errorf("auth = %q", gotAuth)
	}
	if !strings.HasSuffix(gotPath, "/PN123/messages") {
		t.Errorf("path = %q", gotPath)
	}
	if body["to"] != "15551230000" || body["messaging_product"] != "whatsapp" {
		t.Errorf("body = %+v", body)
	}
}

func TestHandlerGET(t *testing.T) {
	c := New("PN", "tok", "vt", "sekret")
	h := c.Handler(make(chan channels.Inbound, 1))
	req := httptest.NewRequest(http.MethodGet, "/webhook/whatsapp?hub.mode=subscribe&hub.verify_token=vt&hub.challenge=99", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "99" {
		t.Errorf("GET verify: code %d body %q", rr.Code, rr.Body.String())
	}
}

func TestHandlerPOSTSignature(t *testing.T) {
	const secret = "sekret"
	c := New("PN", "tok", "vt", secret)
	inbound := make(chan channels.Inbound, 1)
	h := c.Handler(inbound)

	body := `{"entry":[{"changes":[{"value":{"messages":[{"id":"wamid.9","from":"15550001111","type":"text","text":{"body":"hi"}}]}}]}]}`
	sign := func(s string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	post := func(sig string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", strings.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", sig)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// Bad signature → 401, nothing forwarded.
	if rr := post("sha256=00"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: got %d", rr.Code)
	}
	select {
	case <-inbound:
		t.Fatal("unsigned message was forwarded")
	default:
	}

	// Valid signature → 200 and the message is forwarded.
	if rr := post(sign(body)); rr.Code != http.StatusOK {
		t.Fatalf("good sig: got %d", rr.Code)
	}
	select {
	case inb := <-inbound:
		if inb.MessageID != "wamid.9" || inb.Conversation != "15550001111" {
			t.Errorf("forwarded %+v", inb)
		}
	default:
		t.Fatal("signed message not forwarded")
	}
}
