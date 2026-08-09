package provider

import (
	"os"
	"strings"
	"sync"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/gateway/internal/identity"
	"github.com/memcode-ai/memcode/internal/wire"
)

// resolve.go — the gateway's MODEL-SERVING surface. Since the
// all-policy-client-side migration (2026-08-08) there is NO routing here: the
// CLI is the agent and decides which concrete model every call asks for; the
// gateway serves exactly the requested catalog label or returns a typed error.
// What remains in this file is the serving vocabulary: the role config the
// /v1/models control plane reports (so the CLI's ladder knows which model
// plays planner/reviewer/standard/classify), the label⇄raw-id mapping (raw
// provider ids never leave the server), and the deployment's configured-vendor
// facts. The semantic ladder, steering, and escalation live in
// cli/internal/llm (ported against parity goldens generated from the old code
// here before its deletion).

// Cheap-tier model ids (Fireworks), configured by NewFromEnv after the env is loaded. Empty
// → fall back to the OpenAI tier (so anthropic/fireworks modes and a misconfig still work).
var (
	modelPlanner  string // config role planner  — the plan executive (glm). "" → Sol.
	modelReviewer string // config role reviewer — the cross-model plan critic (Luna). "" → Sol.
	modelStandard string // config role standard — the high-volume CODING lane (glm). "" → the OpenAI tier.
	modelClassify string // config role classify — the cheap structured-output classifier lane (gpt-oss). "" → standard → Luna.

	// configMu guards the four role vars above. NewFromEnv / SetModels write them at
	// startup and on env reload, while request-serving goroutines read them via the
	// *Or accessors. The RWMutex gives lock-free-ish reads on the steady-state path.
	configMu sync.RWMutex
)

// SetModels pins the configured role ids for the /v1/models role report. Called by
// NewFromEnv, so the role story is single-sourced here. classify is variadic
// for back-compat: a single-model backend (anthropic/fireworks) passes only the first
// three and the classifier collapses onto the standard lane.
func SetModels(planner, reviewer, standard string, classify ...string) {
	configMu.Lock()
	defer configMu.Unlock()
	modelPlanner, modelReviewer, modelStandard = planner, reviewer, standard
	modelClassify = ""
	if len(classify) > 0 {
		modelClassify = classify[0]
	}
}

func plannerOr(fb string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return orElse(modelPlanner, fb)
}
func reviewerOr(fb string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return orElse(modelReviewer, fb)
}
func standardOr(fb string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return orElse(modelStandard, fb)
}
func classifyOr(fb string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return orElse(modelClassify, fb)
}
func orElse(v, fb string) string {
	if v != "" {
		return v
	}
	return fb
}

// RoleModel is one configured role for the /v1/models surface: which model plays which job,
// with the catalog facts the client can show.
type RoleModel struct {
	Role   string `json:"role"`             // planner | reviewer | standard | classify
	ID     string `json:"id"`               // sanitized label — the raw provider id never leaves the server
	Label  string `json:"label"`            // short human name (catalog label)
	Window int    `json:"window,omitempty"` // context window (tokens), from models.json
	Vision bool   `json:"vision,omitempty"` // reads images natively
}

// ConfiguredModels reports the model this deployment plays for each role — the
// CLI's semantic ladder maps role lanes onto these labels. Falls back to the
// OpenAI tier labels when a role is unset (single-vendor modes).
func ConfiguredModels() []RoleModel {
	rs := []struct{ role, id string }{
		{"planner", plannerOr(catalog.ModelSol)},
		{"reviewer", reviewerOr(catalog.ModelSol)},
		{"standard", standardOr(catalog.ModelTerra)},
		{"classify", classifyOr(standardOr(catalog.ModelLuna))},
	}
	out := make([]RoleModel, 0, len(rs))
	for _, r := range rs {
		spec := reg.spec(r.id)
		clean := SanitizeModelID(r.id) // never expose the provider path to the client
		out = append(out, RoleModel{Role: r.role, ID: clean, Label: clean, Window: spec.Window, Vision: spec.Vision})
	}
	return out
}

// ConfiguredVendors reports which strong-tier vendors the gateway has keys for
// (and can therefore serve), reported on /v1/models. The FIRST entry is the
// deployment's default vendor — the CLI's selection policy uses it as the
// unkeyed-session tier preference.
func ConfiguredVendors() []string {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv(EnvProvider)))
	switch backend {
	case "gemini":
		return []string{"gemini"}
	case "grok":
		return []string{"grok"}
	case "openai":
		return []string{"openai"}
	case "anthropic", "":
		return []string{"anthropic"}
	case "fireworks":
		return nil // cheap-lane-only modes have no strong-tier vendor
	case "hybrid":
		// Report vendors whose keys are present, default (openai) first.
		var out []string
		if os.Getenv(EnvOpenAIKey) != "" {
			out = append(out, "openai")
		}
		if os.Getenv(EnvAPIKey) != "" {
			out = append(out, "anthropic")
		}
		if HasGeminiCreds() { // real key only — not the deploy "placeholder" seed
			out = append(out, "gemini")
		}
		if os.Getenv(EnvGrokKey) != "" {
			out = append(out, "grok")
		}
		if len(out) == 0 {
			out = []string{"openai"} // default fallback
		}
		return out
	}
	return nil
}

// ByokVendors enumerates every vendor a user could bring their own key for —
// derived from the models.json catalog (via owningVendor), never hardcoded, so
// a new catalog vendor shows up in /apikeys and the www API Keys page without
// code changes. Fireworks (the cheap lane) is always included. Order is stable
// (catalog order, fireworks last unless the catalog surfaced it already).
func ByokVendors() []string {
	seen := map[string]bool{}
	var out []string
	for _, spec := range reg.ordered {
		v := owningVendor(spec.ID)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if !seen["fireworks"] {
		out = append(out, "fireworks")
	}
	return out
}

// SanitizeModelID returns the client-facing short name for a model id: its declared catalog
// label (models.json) if the model is known — e.g. "claude-haiku-4-5-20251001" → "haiku",
// "accounts/fireworks/models/glm-5p1" → "glm-5p1" — else its last path segment as a defensive
// fallback. Either way the inference vendor's path never reaches the client. The gateway keeps
// the RAW id internally (cost, metering, catalog lookup); this is only for the wire.
func SanitizeModelID(id string) string {
	if lbl := reg.spec(id).Label; lbl != "" {
		return lbl
	}
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// SanitizeResponse strips provider identity from a response just before it is
// sent to the client: model ids lose their vendor paths, and the cheap lane's
// internal backend tag ("fireworks" — honest inside the gateway) becomes the
// vendor-neutral wire value "cheap". Call it AFTER metering (which needs the
// raw ids/backends) and BEFORE writing.
func SanitizeResponse(resp *wire.Response) {
	resp.Model = SanitizeModelID(resp.Model)
	resp.RequestedModel = SanitizeModelID(resp.RequestedModel)
	if resp.Backend == "fireworks" {
		resp.Backend = "cheap"
	}
	// Response.BYOK RIDES the wire (2026-08-07): whether the turn served on the
	// user's own key is the key owner's own information — the CLI footer shows
	// it per-turn. BYOKVendor stays server-internal (zeroed defensively; the
	// cheap lane's inference vendor never leaves the server).
	resp.BYOKVendor = ""
}

// LookupServable is the compat endpoint's STRICT model gate: it resolves a
// client-facing catalog label ("sonnet", "glm-5p2", "gpt-oss-120b") to its raw
// model id — ok only for known CHAT labels (Window > 0; embedding/image rows
// don't serve chat) whose owning backend has credentials in this deployment.
// Anything else — "auto", vendor names, typos, retired labels — must 400
// (unknown_model): the gateway serves exactly what the agent asked for, and a
// typo must never silently reroute a session.
func LookupServable(label string) (string, bool) {
	spec, ok := reg.specByLabel(strings.TrimSpace(label))
	if !ok || spec.Window <= 0 || !backendConfigured(spec.ID) {
		return "", false
	}
	return spec.ID, true
}

// ServableModel is one /v1/models entry: a model this deployment can actually
// serve, with the control-plane facts the CLI's selection policy reads.
type ServableModel struct {
	Label     string
	Name      string
	Desc      string
	Group     string // display family — presentation only
	Vendor    string // authoritative serving vendor — the selection identity
	Window    int
	Vision    bool
	PDF       bool
	Reasoning bool
	Pinnable  bool // picker fact; serving accepts every listed label
	Byok      bool // the requesting user brought their own key for this model's vendor
}

// ServableModelsFor reports every model /v1/models lists for one user: all
// chat-capable catalog rows (Window > 0) whose serving backend has
// credentials, in models.json order, Byok stamped from the user's presence
// gate. This is the hosted routing control plane — the dataset CLI-side
// selection runs on.
func ServableModelsFor(who identity.Info) []ServableModel {
	var out []ServableModel
	for _, spec := range reg.ordered {
		if spec.Window <= 0 || !backendConfigured(spec.ID) {
			continue
		}
		out = append(out, ServableModel{
			Label: spec.Label, Name: spec.Name, Desc: spec.Desc, Group: spec.Group,
			Vendor: catalog.ModelVendor(spec.ID), Window: spec.Window,
			Vision: spec.Vision, PDF: spec.PDF, Reasoning: spec.Reasoning,
			Pinnable: spec.Pinnable, Byok: who.HasKey(owningVendor(spec.ID)),
		})
	}
	return out
}

// backendConfigured reports whether the backend that owns a model id has usable
// credentials in the current deployment mode — the same env facts ConfiguredVendors
// reads. In single-vendor MEMCODE_PROVIDER modes only that vendor's models qualify.
func backendConfigured(id string) bool {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv(EnvProvider)))
	owner := owningVendor(id)
	switch backend {
	case "hybrid", "":
		switch owner {
		case "fireworks":
			return envFirst(EnvFireworksURL, envLegacyVLLMURL) != ""
		case "openai":
			return os.Getenv(EnvOpenAIKey) != ""
		case "anthropic":
			return os.Getenv(EnvAPIKey) != ""
		case "gemini":
			return HasGeminiCreds()
		case "grok":
			return os.Getenv(EnvGrokKey) != ""
		}
		return false
	default:
		return backend == owner // pure single-vendor mode: only its own models
	}
}

// owningVendor names the backend a model id belongs to. The shared catalog's
// vendor field is authoritative; the prefix rules remain as a defensive
// fallback for ids outside the catalog (they should not exist).
func owningVendor(id string) string {
	if v := catalog.ModelVendor(id); v != "" {
		return v
	}
	switch {
	case strings.HasPrefix(id, "accounts/"):
		return "fireworks"
	case strings.HasPrefix(id, "gpt-"):
		return "openai"
	case strings.HasPrefix(id, "claude-"):
		return "anthropic"
	case strings.HasPrefix(id, "gemini-"):
		return "gemini"
	case strings.HasPrefix(id, "grok-"):
		return "grok"
	}
	return ""
}
