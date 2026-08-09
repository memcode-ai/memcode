package vxui

// /apikeys picker tests: roster from the gateway (nothing hardcoded), masked
// entry (the key never reaches scrollback or the composer), paste routing, and
// the save round-trip.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"
)

// fakeByokGateway serves the /v1/byok surface; records the PUT body key.
func fakeByokGateway(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/byok/keys":
			json.NewEncoder(w).Encode(map[string]any{
				"providers": []string{"openai", "anthropic", "gemini"},
				"keys":      []map[string]string{{"provider": "anthropic", "tail": "abcd", "status": "active"}},
			})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/byok/keys/"):
			var body struct {
				Key string `json:"key"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			gotKey = body.Key
			json.NewEncoder(w).Encode(map[string]string{
				"provider": strings.TrimPrefix(r.URL.Path, "/v1/byok/keys/"),
				"tail":     body.Key[max(0, len(body.Key)-4):],
				"status":   "active",
			})
		case r.Method == http.MethodDelete:
			json.NewEncoder(w).Encode(map[string]any{"deleted": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gotKey
}

func openApikeys(t *testing.T) (*appState, *recBackend, *ui.Runner) {
	t.Helper()
	srv, _ := fakeByokGateway(t)
	t.Setenv("MEMCODE_API_URL", srv.URL)
	t.Setenv("MEMCODE_API_TOKEN", "memcode_test")
	st, _, be, runner := newRecRunnerCapture(t)
	now := time.Now()
	typeSlash(runner, "/apikeys", now)
	for i := 0; i < 100 && !st.apikeysPicking; i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(5 * time.Millisecond)
	}
	if !st.apikeysPicking {
		t.Fatalf("/apikeys did not open the picker.\nrecorded=%q", be.recorded())
	}
	return st, be, runner
}

// TestApikeysRosterFromGateway: rows come from the server's providers list,
// merged with the user's masked key rows.
func TestApikeysRosterFromGateway(t *testing.T) {
	st, _, _ := openApikeys(t)
	if len(st.apikeysRows) != 3 {
		t.Fatalf("rows = %+v, want the gateway's 3 providers", st.apikeysRows)
	}
	if st.apikeysRows[0].provider != "openai" || st.apikeysRows[0].tail != "" {
		t.Fatalf("row0 = %+v, want openai/not-set", st.apikeysRows[0])
	}
	if st.apikeysRows[1].provider != "anthropic" || st.apikeysRows[1].tail != "abcd" {
		t.Fatalf("row1 = %+v, want anthropic/…abcd", st.apikeysRows[1])
	}
}

// TestApikeysMaskedEntrySavesAndRedacts: the typed key reaches the gateway,
// but NEVER scrollback; the success line shows only the tail.
func TestApikeysMaskedEntrySavesAndRedacts(t *testing.T) {
	srv, gotKey := fakeByokGateway(t)
	t.Setenv("MEMCODE_API_URL", srv.URL)
	t.Setenv("MEMCODE_API_TOKEN", "memcode_test")
	st, _, be, runner := newRecRunnerCapture(t)
	now := time.Now()
	typeSlash(runner, "/apikeys", now)
	for i := 0; i < 100 && !st.apikeysPicking; i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(5 * time.Millisecond)
	}

	// Enter on row 0 (openai) → masked entry; type the key; Enter saves.
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)
	if !st.apikeysEntering {
		t.Fatal("Enter must open the masked entry stage")
	}
	const secret = "sk-super-secret-9xyz"
	for _, r := range secret {
		runner.HandleEvent(vaxis.Key{Text: string(r), Keycode: r}, now)
	}
	if string(st.apikeysInput) != secret {
		t.Fatalf("masked buffer = %q", string(st.apikeysInput))
	}
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)
	for i := 0; i < 200 && !strings.Contains(be.recorded(), "key saved"); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(5 * time.Millisecond)
	}
	if *gotKey != secret {
		t.Fatalf("gateway got %q, want the typed key", *gotKey)
	}
	rec := be.recorded()
	if strings.Contains(rec, secret) {
		t.Fatalf("the raw key leaked into scrollback: %q", rec)
	}
	if !strings.Contains(rec, "✓ OpenAI key saved (…9xyz)") {
		t.Fatalf("success line missing/wrong: %q", rec)
	}
	if len(st.apikeysInput) != 0 {
		t.Fatal("masked buffer must be zeroed after submit")
	}
}

// TestApikeysPasteGoesToMaskedBuffer: a bracketed paste during entry lands in
// the masked buffer, not the composer.
func TestApikeysPasteGoesToMaskedBuffer(t *testing.T) {
	st, _, runner := openApikeys(t)
	now := time.Now()
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now) // entry stage

	runner.HandleEvent(vaxis.PasteStartEvent{}, now)
	for _, r := range "sk-pasted-key" {
		runner.HandleEvent(vaxis.Key{Text: string(r), Keycode: r, EventType: vaxis.EventPaste}, now)
	}
	runner.HandleEvent(vaxis.PasteEndEvent{}, now)

	if string(st.apikeysInput) != "sk-pasted-key" {
		t.Fatalf("pasted key must land in the masked buffer, got %q", string(st.apikeysInput))
	}
	if strings.Contains(st.composer, "sk-pasted") || strings.Contains(st.composer, "[pasted") {
		t.Fatalf("paste leaked into the composer: %q", st.composer)
	}
}

// TestApikeysEscapePaths: Esc backs out of entry (zeroing the buffer) and out
// of the picker.
func TestApikeysEscapePaths(t *testing.T) {
	st, _, runner := openApikeys(t)
	now := time.Now()
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)
	for _, r := range "half-typed" {
		runner.HandleEvent(vaxis.Key{Text: string(r), Keycode: r}, now)
	}
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, now)
	if st.apikeysEntering || len(st.apikeysInput) != 0 {
		t.Fatal("Esc must leave entry and zero the buffer")
	}
	if !st.apikeysPicking {
		t.Fatal("Esc from entry returns to the list, not out of the picker")
	}
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, now)
	if st.apikeysPicking {
		t.Fatal("Esc from the list closes the picker")
	}
}

// TestApikeysGatedSignedOut: /apikeys is a gateway command — signed out it
// prompts for /login.
func TestApikeysGatedSignedOut(t *testing.T) {
	_, be, runner := newSignedOutRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/apikeys", now)
	for i := 0; i < 10; i++ {
		be.drain()
		_ = runner.HandleFrame(now)
	}
	if !strings.Contains(be.recorded(), "signed out — run /login") {
		t.Fatalf("/apikeys signed out must hit the login gate.\nrecorded=%q", be.recorded())
	}
}
