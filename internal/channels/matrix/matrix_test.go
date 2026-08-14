package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
)

// recordSink forwards each delivered Inbound to a channel and can be told to
// fail the first N deliveries, for exercising the ack semantics.
type recordSink struct {
	mu       sync.Mutex
	failures int
	got      chan channels.Inbound
}

func newRecordSink(failures int) *recordSink {
	return &recordSink{failures: failures, got: make(chan channels.Inbound, 16)}
}

func (s *recordSink) Deliver(ctx context.Context, inb channels.Inbound) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures > 0 {
		s.failures--
		return errors.New("sink down")
	}
	s.got <- inb
	return nil
}

// fakeCursorStore is an in-memory CursorStore that records every SetCursor.
type fakeCursorStore struct {
	mu     sync.Mutex
	cursor string
	sets   []string
}

func (f *fakeCursorStore) Cursor(ctx context.Context, channel string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursor, nil
}

func (f *fakeCursorStore) SetCursor(ctx context.Context, channel, cursor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursor = cursor
	f.sets = append(f.sets, cursor)
	return nil
}

func (f *fakeCursorStore) lastSet() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sets) == 0 {
		return "", 0
	}
	return f.sets[len(f.sets)-1], len(f.sets)
}

const botID = "@bot:example.org"

// fakeHomeserver serves whoami, m.direct, and a scripted sequence of /sync
// bodies (one per call; the last repeats). It records each sync's query.
type fakeHomeserver struct {
	t     *testing.T
	syncs []string

	mu      sync.Mutex
	queries []string // since param of each /sync call ("" for none)
	filters []string // filter param of each /sync call
}

func (f *fakeHomeserver) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/account/whoami"):
			io.WriteString(w, `{"user_id":"`+botID+`"}`)
		case strings.HasSuffix(r.URL.Path, "/account_data/m.direct"):
			if !strings.Contains(r.URL.Path, botID) {
				f.t.Errorf("m.direct fetched for wrong user: %s", r.URL.Path)
			}
			io.WriteString(w, `{"@friend:example.org":["!dm:example.org"]}`)
		case strings.HasSuffix(r.URL.Path, "/sync"):
			f.mu.Lock()
			n := len(f.queries)
			f.queries = append(f.queries, r.URL.Query().Get("since"))
			f.filters = append(f.filters, r.URL.Query().Get("filter"))
			f.mu.Unlock()
			if got := r.Header.Get("Authorization"); got != "Bearer TOKEN" {
				f.t.Errorf("sync auth = %q, want bearer token", got)
			}
			if n >= len(f.syncs) {
				n = len(f.syncs) - 1
			}
			io.WriteString(w, f.syncs[n])
		default:
			f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *fakeHomeserver) sinceOf(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.queries) {
		return "<no such call>"
	}
	return f.queries[i]
}

func (f *fakeHomeserver) filterOf(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.filters) {
		return "<no such call>"
	}
	return f.filters[i]
}

func TestStartDeliversInbound(t *testing.T) {
	// Sync 1: no since — history that must NOT be delivered, just skipped over.
	// Sync 2: a DM text and a group text carrying an intentional mention.
	fs := &fakeHomeserver{t: t, syncs: []string{
		`{"next_batch":"s1","rooms":{"join":{"!dm:example.org":{"timeline":{"events":[
			{"type":"m.room.message","event_id":"$old","sender":"@friend:example.org","content":{"msgtype":"m.text","body":"ancient history"}}
		]}}}}}`,
		`{"next_batch":"s2","rooms":{"join":{
			"!dm:example.org":{"timeline":{"events":[
				{"type":"m.room.message","event_id":"$dm1","sender":"@friend:example.org","content":{"msgtype":"m.text","body":"hello there"}}
			]}},
			"!group:example.org":{"timeline":{"events":[
				{"type":"m.room.message","event_id":"$grp1","sender":"@friend:example.org","content":{"msgtype":"m.text","body":"do it","m.mentions":{"user_ids":["@bot:example.org"]}}}
			]}}
		}}}`,
		`{"next_batch":"s2"}`,
	}}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()

	sink := newRecordSink(0)
	c := New(srv.URL, "TOKEN", nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Start(ctx, sink)

	// Room map iteration order is unspecified, so collect by event id.
	byID := map[string]channels.Inbound{}
	for range 2 {
		select {
		case inb := <-sink.got:
			byID[inb.MessageID] = inb
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out; delivered so far: %v", byID)
		}
	}
	cancel()

	if _, ok := byID["$old"]; ok {
		t.Error("initial (no-since) sync must deliver nothing, but $old came through")
	}
	dm, ok := byID["$dm1"]
	if !ok {
		t.Fatal("DM event not delivered")
	}
	want := channels.Inbound{
		Channel: "matrix", Conversation: "!dm:example.org",
		Principal: "@friend:example.org", Text: "hello there",
		MessageID: "$dm1", IsDirect: true,
	}
	if !reflect.DeepEqual(dm, want) {
		t.Errorf("dm inbound = %+v, want %+v", dm, want)
	}
	grp, ok := byID["$grp1"]
	if !ok {
		t.Fatal("group event not delivered")
	}
	if grp.IsDirect || !grp.Mentioned {
		t.Errorf("group: IsDirect=%v Mentioned=%v, want false/true (m.mentions)", grp.IsDirect, grp.Mentioned)
	}
	if fs.filterOf(0) == "" {
		t.Error("first-ever sync must carry a history-limiting filter")
	}
	if fs.sinceOf(1) != "s1" {
		t.Errorf("second sync since = %q, want s1", fs.sinceOf(1))
	}
}

func TestDeliverFailureHoldsCursor(t *testing.T) {
	// The store already holds a cursor, so the first sync is a real batch (no
	// initial-sync special case). The sink fails that delivery once; the
	// since-token must stay put and the batch must be re-synced before any
	// SetCursor advances it.
	batch := `{"next_batch":"s2","rooms":{"join":{"!r:example.org":{"timeline":{"events":[
		{"type":"m.room.message","event_id":"$e1","sender":"@friend:example.org","content":{"msgtype":"m.text","body":"retry me"}}
	]}}}}}`
	fs := &fakeHomeserver{t: t, syncs: []string{batch}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Once the cursor advances past s1, serve empty so the loop idles.
		if strings.HasSuffix(r.URL.Path, "/sync") && r.URL.Query().Get("since") == "s2" {
			io.WriteString(w, `{"next_batch":"s2"}`)
			return
		}
		fs.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	store := &fakeCursorStore{cursor: "s1"}
	sink := newRecordSink(1) // first Deliver errors
	c := New(srv.URL, "TOKEN", store, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Start(ctx, sink)

	select {
	case inb := <-sink.got:
		if inb.MessageID != "$e1" {
			t.Fatalf("replayed event id = %q, want $e1", inb.MessageID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event never redelivered after sink recovery")
	}
	// The retry must have come from the un-advanced token.
	if fs.sinceOf(0) != "s1" || fs.sinceOf(1) != "s1" {
		t.Errorf("syncs used since %q then %q, want s1 both times (no advance on failure)",
			fs.sinceOf(0), fs.sinceOf(1))
	}
	// SetCursor fires only after the successful delivery, and only with s2.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if last, n := store.lastSet(); n > 0 {
			if last != "s2" || n != 1 {
				t.Errorf("SetCursor calls = %d last %q, want exactly one with s2", n, last)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cursor never persisted after successful delivery")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
}

func TestSkipsOwnAndNotice(t *testing.T) {
	fs := &fakeHomeserver{t: t, syncs: []string{
		`{"next_batch":"s2","rooms":{"join":{"!r:example.org":{"timeline":{"events":[
			{"type":"m.room.message","event_id":"$own","sender":"@bot:example.org","content":{"msgtype":"m.text","body":"my own echo"}},
			{"type":"m.room.message","event_id":"$ntc","sender":"@otherbot:example.org","content":{"msgtype":"m.notice","body":"bot output"}},
			{"type":"m.room.message","event_id":"$ok","sender":"@friend:example.org","content":{"msgtype":"m.text","body":"real message"}}
		]}}}}}`,
		`{"next_batch":"s3"}`,
	}}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()

	sink := newRecordSink(0)
	c := New(srv.URL, "TOKEN", &fakeCursorStore{cursor: "s1"}, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Start(ctx, sink)

	select {
	case inb := <-sink.got:
		if inb.MessageID != "$ok" {
			t.Fatalf("delivered %q, want only $ok", inb.MessageID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the one real message never arrived")
	}
	select {
	case inb := <-sink.got:
		t.Fatalf("own/notice event leaked through: %+v", inb)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
}

func TestSendChunksAsNotice(t *testing.T) {
	type sent struct {
		path, auth string
		body       map[string]any
	}
	var mu sync.Mutex
	var posts []sent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("unexpected method %s", r.Method)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		posts = append(posts, sent{r.URL.Path, r.Header.Get("Authorization"), body})
		mu.Unlock()
		io.WriteString(w, `{"event_id":"$sent"}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "TOKEN", nil, "")
	long := strings.Repeat("a", matrixMaxMessage+500) // forces two chunks
	if err := c.Send(context.Background(), "!room:example.org", channels.Outbound{Text: long}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("got %d PUTs, want 2 (chunked)", len(posts))
	}
	total := ""
	for i, p := range posts {
		if !strings.HasPrefix(p.path, "/_matrix/client/v3/rooms/!room:example.org/send/m.room.message/") {
			t.Errorf("put %d path = %q", i, p.path)
		}
		if p.auth != "Bearer TOKEN" {
			t.Errorf("put %d auth = %q, want bearer token", i, p.auth)
		}
		if p.body["msgtype"] != "m.notice" {
			t.Errorf("put %d msgtype = %v, want m.notice (bot convention)", i, p.body["msgtype"])
		}
		s, _ := p.body["body"].(string)
		total += s
	}
	if total != long {
		t.Error("chunked bodies do not reassemble to the original text")
	}
}
