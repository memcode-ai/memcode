package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// fakeProvider records the request it served and answers canned.
type fakeProvider struct{ lastSystem, lastVolatile string }

func (f *fakeProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	f.lastSystem = r.System
	f.lastVolatile = r.SystemVolatile
	return wire.Response{
		StopReason: "end_turn",
		Blocks:     []wire.Block{wire.TextBlock("gateway says hi")},
		Model:      r.Model, Backend: "anthropic",
		InputTokens: 7, OutputTokens: 3,
	}, nil
}

func (f *fakeProvider) Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	if h.Text != nil {
		h.Text("gateway ")
		h.Text("says hi")
	}
	if h.Usage != nil {
		h.Usage(7, 3)
	}
	return f.Complete(ctx, r)
}
func newTestServer(t *testing.T) (*httptest.Server, *fakeProvider) {
	t.Helper()
	fp := &fakeProvider{}
	srv := httptest.NewServer(newCoreHandler(Config{
		SystemPrefix: "DOCTRINE",
		Provider:     fp,
		BackendName:  "test",
	}))
	t.Cleanup(srv.Close)
	return srv, fp
}

func TestAuthRequired(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(compatBody("auto", "hi")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token must 401, got %d", resp.StatusCode)
	}
	// healthz stays open for Cloud Run probes
	hr, _ := http.Get(srv.URL + "/healthz")
	if hr.StatusCode != http.StatusOK {
		t.Fatalf("healthz must be unauthenticated, got %d", hr.StatusCode)
	}
	hr.Body.Close()
}

// TestLegacyTurnSurfaceGone pins the one-wire migration: the memcode-shaped
// turn routes must not resolve anymore — the compat surface is the ONLY way to
// run a turn. (404 from the mux, not 401: the routes are unregistered, not
// merely gated.)
func TestLegacyTurnSurfaceGone(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{"/v1/code/chat", "/v1/complete", "/v1/stream", "/openai/v1/chat/completions"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer sekrit")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404 (legacy surface must be unmounted)", path, resp.StatusCode)
		}
	}
	// the old /openai/v1 models mount is gone too
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/openai/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /openai/v1/models = %d, want 404", resp.StatusCode)
	}
}

// newSplitTokenServer has DISTINCT user + internal tokens to prove /internal/*
// is not reachable with the user token the CLI ships.
func newSplitTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newCoreHandler(Config{
		Provider:    &fakeProvider{},
		BackendName: "test",
	}))
	t.Cleanup(srv.Close)
	return srv
}

// leakyProvider serves a Fireworks-pathed model id — the gateway must strip it before the
// response reaches the client.
type leakyProvider struct{}

func (leakyProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	return wire.Response{
		StopReason:  "end_turn",
		Blocks:      []wire.Block{wire.TextBlock("hi")},
		Model:       "accounts/fireworks/models/glm-5p1",
		Backend:     "fireworks",
		InputTokens: 5, OutputTokens: 2,
	}, nil
}

func (p leakyProvider) Stream(ctx context.Context, r wire.Request, _ wire.StreamHandler) (wire.Response, error) {
	return p.Complete(ctx, r)
}

// GUARD: a Fireworks-served turn must never leak the provider path to the
// client on the turn surface. (The streamed wire is covered by
// TestCompatStreamShape; the model list by TestModelsEndpoint.)
func TestNoProviderLeakToClient(t *testing.T) {
	servingEnv(t)
	srv := httptest.NewServer(newCoreHandler(Config{Provider: leakyProvider{}, BackendName: "hybrid"}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(compatBody("glm-5p2", "hi")))
	req.Header.Set("Authorization", "Bearer user-tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	for _, leak := range []string{"fireworks", "accounts/"} {
		if strings.Contains(strings.ToLower(string(body)), leak) {
			t.Errorf("turn response leaked %q to the client: %s", leak, body)
		}
	}
}

// modelsBody decodes GET /v1/models with an INDEPENDENT struct (not the compat
// package's types) so the test pins the actual JSON keys on the wire.
type modelsBody struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		Memcode *struct {
			Name      string `json:"name"`
			Desc      string `json:"desc"`
			Group     string `json:"group"`
			Window    int    `json:"window"`
			Vision    bool   `json:"vision"`
			Reasoning bool   `json:"reasoning"`
			Byok      bool   `json:"byok"`
		} `json:"memcode"`
	} `json:"data"`
	Memcode *struct {
		CreditsExhausted bool     `json:"credits_exhausted"`
		Backend          string   `json:"backend"`
		Vendors          []string `json:"vendors"`
		Roles            []struct {
			Role   string `json:"role"`
			ID     string `json:"id"`
			Label  string `json:"label"`
			Window int    `json:"window"`
		} `json:"roles"`
	} `json:"memcode"`
}

func getModels(t *testing.T, srv *httptest.Server) (modelsBody, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer user-tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got modelsBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got, raw
}

// TestModelsEndpoint pins the control-plane GET /v1/models shape: the
// standard OpenAI list ({object:"list", data:[{id, object:"model"}]}) whose
// ids are every SERVABLE catalog label (pinnable and not — pinnable is a
// picker fact riding the meta), extended with an ignorable `memcode` object
// per entry (selection facts: vendor/window/vision/pdf/reasoning/pinnable/
// byok) and on the list (credits_exhausted, backend, vendors, roles). There
// is no "auto" row — Automatic is CLI policy now. No provider identity may
// leak — labels only; "fireworks" appears ONLY as a vendor identity fact
// (the CLI's steering needs it), never as a served-path id.
func TestModelsEndpoint(t *testing.T) {
	t.Setenv("MEMCODE_PROVIDER", "hybrid")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("MEMCODE_FIREWORKS_URL", "http://cheap.invalid/v1")
	srv, _ := newTestServer(t)

	// unauthenticated → 401 (same auth door as the turn surface)
	r0, _ := http.Get(srv.URL + "/v1/models")
	r0.Body.Close()
	if r0.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated models = %d, want 401", r0.StatusCode)
	}

	got, raw := getModels(t, srv)
	if got.Object != "list" {
		t.Fatalf("object = %q, want \"list\" (%s)", got.Object, raw)
	}
	if len(got.Data) == 0 {
		t.Fatalf("empty model list: %s", raw)
	}

	ids := map[string]bool{}
	sawReasoning := false
	for _, m := range got.Data {
		ids[m.ID] = true
		if m.Object != "model" {
			t.Errorf("entry %q object = %q, want \"model\"", m.ID, m.Object)
		}
		if strings.Contains(m.ID, "/") {
			t.Errorf("raw provider id leaked into the model list: %q", m.ID)
		}
		if m.Memcode == nil || m.Memcode.Window <= 0 {
			t.Errorf("servable %q must carry memcode meta with a window: %+v", m.ID, m.Memcode)
			continue
		}
		if m.Memcode.Reasoning {
			sawReasoning = true
		}
	}
	for _, want := range []string{"sonnet", "terra", "glm-5p2", "gpt-oss-120b"} {
		if !ids[want] {
			t.Errorf("model list missing %q (got %s)", want, raw)
		}
	}
	if ids["auto"] {
		t.Error("the \"auto\" row must be gone — Automatic is CLI policy")
	}
	if !sawReasoning {
		t.Error("no entry reports reasoning=true — the catalog reasoning fact must ride the wire")
	}

	// list-level extension: org + routing facts the CLI picker needs.
	if got.Memcode == nil {
		t.Fatalf("list-level memcode extension missing: %s", raw)
	}
	if got.Memcode.CreditsExhausted {
		t.Error("a funded org must report credits_exhausted=false")
	}
	if got.Memcode.Backend != "test" {
		t.Errorf("backend = %q, want the configured backend name", got.Memcode.Backend)
	}
	vendors := map[string]bool{}
	for _, v := range got.Memcode.Vendors {
		vendors[v] = true
	}
	if !vendors["openai"] || !vendors["anthropic"] {
		t.Errorf("vendors = %v — hybrid with both keys must report openai + anthropic", got.Memcode.Vendors)
	}
	roles := map[string]bool{}
	for _, rm := range got.Memcode.Roles {
		if rm.ID == "" || rm.Label == "" {
			t.Errorf("role %q missing id/label: %+v", rm.Role, rm)
		}
		roles[rm.Role] = true
	}
	for _, want := range []string{"planner", "reviewer", "standard", "classify"} {
		if !roles[want] {
			t.Errorf("models extension missing role %q (got %+v)", want, got.Memcode.Roles)
		}
	}

	// The leak guard covers the WHOLE body: raw provider PATHS never ride the
	// wire. Vendor IDENTITY ("fireworks") is a deliberate control-plane fact
	// now — client-side steering keys on it, and /apikeys already names it.
	if strings.Contains(strings.ToLower(string(raw)), "accounts/") {
		t.Errorf("/v1/models leaked a raw provider path: %s", raw)
	}
}
