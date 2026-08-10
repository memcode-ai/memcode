package provider

// credsource.go — turning credentials the developer ALREADY has into a working
// backend, so the first turn needs no memcode account. The lowest-friction
// front door: someone with an exported provider key (the same key their shell
// already uses for `openai`, `curl api.anthropic.com`, another agent) gets a
// live agent with zero setup.
//
// Consent line: an EXPORTED ENV KEY is an explicit action by the user in their
// own shell, so activating it is consent-satisfied and silent. Credentials that
// live in ANOTHER tool's files or the OS keychain (a Claude Code / Codex login,
// a gh token) are NOT — the user signed into that tool, not memcode — so those
// sources never auto-activate here; they are offered only through the first-run
// wizard's explicit opt-in.
//
// Precedence in the selection ladder (NewFromEnv / NewFromEnvLazy): a memcode
// login and an explicit MEMCODE_ENDPOINT_URL both outrank discovery. This is
// the fallback that lights up an ambient key when nothing else is configured,
// never an override of a backend the user chose.

import (
	"os"
	"strings"
)

// ownKeyVendors lists the direct-provider hosts memcode dials when the matching
// ecosystem-standard key is exported, in priority order. Each BaseURL is a
// native host (native.go), so the key rides that vendor's own auth convention
// (Anthropic x-api-key, OpenAI Bearer) via the full-fidelity adapter — the same
// implementation the hosted gateway runs.
var ownKeyVendors = []struct {
	env     string
	baseURL string
	name    string
}{
	{"ANTHROPIC_API_KEY", "https://api.anthropic.com", "anthropic"},
	{"OPENAI_API_KEY", "https://api.openai.com/v1", "openai"},
}

// OwnKeyVendor reports the vendor name when a base URL is a direct-provider
// host memcode dials with an exported key ("anthropic", "openai"), ok=false for
// a generic/local endpoint. Lets diagnostics name an own-key backend for what
// it is instead of calling it a nameless "custom endpoint".
func OwnKeyVendor(baseURL string) (string, bool) {
	for _, v := range ownKeyVendors {
		if strings.EqualFold(strings.TrimRight(baseURL, "/"), v.baseURL) {
			return v.name, true
		}
	}
	return "", false
}

// discoverCredentialEndpoint returns a direct-provider endpoint built from an
// ambient exported key, ok=false when none is present. The model is left unset:
// endpoint mode resolves it from the catalog / the /model picker exactly as for
// any custom endpoint. Subscription sources (a Claude/Codex/Copilot login) do
// NOT flow through here — they are wizard-gated (see the consent line above).
func discoverCredentialEndpoint() (Endpoint, bool) {
	for _, v := range ownKeyVendors {
		if key := strings.TrimSpace(os.Getenv(v.env)); key != "" {
			return Endpoint{Name: v.name, BaseURL: v.baseURL, Key: key}, true
		}
	}
	return Endpoint{}, false
}
