package signal

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

const dmEnvelope = `{"envelope":{"sourceNumber":"+15551230000","sourceUuid":"uuid-1","timestamp":1700000000001,"dataMessage":{"message":"fix the build"}},"account":"+15550009999"}`

func deliverOne(t *testing.T, c *Channel, raw string) (channels.Inbound, bool) {
	t.Helper()
	var got channels.Inbound
	var ok bool
	c.handleEvent(context.Background(), sinkFn(func(inb channels.Inbound) error {
		got, ok = inb, true
		return nil
	}), []byte(raw))
	return got, ok
}

func TestParseEnvelopeDM(t *testing.T) {
	c := New("", "+15550009999", "", "")
	inb, ok := deliverOne(t, c, dmEnvelope)
	if !ok {
		t.Fatal("not delivered")
	}
	// Principal is the STABLE uuid; the phone number is only a fallback.
	if inb.Channel != "signal" || inb.Principal != "uuid-1" || inb.Conversation != "uuid-1" {
		t.Errorf("inbound = %+v", inb)
	}
	if !inb.IsDirect || inb.Mentioned {
		t.Errorf("gating = %+v", inb)
	}
	if inb.MessageID != "uuid-1:1700000000001" {
		t.Errorf("MessageID = %q (dedup key is sender:timestamp)", inb.MessageID)
	}
	if len(inb.Attachments) != 0 {
		t.Errorf("attachments = %v", inb.Attachments)
	}
}

// A number-only redelivery of a sender first seen WITH a uuid resolves to the
// same uuid, so the dedup key and principal never flip (review finding M2).
func TestSignalNumberResolvesToLearnedUUID(t *testing.T) {
	c := New("", "+15550009999", "", "")
	// First delivery carries both uuid and number.
	if _, ok := deliverOne(t, c, dmEnvelope); !ok {
		t.Fatal("first delivery dropped")
	}
	// A later delivery of the same account carrying ONLY the number.
	numOnly := `{"envelope":{"sourceNumber":"+15551230000","timestamp":1700000000002,"dataMessage":{"message":"again"}}}`
	inb, ok := deliverOne(t, c, numOnly)
	if !ok {
		t.Fatal("number-only delivery dropped")
	}
	if inb.Principal != "uuid-1" || inb.Conversation != "uuid-1" {
		t.Errorf("identity flipped to number: %+v", inb)
	}
	if inb.MessageID != "uuid-1:1700000000002" {
		t.Errorf("MessageID = %q, want uuid-keyed", inb.MessageID)
	}
}

func TestParseEnvelopeGroupAndMentions(t *testing.T) {
	raw := `{"envelope":{"sourceNumber":"+15551230000","timestamp":2,
		"dataMessage":{"message":"@bot do it","groupInfo":{"groupId":"g99"},
		"mentions":[{"number":"+15550009999"}]}}}`
	c := New("", "+15550009999", "", "")
	inb, ok := deliverOne(t, c, raw)
	if !ok {
		t.Fatal("not delivered")
	}
	if inb.IsDirect || inb.Conversation != "group:g99" || !inb.Mentioned {
		t.Errorf("group inbound = %+v", inb)
	}

	// Quote-reply to us also counts as addressed.
	raw = `{"envelope":{"sourceNumber":"+15551230000","timestamp":3,
		"dataMessage":{"message":"and this?","groupInfo":{"groupId":"g99"},
		"quote":{"author":"+15550009999"}}}}`
	inb, _ = deliverOne(t, c, raw)
	if !inb.Mentioned {
		t.Error("quote-reply must count as mentioned")
	}
}

func TestParseEnvelopeSkips(t *testing.T) {
	for name, raw := range map[string]string{
		"receipt":   `{"envelope":{"sourceNumber":"+1555","timestamp":1,"receiptMessage":{}}}`,
		"own":       `{"envelope":{"sourceNumber":"+15550009999","timestamp":1,"dataMessage":{"message":"hi"}}}`,
		"empty":     `{"envelope":{"sourceNumber":"+1555","timestamp":1,"dataMessage":{"message":""}}}`,
		"malformed": `not json`,
	} {
		if _, _, _, ok := parseEnvelope([]byte(raw), "+15550009999"); ok {
			t.Errorf("%s must be skipped", name)
		}
	}
}

func TestSendDMAndGroup(t *testing.T) {
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		calls = append(calls, req)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "+15550009999", "", "")
	if err := c.Send(context.Background(), "+15551230000", channels.Outbound{Text: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background(), "group:g99", channels.Outbound{Text: strings.Repeat("x", signalMaxMessage+10)}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 { // 1 DM + 2 chunked group parts
		t.Fatalf("calls = %d", len(calls))
	}
	p0 := calls[0]["params"].(map[string]any)
	if rec, ok := p0["recipient"].([]any); !ok || rec[0] != "+15551230000" {
		t.Errorf("DM params = %+v", p0)
	}
	p1 := calls[1]["params"].(map[string]any)
	if p1["groupId"] != "g99" {
		t.Errorf("group params = %+v", p1)
	}
}

// The SSE stream is parsed into envelopes and delivered best-effort.
func TestStreamDelivers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+dmEnvelope+"\n\n")
	}))
	defer srv.Close()

	c := New(srv.URL, "+15550009999", "", "")
	var got []channels.Inbound
	sink := sinkFn(func(inb channels.Inbound) error { got = append(got, inb); return nil })
	backoff := channels.NewBackoff(time.Second, time.Minute)
	if err := c.stream(context.Background(), sink, backoff); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "fix the build" {
		t.Fatalf("delivered %+v", got)
	}
}

type sinkFn func(channels.Inbound) error

func (s sinkFn) Deliver(_ context.Context, inb channels.Inbound) error { return s(inb) }
