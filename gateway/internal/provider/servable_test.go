package provider

// servable_test.go — the strict serving gate + the /v1/models control plane
// (all-policy-client-side): the gateway serves exactly the catalog labels it
// has credentials for, and /v1/models exposes every fact CLI-side selection
// reads. Replaces the retired pin-gate tests (the pin concept died with
// server-side routing).

import (
	"testing"

	"github.com/memcode-ai/memcode/gateway/internal/identity"
)

func hybridEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvProvider, "hybrid")
	t.Setenv(EnvOpenAIKey, "sk-oa")
	t.Setenv(EnvAPIKey, "sk-ant")
	t.Setenv(EnvGrokKey, "sk-grok")
	t.Setenv(EnvFireworksURL, "https://fw.example/v1")
	t.Setenv(EnvGeminiKey, "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS_JSON", "")
}

func TestLookupServableGate(t *testing.T) {
	hybridEnv(t)

	// Servable labels resolve to their raw ids — including NON-pinnable chat
	// labels (the CLI's classify lane requests gpt-oss-120b directly now).
	for label, wantID := range map[string]string{
		"sonnet":       "claude-sonnet-5",
		"glm-5p2":      "accounts/fireworks/models/glm-5p2",
		"gpt-oss-120b": "accounts/fireworks/models/gpt-oss-120b",
		"terra":        "gpt-5.6-terra",
	} {
		id, ok := LookupServable(label)
		if !ok || id != wantID {
			t.Errorf("LookupServable(%q) = (%q, %v), want (%q, true)", label, id, ok, wantID)
		}
	}

	// Everything else refuses: the old logical ids ("auto", vendor names),
	// typos, non-chat rows (embeddings/images), and unkeyed vendors.
	for _, label := range []string{"", "auto", "openai", "anthropic", "bogus",
		"gemini-embedding-001", "gpt-image-1", "gemini-flash"} {
		if _, ok := LookupServable(label); ok {
			t.Errorf("LookupServable(%q) = ok, want refused", label)
		}
	}

	// gemini-flash refuses above ONLY because no gemini creds are set — with a
	// key it serves.
	t.Setenv(EnvGeminiKey, "sk-gem")
	if _, ok := LookupServable("gemini-flash"); !ok {
		t.Error("LookupServable(gemini-flash) with a gemini key must serve")
	}
}

func TestServableModelsForControlPlane(t *testing.T) {
	hybridEnv(t)

	who := identity.Info{KeyedVendors: []string{"anthropic", "fireworks"}}
	models := ServableModelsFor(who)
	byLabel := map[string]ServableModel{}
	for _, m := range models {
		byLabel[m.Label] = m
		if m.Vendor == "" {
			t.Errorf("%s: no vendor — selection needs authoritative vendor identity", m.Label)
		}
		if m.Window <= 0 {
			t.Errorf("%s: non-chat row leaked into /v1/models", m.Label)
		}
	}

	// Byok stamps follow the user's keyed vendors, per-model.
	if !byLabel["sonnet"].Byok || !byLabel["glm-5p2"].Byok {
		t.Error("anthropic+fireworks keys must stamp byok on sonnet and glm-5p2")
	}
	if byLabel["terra"].Byok || byLabel["grok-4.5"].Byok {
		t.Error("unkeyed vendors must not stamp byok")
	}

	// The control plane carries the capability facts selection reads.
	if !byLabel["sonnet"].PDF || byLabel["glm-5p2"].PDF {
		t.Error("pdf fact wrong: sonnet has native PDF, glm-5p2 does not")
	}
	if byLabel["sonnet"].Vendor != "anthropic" || byLabel["glm-5p2"].Vendor != "fireworks" {
		t.Error("vendor fact wrong")
	}

	// Non-pinnable chat models are listed (servable), embeddings are not.
	if _, ok := byLabel["gpt-oss-120b"]; !ok {
		t.Error("gpt-oss-120b (classify lane) must be servable")
	}
	if byLabel["gpt-oss-120b"].Pinnable {
		t.Error("gpt-oss-120b must stay out of the picker (pinnable=false)")
	}
	if _, ok := byLabel["gemini-embedding-001"]; ok {
		t.Error("embedding rows must not be servable")
	}
	// No gemini creds in this env → gemini labels absent entirely.
	if _, ok := byLabel["gemini-flash"]; ok {
		t.Error("unkeyed gemini must not be listed")
	}
}
