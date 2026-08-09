package provider

import (
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

func reqWithPin(pin string) wire.Request {
	return wire.Request{Pin: pin, Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}}}
}

// Native-endpoint selection: a provider's OWN API gets its shared
// full-fidelity adapter; everything else (local runtimes, compat clouds, the
// gateway path) stays on the generic chat/completions transport.
func TestNativeTurnTransportSelection(t *testing.T) {
	for base, wantNative := range map[string]bool{
		"https://api.openai.com/v1":             true,
		"https://API.OPENAI.COM/v1":             true, // host casing
		"https://api.x.ai/v1":                   true,
		"http://localhost:11434/v1":             false, // ollama
		"https://api.fireworks.ai/inference/v1": false,
		"https://api.groq.com/openai/v1":        false,
		"https://openrouter.ai/api/v1":          false,
	} {
		got := nativeTurnTransport(Endpoint{BaseURL: base, Key: "k", Model: "m"})
		if (got != nil) != wantNative {
			t.Errorf("%s: native=%v, want %v", base, got != nil, wantNative)
		}
	}
}

// The shim satisfies the turnTransport seam and refuses modelless calls with
// the actionable error before any network leaves the machine.
func TestNativeShimModelResolution(t *testing.T) {
	sh := &nativeShim{prov: nil, model: ""}
	sh.SetRetryNotify(func(int, error, time.Duration) {})
	if _, err := sh.finalize(reqWithPin("")); err == nil {
		t.Fatal("modelless native call must fail locally")
	}
	r, err := sh.finalize(reqWithPin("gpt-5.6-terra"))
	if err != nil || r.Model != "gpt-5.6-terra" {
		t.Fatalf("pin must become the model: %v %q", err, r.Model)
	}
	sh.model = "gpt-5.6-luna"
	r, err = sh.finalize(reqWithPin(""))
	if err != nil || r.Model != "gpt-5.6-luna" {
		t.Fatalf("endpoint default must fill a pinless call: %v %q", err, r.Model)
	}
}

// A custom port/path on a native host must reach the adapter — an
// enterprise-proxy URL silently talking to the real vendor was the bug.
func TestNativeBaseURLPassThrough(t *testing.T) {
	type based interface{ BaseURL() string }
	cases := map[string]string{
		"https://api.openai.com/v1":                    "", // vendor default → no override
		"https://api.openai.com:8443/corp/v1":          "https://api.openai.com:8443/corp/v1",
		"https://api.x.ai/v1":                          "",
		"https://api.x.ai/v1beta":                      "https://api.x.ai/v1beta",
		"https://api.anthropic.com":                    "",
		"https://api.anthropic.com/proxy":              "https://api.anthropic.com/proxy",
		"https://generativelanguage.googleapis.com":    "",
		"https://generativelanguage.googleapis.com/gw": "https://generativelanguage.googleapis.com/gw",
	}
	for base, want := range cases {
		tr := nativeTurnTransport(Endpoint{BaseURL: base, Key: "k", Model: "m"})
		if tr == nil {
			t.Fatalf("%s: expected a native transport", base)
		}
		b, ok := tr.(*nativeShim).prov.(based)
		if !ok {
			t.Fatalf("%s: adapter has no BaseURL()", base)
		}
		if got := b.BaseURL(); got != want && !(want == "" && got == vendorSelfDefault(base)) {
			t.Errorf("%s: adapter base = %q, want %q", base, got, want)
		}
	}
}

// vendorSelfDefault: grok's constructor sets its own baseURL to the vendor
// default (the SDK default is OpenAI's), so a default x.ai URL reads back as
// that constant rather than "".
func vendorSelfDefault(base string) string {
	if strings.Contains(base, "api.x.ai") {
		return "https://api.x.ai/v1"
	}
	return ""
}
