package provider

import (
	"os"
	"strings"
)

// EnvCredentials is the ordered CONSENT LIST of attached credential sources
// ("claude,codex"). It replaces the single-value MEMCODE_CREDENTIAL_SOURCE:
// with subscriptions as family lanes there is no "the" source anymore — every
// listed source the user has consented to attaches as a lane. The old var
// still reads as a one-item list and is migrated (rewritten to this var, old
// one deleted) the next time `memcode auth` writes; consent semantics are
// unchanged — a subscription NEVER activates from ambient state alone.
const EnvCredentials = "MEMCODE_CREDENTIALS"

// canonicalSourceAliases maps every accepted spelling to the canonical source
// id. Canonical ids are what the wizard writes and what SourceVendor keys on.
var canonicalSourceAliases = map[string]string{
	"claude": "claude", "claude-sub": "claude", "anthropic-sub": "claude",
	"codex":   "codex",
	"copilot": "copilot",
	"grok":    "grok", "grok-sub": "grok", "xai-sub": "grok",
}

// sourceVendors maps a canonical source to the catalog vendor family its lane
// serves. Both codex and copilot are openai-family; when both are attached the
// FIRST listed wins (deliberate, pinned by tests).
var sourceVendors = map[string]string{
	"claude":  "anthropic",
	"codex":   "openai",
	"copilot": "openai",
	"grok":    "grok",
}

// SourceVendor returns the catalog vendor a canonical source serves, "" for
// unknown sources.
func SourceVendor(source string) string { return sourceVendors[source] }

// AttachedSources returns the ordered, canonicalized, deduplicated consent
// list. MEMCODE_CREDENTIALS wins; the legacy single-value var reads as a
// one-item list so pre-migration installs keep working. Unknown tokens are
// dropped (the boot warning surfaces them via SelectedSourcesUnresolved's
// resolution pass, not here).
func AttachedSources() []string {
	raw := os.Getenv(EnvCredentials)
	if strings.TrimSpace(raw) == "" {
		raw = os.Getenv(EnvCredentialSource)
	}
	var out []string
	seen := map[string]bool{}
	for _, tok := range strings.Split(raw, ",") {
		src := canonicalSourceAliases[strings.ToLower(strings.TrimSpace(tok))]
		if src == "" || seen[src] {
			continue
		}
		seen[src] = true
		out = append(out, src)
	}
	return out
}

// resolveSource runs a canonical source's resolver, returning its lane
// endpoint. ok=false when the underlying login is absent/expired.
func resolveSource(source string) (Endpoint, bool) {
	switch source {
	case "claude":
		return resolveClaudeSub()
	case "codex":
		return resolveCodex()
	case "copilot":
		return resolveCopilot()
	case "grok":
		return resolveGrok()
	}
	return Endpoint{}, false
}

// SelectedSourcesUnresolved lists attached sources whose login did NOT
// resolve (expired token, signed out of the host tool). Boot warns per
// source: serving silently missing a consented lane is the failure users
// cannot see.
func SelectedSourcesUnresolved() []string {
	var out []string
	for _, src := range AttachedSources() {
		if _, ok := resolveSource(src); !ok {
			out = append(out, src)
		}
	}
	return out
}
