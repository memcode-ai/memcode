package sms

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/channels"
)

type recSink struct {
	got  []channels.Inbound
	fail bool
}

func (s *recSink) Deliver(_ context.Context, inb channels.Inbound) error {
	if s.fail {
		return errors.New("db down")
	}
	s.got = append(s.got, inb)
	return nil
}

func sign(authToken, webhookURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(webhookURL)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(form.Get(k))
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func post(h http.Handler, sig string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook/sms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if sig != "" {
		req.Header.Set("X-Twilio-Signature", sig)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHandlerSignatureAndDelivery(t *testing.T) {
	const hook = "https://gw.example.com/webhook/sms"
	c := New("AC123", "tok", "+15550009999", hook, "")
	sink := &recSink{}
	h := c.Handler(sink)

	form := url.Values{}
	form.Set("From", "+15551230000")
	form.Set("MessageSid", "SM1")
	form.Set("Body", "restart the deploy")
	form.Set("NumMedia", "0")

	// Bad signature → 401, nothing delivered.
	if rr := post(h, "AAAA", form); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: %d", rr.Code)
	}
	// Missing signature → 401.
	if rr := post(h, "", form); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no sig: %d", rr.Code)
	}
	if len(sink.got) != 0 {
		t.Fatal("unauthenticated SMS delivered")
	}

	// Valid signature → delivered, empty TwiML back.
	rr := post(h, sign("tok", hook, form), form)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "<Response></Response>") {
		t.Fatalf("good sig: %d %q", rr.Code, rr.Body.String())
	}
	inb := sink.got[0]
	if inb.Channel != "sms" || inb.Principal != "+15551230000" || inb.MessageID != "SM1" || !inb.IsDirect {
		t.Errorf("inbound = %+v", inb)
	}

	// Deliver failure → 503 so Twilio retries.
	sink.fail = true
	if rr := post(h, sign("tok", hook, form), form); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("deliver fail: %d", rr.Code)
	}
}

// No configured webhook URL → fail closed: nothing verifies.
func TestHandlerFailsClosedWithoutWebhookURL(t *testing.T) {
	c := New("AC123", "tok", "+1555", "", "")
	sink := &recSink{}
	form := url.Values{"From": {"+1"}, "MessageSid": {"SM2"}, "Body": {"x"}}
	if rr := post(c.Handler(sink), sign("tok", "https://anything", form), form); rr.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rr.Code)
	}
}

func TestSendChunksWithBasicAuth(t *testing.T) {
	var bodies []string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		bodies = append(bodies, v.Get("Body"))
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New("AC123", "tok", "+15550009999", "https://hook", "")
	c.base = srv.URL
	long := strings.Repeat("y", smsMaxMessage+5)
	if err := c.Send(context.Background(), "+15551230000", channels.Outbound{Text: long}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("parts = %d", len(bodies))
	}
	if got := strings.Join(bodies, ""); got != long {
		t.Error("chunking lost content")
	}
	if !strings.HasPrefix(auth, "Basic ") {
		t.Errorf("auth = %q", auth)
	}
}
