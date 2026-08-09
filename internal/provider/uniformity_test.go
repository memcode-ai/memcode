package provider_test

// uniformity_test.go — THE acceptance test for "memcode behaves the same
// everywhere" (all-policy-client-side): one scripted agent session runs
// through the REAL stack — env-driven backend selection → conn → the compat
// transport → the Runner's selection/recovery policy — against (a) a
// gateway-shaped compat server and (b) the same server presenting as a bare
// third-party endpoint. The request wire must be IDENTICAL in shape (no
// memcode headers anywhere, model in the body, the two-system convention),
// and the policy identical in behavior (ladder decisions, visible fallback on
// model errors), differing only in the available-model set — hosted selection
// runs on the control plane; an endpoint serves exactly its session model.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// compatServer is a minimal OpenAI-compat backend: GET /v1/models +
// POST /v1/chat/completions, recording every request, scriptable per-model
// failures. gateway=true serves the memcode control-plane extensions.
type compatServer struct {
	mu       sync.Mutex
	models   []string
	gateway  bool
	failOnce map[string]bool // model → fail the next call with a 500
	bodies   []map[string]any
	headers  []http.Header
}

func (cs *compatServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		type entry struct {
			ID      string         `json:"id"`
			Object  string         `json:"object"`
			Memcode map[string]any `json:"memcode,omitempty"`
		}
		out := map[string]any{"object": "list"}
		var data []entry
		for _, m := range cs.models {
			e := entry{ID: m, Object: "model"}
			if cs.gateway {
				cm, _ := catalog.LookupModel(m)
				e.Memcode = map[string]any{"vendor": cm.Vendor, "window": cm.Window,
					"vision": cm.Vision, "pdf": cm.PDF, "pinnable": cm.Pinnable}
			}
			data = append(data, e)
		}
		out["data"] = data
		if cs.gateway {
			out["memcode"] = map[string]any{
				"backend": "test", "vendors": []string{"openai", "anthropic"},
				"roles": []map[string]any{
					{"role": "planner", "id": "glm-5p2", "label": "glm-5p2"},
					{"role": "reviewer", "id": "luna", "label": "luna"},
					{"role": "standard", "id": "glm-5p2", "label": "glm-5p2"},
					{"role": "classify", "id": "gpt-oss-120b", "label": "gpt-oss-120b"},
				},
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		cs.mu.Lock()
		cs.bodies = append(cs.bodies, body)
		cs.headers = append(cs.headers, r.Header.Clone())
		model, _ := body["model"].(string)
		fail := cs.failOnce[model]
		if fail {
			cs.failOnce[model] = false
		}
		cs.mu.Unlock()
		if fail {
			// A non-retryable failure (the transport retries 5xx itself with
			// backoff; a 400-class model failure surfaces immediately) — the
			// Runner's chain walk is the layer under test here.
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "backend exploded"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-x", "object": "chat.completion", "model": model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"}}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 1},
		})
	})
	return mux
}

func (cs *compatServer) requestedModels() []string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []string
	for _, b := range cs.bodies {
		m, _ := b["model"].(string)
		out = append(out, m)
	}
	return out
}

// completer is the one call surface the script drives — satisfied by the
// Runner and the failure-arming wrapper alike.
type completer interface {
	Complete(context.Context, llm.Purpose, wire.Request) (wire.Response, error)
}

// script runs the same four-turn agent session against any backend: a chat
// turn, a classify (judge) turn, a self-heal escalation, and a turn whose
// first model fails once (the recovery walk).
func script(t *testing.T, r completer) []error {
	t.Helper()
	turn := func(p llm.Purpose, mode, risk string) error {
		req := wire.Request{
			System: "STABLE-DOCTRINE", SystemVolatile: "[today: test]", Mode: mode,
			Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("do it")}}},
		}
		if risk != "" {
			req.RoutingHint = &wire.RoutingHint{Reason: risk}
		}
		_, err := r.Complete(context.Background(), p, req)
		return err
	}
	return []error{
		turn(llm.MainLoop, "exec", ""),
		turn(llm.Classify, "turn_intent", ""),
		turn(llm.MainLoop, "exec", "self_heal"),
		turn(llm.MainLoop, "exec", ""), // the scripted failure fires here per-backend
	}
}

// assertUniformWire holds every recorded request to the shared wire shape:
// model in the body, two system messages, session affinity via `user` — and
// ZERO memcode headers (the request sent to the gateway is byte-shape-
// identical to the one sent to any endpoint).
func assertUniformWire(t *testing.T, cs *compatServer, wantAuth string) {
	t.Helper()
	for i, h := range cs.headers {
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "x-memcode") {
				t.Errorf("request %d carries %s — no memcode headers exist on any backend", i, k)
			}
		}
		if got := h.Get("Authorization"); got != wantAuth {
			t.Errorf("request %d Authorization = %q, want %q", i, got, wantAuth)
		}
	}
	for i, b := range cs.bodies {
		if m, _ := b["model"].(string); m == "" || m == "auto" {
			t.Errorf("request %d model = %q — every request names a concrete model", i, b["model"])
		}
		msgs, _ := b["messages"].([]any)
		if len(msgs) < 3 {
			t.Errorf("request %d: want two system messages + user, got %d messages", i, len(msgs))
		}
	}
}

func TestBackendUniformity(t *testing.T) {
	// ── (a) the gateway-shaped backend: hosted policy over the control plane ──
	gw := &compatServer{gateway: true,
		models:   []string{"sol", "terra", "luna", "opus", "sonnet", "haiku", "glm-5p2", "kimi-k2p7-code", "kimi-k2p6", "kimi-k3", "gpt-oss-120b"},
		failOnce: map[string]bool{},
	}
	gwSrv := httptest.NewServer(gw.handler())
	t.Cleanup(gwSrv.Close)

	t.Setenv(provider.EnvAPIURL, gwSrv.URL)
	t.Setenv(provider.EnvAPIToken, "memcode_uniformity")
	t.Setenv(provider.EnvEndpointURL, "")
	t.Setenv(provider.EnvEndpointKey, "")
	t.Setenv(provider.EnvEndpointModel, "")

	prov := provider.NewFromEnvLazy()
	if _, onEP := prov.Endpoint(); onEP {
		t.Fatal("token present → hosted backend expected")
	}
	runner := llm.NewRunner(prov)

	// Turn 4's primary (the standard role, glm-5p2) fails once → the catalog
	// chain rescues on kimi-k2p7-code. Scripted AFTER turns 1-3 so only turn 4
	// trips it: failOnce is per-model and turn 1 also serves glm-5p2.
	errs := script(t, &scriptedFail{Runner: runner, gw: gw})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("hosted turn %d failed: %v", i+1, err)
		}
	}
	wantHosted := []string{"glm-5p2", "gpt-oss-120b", "sol", "glm-5p2", "kimi-k2p7-code"}
	if got := gw.requestedModels(); !equalStrings(got, wantHosted) {
		t.Fatalf("hosted decision sequence = %v, want %v", got, wantHosted)
	}
	assertUniformWire(t, gw, "Bearer memcode_uniformity")
	// The hosted extras: delegate doctrine on the cheap-lane exec turn, absent
	// on the strong-tier turn.
	if sys := systemText(gw.bodies[0]); !strings.Contains(sys, "Match the work to the model") {
		t.Error("hosted cheap-lane exec turn must carry the delegate doctrine")
	}
	if sys := systemText(gw.bodies[2]); strings.Contains(sys, "Match the work to the model") {
		t.Error("the frontier-tier turn must not carry the delegate doctrine")
	}

	// ── (b) the SAME server as a bare endpoint: same wire, no policy extras ──
	ep := &compatServer{gateway: false, models: []string{"qwen3:4b"}, failOnce: map[string]bool{}}
	epSrv := httptest.NewServer(ep.handler())
	t.Cleanup(epSrv.Close)

	t.Setenv(provider.EnvAPIToken, "")
	t.Setenv(provider.EnvEndpointURL, epSrv.URL+"/v1")
	t.Setenv(provider.EnvEndpointModel, "qwen3:4b")

	eprov := provider.NewFromEnvLazy()
	if _, onEP := eprov.Endpoint(); !onEP {
		t.Fatal("endpoint URL without a token → endpoint backend expected")
	}
	erunner := llm.NewRunner(eprov)
	for i, err := range script(t, &scriptedFail{Runner: erunner}) {
		if err != nil {
			t.Fatalf("endpoint turn %d failed: %v", i+1, err)
		}
	}
	// Every turn — chat, judge, escalation — serves the ONE session model:
	// the endpoint is the whole model set, and the ladder degrades to it.
	for i, m := range ep.requestedModels() {
		if m != "qwen3:4b" {
			t.Fatalf("endpoint request %d model = %q, want the session model", i, m)
		}
	}
	assertUniformWire(t, ep, "")
	for i, b := range ep.bodies {
		raw, _ := json.Marshal(b)
		if strings.Contains(string(raw), "memcode") {
			t.Errorf("endpoint request %d carries a memcode extension: %s", i, raw)
		}
	}
}

// scriptedFail arms the gateway failure right before the script's 4th turn.
type scriptedFail struct {
	*llm.Runner
	gw   *compatServer
	seen int
}

func (s *scriptedFail) Complete(ctx context.Context, p llm.Purpose, req wire.Request) (wire.Response, error) {
	s.seen++
	if s.seen == 4 && s.gw != nil {
		s.gw.mu.Lock()
		s.gw.failOnce["glm-5p2"] = true
		s.gw.mu.Unlock()
	}
	return s.Runner.Complete(ctx, p, req)
}

func systemText(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	m, _ := msgs[0].(map[string]any)
	txt, _ := m["content"].(string)
	return txt
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
