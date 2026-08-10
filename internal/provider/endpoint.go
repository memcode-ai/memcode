package provider

// endpoint.go — arbitrary-endpoint mode (one-wire Phase C): pointing the CLI at
// any OpenAI-compatible base URL (Ollama, LM Studio, vLLM, a provider cloud)
// instead of the memcode gateway. The endpoint serves TURNS on the same compat
// transport the gateway path uses (sdk providers/compat — no
// memcode extensions, no routing headers, no auth header unless a key is set);
// the gateway-only side channels (websearch/webfetch/advisor), BYOK, artifacts
// and websites are simply absent. Backend selection lives in NewFromEnv /
// NewFromEnvLazy: memcode token → hosted; else configured endpoint → local;
// else signed out.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/catalog"
)

const (
	// EnvEndpointURL is the FULL compat base including any path prefix
	// (Groq-style), e.g. http://localhost:11434/v1 — {base}/chat/completions is
	// the turn endpoint. Setting it (with no memcode token) puts the CLI in
	// endpoint mode.
	EnvEndpointURL = "MEMCODE_ENDPOINT_URL"
	// EnvEndpointKey is the endpoint's optional bearer credential; unset sends
	// no Authorization header (a keyless local endpoint).
	EnvEndpointKey = "MEMCODE_ENDPOINT_KEY"
	// EnvEndpointModel is the endpoint's INITIAL model id — used until a /model
	// choice is remembered for the endpoint (config wins once one exists).
	EnvEndpointModel = "MEMCODE_ENDPOINT_MODEL"
)

// Endpoint describes one arbitrary OpenAI-compatible endpoint. Resolved from
// the environment (EndpointFromEnv) or from the project config's named endpoint
// list (config.ResolveEndpoint merges the two).
type Endpoint struct {
	Name    string   // short display name ("ollama", or the host:port for env endpoints)
	BaseURL string   // full compat base incl. any path prefix (http://localhost:11434/v1)
	Key     string   // optional bearer; "" = no Authorization header
	Model   string   // session model id ("" = resolve via GET {base}/models or /model)
	Models  []string // optional curated picker list / allowlist from config

	// Headers are extra request headers a subscription backend requires to
	// accept the turn (a Copilot endpoint's Editor-Version / integration id).
	// Set only by the credential sources; empty for a normal endpoint. Carried
	// through to the compat transport.
	Headers map[string]string
}

// EndpointFromEnv resolves the env-configured endpoint (the dotenv chain loads
// MEMCODE_ENDPOINT_* like every other knob). ok=false when no URL is set.
func EndpointFromEnv() (Endpoint, bool) {
	base := strings.TrimSpace(os.Getenv(EnvEndpointURL))
	if base == "" {
		return Endpoint{}, false
	}
	key := strings.TrimSpace(os.Getenv(EnvEndpointKey))
	if key == "" {
		key = ConventionalKey(base)
	}
	return Endpoint{
		Name:    EndpointName(base),
		BaseURL: base,
		Key:     key,
		Model:   strings.TrimSpace(os.Getenv(EnvEndpointModel)),
	}, true
}

// conventionalKeyVars maps well-known provider hosts to the ecosystem-standard
// API-key env var every tool reads. Credential CONVENIENCE, not routing: an
// explicit MEMCODE_ENDPOINT_KEY or a config key_env always wins; this only
// fills the gap when someone points the CLI at a provider cloud with nothing
// configured — the same shell that runs `openai` or `curl api.openai.com`
// already exports these.
var conventionalKeyVars = map[string]string{
	"api.anthropic.com":                 "ANTHROPIC_API_KEY",
	"api.openai.com":                    "OPENAI_API_KEY",
	"api.fireworks.ai":                  "FIREWORKS_API_KEY",
	"api.groq.com":                      "GROQ_API_KEY",
	"api.x.ai":                          "XAI_API_KEY",
	"api.mistral.ai":                    "MISTRAL_API_KEY",
	"api.deepseek.com":                  "DEEPSEEK_API_KEY",
	"api.together.xyz":                  "TOGETHER_API_KEY",
	"openrouter.ai":                     "OPENROUTER_API_KEY",
	"api.cerebras.ai":                   "CEREBRAS_API_KEY",
	"generativelanguage.googleapis.com": "GEMINI_API_KEY",
}

// ConventionalKey returns the standard env var's value for a known provider
// host, "" for local/unknown endpoints (a keyless Ollama stays keyless).
func ConventionalKey(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if v, ok := conventionalKeyVars[host]; ok {
		return strings.TrimSpace(os.Getenv(v))
	}
	return ""
}

// EndpointName derives a short display name from a base URL — the host (with
// port) for a valid URL, the raw string otherwise. Config-listed endpoints
// carry their own names; this covers the env-defined ones.
func EndpointName(base string) string {
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return base
}

// ErrNoEndpointModel is returned when a turn reaches an endpoint transport
// with no model anywhere (no session pin, no endpoint default).
var ErrNoEndpointModel = errors.New("no model selected for this endpoint — pick one with /model, or set MEMCODE_ENDPOINT_MODEL")

// ErrGatewayOnly is returned by the side-channel capabilities (websearch /
// webfetch / advisor) in endpoint mode: they are memcode gateway services with
// no compat equivalent. Callers already degrade on error (webfetch falls back
// to the local fetch; the web_search tool def isn't advertised off-gateway).
var ErrGatewayOnly = errors.New("not available on a custom endpoint — this needs the memcode gateway (run /login)")

// Endpointer is the endpoint-mode introspection capability, sibling to
// Connector: present on *Lazy (and the raw conn), absent on test fakes. The
// runtime forwards it (Session.Endpoint) so the TUI can drive the /model
// picker, cost display, and capability gating off the ACTIVE backend rather
// than env sniffing.
type Endpointer interface {
	Endpoint() (Endpoint, bool)
}

// CatalogWindow returns the embedded catalog's context window for a model id
// it KNOWS (exact id or label) — 0 otherwise. Endpoint mode keys on it for the
// /model picker's window column and the pin's meter sizing: known ids get real
// numbers, unknown local models get blank, never a made-up default.
func CatalogWindow(id string) int {
	if m, ok := catalog.LookupModel(id); ok && m.Window > 0 {
		return m.Window
	}
	return 0
}

// CatalogKnows reports whether the embedded catalog has a real entry for a
// model id — the cost-display gate: uncataloged (local) models show token
// counts, not a $ figure priced off the defaults card.
func CatalogKnows(id string) bool {
	_, ok := catalog.LookupModel(id)
	return ok
}

// endpointModelList is the standard GET {base}/models body.
type endpointModelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// EndpointModels lists the endpoint's model ids via GET {base}/models (part of
// the compat standard — OpenAI, Groq, Ollama, LM Studio, vLLM all serve it).
// When the base was configured WITHOUT its /v1 path prefix, {base}/models 404s
// on most local runtimes — one retry at {base}/v1/models covers that without
// the CLI ever rewriting the configured base for turns. Errors (endpoint
// lacks the route entirely) return nil — the /model picker then falls back to
// the config list / free-text entry.
func EndpointModels(ctx context.Context, ep Endpoint) []string {
	bases := []string{strings.TrimRight(ep.BaseURL, "/")}
	if !strings.HasSuffix(bases[0], "/v1") {
		bases = append(bases, bases[0]+"/v1")
	}
	for _, base := range bases {
		ids, err := fetchEndpointModels(ctx, base+"/models", ep.Key)
		if err == nil && len(ids) > 0 {
			return ids
		}
	}
	return nil
}

// shapeListAuth applies the vendor's list-route credential convention:
// Anthropic wants x-api-key + anthropic-version (Bearer alone 401s), the
// Gemini API wants the key as a ?key= query param, and everything
// OpenAI-compatible takes the standard Bearer. Returns the (possibly
// rewritten) URL; headers are set on hdr.
func shapeListAuth(rawURL, key string, hdr http.Header) string {
	if key == "" {
		return rawURL
	}
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	switch host {
	case "api.anthropic.com":
		hdr.Set("x-api-key", key)
		hdr.Set("anthropic-version", "2023-06-01")
	case "generativelanguage.googleapis.com":
		sep := "?"
		if strings.Contains(rawURL, "?") {
			sep = "&"
		}
		rawURL += sep + "key=" + url.QueryEscape(key)
	default:
		hdr.Set("Authorization", "Bearer "+key)
	}
	return rawURL
}

// fetchEndpointModels performs one GET models call against one candidate URL,
// with vendor-aware credential shaping (shapeListAuth).
func fetchEndpointModels(ctx context.Context, rawURL, key string) ([]string, error) {
	hdr := http.Header{}
	rawURL = shapeListAuth(rawURL, key, hdr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("endpoint returned %s", resp.Status)
	}
	var body endpointModelList
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
