package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// fakeVendor is a minimal chat-completions host recording what reached it.
type fakeVendor struct {
	mu      sync.Mutex
	models  []string
	headers []http.Header
	status  int // non-zero: answer every call with this HTTP status
}

func (f *fakeVendor) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		m, _ := body["model"].(string)
		f.models = append(f.models, m)
		f.headers = append(f.headers, r.Header.Clone())
		status := f.status
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"quota"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
}

func laneFor(t *testing.T, vendor, name, model, baseURL string, headers map[string]string) lane {
	t.Helper()
	ep := Endpoint{Name: name, BaseURL: baseURL, Key: "k", Model: model, Headers: headers}
	return lane{vendor: vendor, kind: laneSub, ep: ep, c: dialEndpoint(ep)}
}

func lazyWith(base *conn, lanes []lane) *Lazy {
	l := &Lazy{}
	if base != nil {
		l.c.Store(base)
	}
	l.lanes.Store(&lanes)
	return l
}

func req(pin string) wire.Request {
	return wire.Request{Pin: pin, Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}}}
}

// C3+C6: family dispatch, label→raw-id translation at the lane boundary,
// header isolation, per-turn provenance restamp; gateway keeps labels.
func TestLaneDispatchMatrix(t *testing.T) {
	anth := &fakeVendor{}
	anthSrv := anth.serve()
	defer anthSrv.Close()
	base := &fakeVendor{}
	baseSrv := base.serve()
	defer baseSrv.Close()

	lanes := []lane{laneFor(t, "anthropic", "claude-sub", "claude-sonnet-5", anthSrv.URL, map[string]string{"X-Lane-Probe": "A"})}
	baseEp := Endpoint{Name: "gw", BaseURL: baseSrv.URL, Key: "g"}
	l := lazyWith(dialEndpoint(baseEp), lanes)

	// Anthropic label rides the lane, translated to the raw id.
	resp, err := l.Complete(context.Background(), req("opus"))
	if err != nil {
		t.Fatalf("lane turn: %v", err)
	}
	if resp.Backend != "claude-sub" {
		t.Fatalf("Backend = %q, want claude-sub", resp.Backend)
	}
	// Fireworks label rides base, label untouched.
	if _, err := l.Complete(context.Background(), req("kimi-k3")); err != nil {
		t.Fatalf("base turn: %v", err)
	}
	// Unknown model rides base verbatim.
	if _, err := l.Complete(context.Background(), req("custom-x")); err != nil {
		t.Fatalf("unknown-model turn: %v", err)
	}

	anth.mu.Lock()
	if len(anth.models) != 1 || anth.models[0] != "claude-opus-5" {
		t.Errorf("anthropic lane saw models %v, want [claude-opus-5]", anth.models)
	}
	for _, h := range anth.headers {
		if h.Get("X-Lane-Probe") != "A" {
			t.Error("lane identity header missing on lane request")
		}
	}
	anth.mu.Unlock()
	base.mu.Lock()
	if len(base.models) != 2 || base.models[0] != "kimi-k3" || base.models[1] != "custom-x" {
		t.Errorf("base saw models %v, want [kimi-k3 custom-x]", base.models)
	}
	for _, h := range base.headers {
		if h.Get("X-Lane-Probe") != "" {
			t.Error("lane identity header leaked to base")
		}
	}
	base.mu.Unlock()
}

// C5: off-family with no gateway is ErrNoLane naming vendor + attached lanes.
func TestLaneDispatchNoBase(t *testing.T) {
	anth := &fakeVendor{}
	srv := anth.serve()
	defer srv.Close()
	l := lazyWith(nil, []lane{laneFor(t, "anthropic", "claude-sub", "claude-sonnet-5", srv.URL, nil)})

	_, err := l.Complete(context.Background(), req("kimi-k3"))
	var noLane *ErrNoLane
	if !errors.As(err, &noLane) {
		t.Fatalf("err = %v, want ErrNoLane", err)
	}
	if noLane.Vendor != "fireworks" {
		t.Errorf("Vendor = %q, want fireworks", noLane.Vendor)
	}
	// Empty pin serves on the first lane (its default model applies).
	if _, err := l.Complete(context.Background(), req("")); err != nil {
		t.Fatalf("empty-pin turn: %v", err)
	}
}

// C5: lane 429 classifies as ErrLaneExhausted; 404 stays an ordinary error.
func TestLaneExhaustedClassification(t *testing.T) {
	anth := &fakeVendor{status: 429}
	srv := anth.serve()
	defer srv.Close()
	base := &fakeVendor{}
	baseSrv := base.serve()
	defer baseSrv.Close()

	l := lazyWith(dialEndpoint(Endpoint{Name: "gw", BaseURL: baseSrv.URL, Key: "g"}),
		[]lane{laneFor(t, "anthropic", "claude-sub", "claude-sonnet-5", srv.URL, nil)})

	_, err := l.Complete(context.Background(), req("sonnet"))
	var exh *ErrLaneExhausted
	if !errors.As(err, &exh) {
		t.Fatalf("err = %v, want ErrLaneExhausted", err)
	}
	if !exh.CanFallback {
		t.Error("CanFallback = false with a gateway base present")
	}
	if exh.Lane.Name != "claude-sub" {
		t.Errorf("Lane.Name = %q", exh.Lane.Name)
	}

	anth.status = 404
	_, err = l.Complete(context.Background(), req("sonnet"))
	var exhAgain *ErrLaneExhausted
	if errors.As(err, &exhAgain) {
		t.Fatalf("model-404 classified as exhaustion: %v", err)
	}
	if err == nil {
		t.Fatal("model-404 must still be an error")
	}
}

// Transitions: /login swaps base only; lanes persist. /logout keeps lanes.
func TestCredentialSwapsPreserveLanes(t *testing.T) {
	anth := &fakeVendor{}
	srv := anth.serve()
	defer srv.Close()
	l := lazyWith(nil, []lane{laneFor(t, "anthropic", "claude-sub", "claude-sonnet-5", srv.URL, nil)})

	l.SetCredentials("http://127.0.0.1:0", "memcode_x")
	if got := len(l.Lanes()); got != 1 {
		t.Fatalf("lanes after SetCredentials = %d, want 1", got)
	}
	l.ClearCredentials()
	if got := len(l.Lanes()); got != 1 {
		t.Fatalf("lanes after ClearCredentials = %d, want 1", got)
	}
	if !l.Connected() {
		t.Fatal("lanes-only session must report Connected")
	}
}

// codex+copilot are both openai-family: FIRST listed wins, both orders.
func TestOpenAIFamilyConflictFirstWins(t *testing.T) {
	restore := resolveSourceFn
	defer func() { resolveSourceFn = restore }()
	resolveSourceFn = func(source string) (Endpoint, bool) {
		switch source {
		case "codex":
			return Endpoint{Name: "codex", BaseURL: "http://codex.invalid", Key: "k", Model: "gpt-5.6-terra"}, true
		case "copilot":
			return Endpoint{Name: "copilot", BaseURL: "http://copilot.invalid", Key: "k", Model: "gpt-4o"}, true
		}
		return Endpoint{}, false
	}
	restoreDial := dialLane
	defer func() { dialLane = restoreDial }()
	dialLane = func(ep Endpoint) *conn { return &conn{ep: &ep} }

	t.Setenv(EnvCredentials, "codex,copilot")
	t.Setenv(EnvCredentialSource, "")
	if lanes := buildLanes(); len(lanes) != 1 || lanes[0].ep.Name != "codex" {
		t.Fatalf("codex,copilot → %+v, want single codex lane", lanesNames(lanes))
	}
	t.Setenv(EnvCredentials, "copilot,codex")
	if lanes := buildLanes(); len(lanes) != 1 || lanes[0].ep.Name != "copilot" {
		t.Fatalf("copilot,codex → %+v, want single copilot lane", lanesNames(lanes))
	}
}

func lanesNames(ls []lane) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.ep.Name)
	}
	return out
}
