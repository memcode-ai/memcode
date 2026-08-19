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
	"context"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/subscription/claudesub"
	"github.com/memcode-ai/memcode/internal/subscription/codex"
	"github.com/memcode-ai/memcode/internal/subscription/copilot"
	"github.com/memcode-ai/memcode/internal/subscription/grok"
)

// EnvCredentialSource names the explicitly-selected credential source. Empty =
// own exported keys only (the ambient path). Set to a subscription source
// ("copilot", …) — by the first-run wizard or by hand — to activate a login
// that lives in another tool's files/keychain, which never auto-activates.
const EnvCredentialSource = "MEMCODE_CREDENTIAL_SOURCE"

// ownKeyVendors lists the direct-provider hosts memcode dials when the matching
// ecosystem-standard key is exported, in priority order. Each BaseURL is a
// native host (native.go), so the key rides that vendor's own auth convention
// (Anthropic x-api-key, OpenAI Bearer) via the full-fidelity adapter — the same
// implementation the hosted gateway runs.
var ownKeyVendors = []struct {
	env      string
	baseURL  string
	name     string
	defModel string
}{
	{"ANTHROPIC_API_KEY", "https://api.anthropic.com", "anthropic", catalog.ModelSonnet},
	{"OPENAI_API_KEY", "https://api.openai.com/v1", "openai", catalog.ModelTerra},
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
	// An explicitly-selected subscription source wins: the user chose it, so it
	// is allowed to read a login from another tool. Selection is required — a
	// subscription source NEVER activates from ambient state alone.
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvCredentialSource))) {
	case "copilot":
		if ep, ok := resolveCopilot(); ok {
			return ep, true
		}
		// Selected but unresolved (not signed in, Copilot disabled): fall
		// through so an exported own key can still connect.
	case "codex":
		if ep, ok := resolveCodex(); ok {
			return ep, true
		}
	case "claude", "claude-sub", "anthropic-sub":
		if ep, ok := resolveClaudeSub(); ok {
			return ep, true
		}
	case "grok", "grok-sub", "xai-sub":
		if ep, ok := resolveGrok(); ok {
			return ep, true
		}
	}
	for _, v := range ownKeyVendors {
		if key := strings.TrimSpace(os.Getenv(v.env)); key != "" {
			return Endpoint{Name: v.name, BaseURL: v.baseURL, Key: key, Model: sourceModel(v.defModel)}, true
		}
	}
	return Endpoint{}, false
}

// ServingLabel maps an endpoint's internal name to the word a user chose in
// the auth wizard — the "via X" in the TUI's served-by line. Empty for
// endpoints that aren't a subscription source (their Name already reads fine).
func ServingLabel(name string) string {
	switch name {
	case "claude-sub":
		return "claude"
	case "codex":
		return "codex"
	case "copilot":
		return "github"
	case "grok-sub":
		return "grok"
	}
	return name
}

// SubscriptionEndpointName reports whether an endpoint name is one of the
// wizard-selected subscription sources (vs a custom endpoint or own key).
func SubscriptionEndpointName(name string) bool {
	switch name {
	case "claude-sub", "codex", "copilot", "grok-sub":
		return true
	}
	return false
}

// ExplicitCredentialSource reports the user has actively selected a
// credential source (memcode auth …) — the consent signal that lets a
// subscription login outrank other backends.
func ExplicitCredentialSource() bool {
	return strings.TrimSpace(os.Getenv(EnvCredentialSource)) != ""
}

// sourceModel resolves a subscription source's initial model: an explicit
// MEMCODE_ENDPOINT_MODEL override wins, else the source's sensible default so a
// subscription "just works" with no configuration. The /model picker changes it
// per session.
func sourceModel(def string) string {
	if m := strings.TrimSpace(os.Getenv(EnvEndpointModel)); m != "" {
		return m
	}
	return def
}

// resolveCopilot exchanges the machine's GitHub token for a Copilot backend and
// shapes it as an endpoint on the compat transport (Copilot speaks
// chat/completions). ok=false when Copilot can't be resolved.
func resolveCopilot() (Endpoint, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b, err := copilot.Resolve(ctx)
	if err != nil {
		return Endpoint{}, false
	}
	return Endpoint{
		Name:    "copilot",
		BaseURL: b.BaseURL,
		Key:     b.Token,
		Headers: b.Headers,
		Model:   sourceModel("gpt-4o"), // widely served by Copilot; /model to change
	}, true
}

// resolveCodex reuses the Codex CLI login and shapes it as an endpoint on the
// ChatGPT/Codex backend (the native Responses adapter, base-overridden).
func resolveCodex() (Endpoint, bool) {
	b, err := codex.Resolve()
	if err != nil {
		return Endpoint{}, false
	}
	return Endpoint{
		Name:    "codex",
		BaseURL: b.BaseURL,
		Key:     b.Token,
		Headers: b.Headers,
		Model:   sourceModel(catalog.ModelTerra), // a current Codex model; /model to change
	}, true
}

// resolveClaudeSub reuses the Claude Code login and shapes it as an endpoint on
// api.anthropic.com. The token is a subscription OAuth token, so the native
// Anthropic adapter switches itself into Claude Code compatibility mode purely
// from the token shape (never from the host).
// resolveGrok serves the memcode-owned SuperGrok / X Premium+ login (memcode
// runs that OAuth flow itself — see the grok package) as a bearer on api.x.ai,
// riding the shared Grok Responses adapter unchanged.
func resolveGrok() (Endpoint, bool) {
	tok, err := grok.Resolve()
	if err != nil {
		return Endpoint{}, false
	}
	return Endpoint{
		Name:    "grok-sub",
		BaseURL: grok.BaseURL,
		Key:     tok,
		Model:   sourceModel(catalog.ModelGrok46), // the catalog's Grok tier; /model to change
	}, true
}

func resolveClaudeSub() (Endpoint, bool) {
	tok, err := claudesub.Resolve()
	if err != nil {
		return Endpoint{}, false
	}
	return Endpoint{
		Name:    "claude-sub",
		BaseURL: "https://api.anthropic.com",
		Key:     tok,
		Model:   sourceModel(catalog.ModelSonnet), // Claude Sonnet by default; /model to change
	}, true
}
