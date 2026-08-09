package provider

// native.go — full-fidelity NATIVE endpoints: when the configured endpoint IS
// a provider's own API, the CLI uses the SHARED wire adapter for that
// provider's dialect (the sdk-go providers/* packages) instead of forcing the
// generic chat/completions contract — api.openai.com speaks the Responses API
// (reasoning + tools together, encrypted-reasoning round-trip), exactly the
// same implementation the hosted gateway runs, selected by base URL. One
// implementation per provider protocol, reused everywhere; policy stays in
// the Runner; generic compat endpoints (Ollama, Fireworks, Groq, vLLM, the
// gateway) keep the chat/completions transport.

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/providers/anthropic"
	"github.com/memcode-ai/memcode/internal/providers/gemini"
	"github.com/memcode-ai/memcode/internal/providers/openai"
	"github.com/memcode-ai/memcode/internal/wire"
)

// nativeDefaults maps each native host to the base the vendor SDK would use
// anyway — a configured URL that matches it needs no override; anything else
// (custom port, path prefix, corp proxy on the same hostname) is passed
// through so the user's URL is honored byte-for-byte.
var nativeDefaults = map[string]string{
	"api.openai.com":                    "https://api.openai.com/v1",
	"api.x.ai":                          "https://api.x.ai/v1",
	"api.anthropic.com":                 "https://api.anthropic.com",
	"generativelanguage.googleapis.com": "https://generativelanguage.googleapis.com",
}

// nativeTurnTransport returns the shared native adapter for an endpoint whose
// base URL is a provider's own API, nil for everything else (generic compat).
func nativeTurnTransport(ep Endpoint) turnTransport {
	u, err := url.Parse(ep.BaseURL)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	override := ""
	if def, ok := nativeDefaults[host]; ok {
		if b := strings.TrimRight(ep.BaseURL, "/"); !strings.EqualFold(b, def) {
			override = b
		}
	}
	switch host {
	case "api.openai.com":
		p := openai.NewOpenAI(ep.Key)
		if override != "" {
			p.SetBaseURL(override)
		}
		return &nativeShim{prov: p, model: ep.Model}
	case "api.x.ai":
		p := openai.NewGrok(ep.Key)
		if override != "" {
			p.SetBaseURL(override)
		}
		return &nativeShim{prov: p, model: ep.Model}
	case "api.anthropic.com":
		p := anthropic.NewAnthropic(ep.Key)
		if override != "" {
			p.SetBaseURL(override)
		}
		return &nativeShim{prov: p, model: ep.Model}
	case "generativelanguage.googleapis.com":
		p := gemini.NewGemini(ep.Key)
		if override != "" {
			p.SetBaseURL(override)
		}
		return &nativeShim{prov: p, model: ep.Model}
	}
	return nil
}

// nativeProvider is the call surface the shared adapters expose.
type nativeProvider interface {
	Complete(ctx context.Context, r wire.Request) (wire.Response, error)
	Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error)
}

// nativeShim adapts a shared wire adapter to the turnTransport seam: it
// finalizes the model (the session pin, else the endpoint default), composes
// legacy-shaped side calls exactly like the compat transport, and satisfies
// the retry-notify seam (a no-op — the shared adapters run their own bounded
// retry).
type nativeShim struct {
	prov  nativeProvider
	model string
}

func (n *nativeShim) finalize(r wire.Request) (wire.Request, error) {
	if r.Mode != "" && r.SystemVolatile == "" {
		var err error
		if r, err = composeDoctrine(r); err != nil {
			return r, err
		}
	}
	r.Model = r.Pin
	if r.Model == "" {
		r.Model = n.model
	}
	if r.Model == "" {
		return r, ErrNoEndpointModel
	}
	return r, nil
}

func (n *nativeShim) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	r, err := n.finalize(r)
	if err != nil {
		return wire.Response{}, err
	}
	return n.prov.Complete(ctx, r)
}

func (n *nativeShim) Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	r, err := n.finalize(r)
	if err != nil {
		return wire.Response{}, err
	}
	return n.prov.Stream(ctx, r, h)
}

func (n *nativeShim) SetRetryNotify(func(attempt int, err error, delay time.Duration)) {}

var _ turnTransport = (*nativeShim)(nil)
