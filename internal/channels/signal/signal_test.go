package signal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/channels"
)

const dmEnvelope = `{"envelope":{"sourceNumber":"+15551230000","sourceUuid":"uuid-1","timestamp":1700000000001,"dataMessage":{"message":"fix the build"}},"account":"+15550009999"}`

func TestParseEnvelopeDM(t *testing.T) {
	inb, refs, ok := parseEnvelope([]byte(dmEnvelope), "+15550009999")
	if !ok {
		t.Fatal("parse failed")
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
	if len(refs) != 0 {
		t.Errorf("refs = %v", refs)
	}
}

func TestParseEnvelopeGroupAndMentions(t *testing.T) {
	raw := `{"envelope":{"sourceNumber":"+15551230000","timestamp":2,
		"dataMessage":{"message":"@bot do it","groupInfo":{"groupId":"g99"},
		"mentions":[{"number":"+15550009999"}]}}}`
	inb, _, ok := parseEnvelope([]byte(raw), "+15550009999")
	if !ok {
		t.Fatal("parse failed")
	}
	if inb.IsDirect || inb.Conversation != "group:g99" || !inb.Mentioned {
		t.Errorf("group inbound = %+v", inb)
	}

	// Quote-reply to us also counts as addressed.
	raw = `{"envelope":{"sourceNumber":"+15551230000","timestamp":3,
		"dataMessage":{"message":"and this?","groupInfo":{"groupId":"g99"},
		"quote":{"author":"+15550009999"}}}}`
	inb, _, _ = parseEnvelope([]byte(raw), "+15550009999")
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
		if _, _, ok := parseEnvelope([]byte(raw), "+15550009999"); ok {
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
	if err := c.stream(context.Background(), sink); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "fix the build" {
		t.Fatalf("delivered %+v", got)
	}
}

type sinkFn func(channels.Inbound) error

func (s sinkFn) Deliver(_ context.Context, inb channels.Inbound) error { return s(inb) }
