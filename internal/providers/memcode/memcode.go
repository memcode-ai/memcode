// Package memcode is the memcode PROTOCOL's provider: the OpenAI-compat
// chat/completions dialect PLUS the memcode extensions — the two-system
// stable/volatile convention, the memcode_opaque reasoning round-trip, the
// `memcode` response object, the enforced `memcode_billing` lane, session
// affinity via `user`, and the GET /v1/models routing CONTROL PLANE the
// CLI-side selection policy runs on. It is a configuration of the shared
// generic engine (providers/compat) — same wire, extensions on — plus the
// control-plane client. One implementation, consumed by the CLI; the gateway
// SERVES this dialect (api/internal/compat + api/internal/server).
package memcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/providers/compat"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Config configures the memcode-dialect transport.
type Config struct {
	// BaseURL is the gateway ROOT (e.g. https://code.memcode.ai); the /v1
	// compat surface is mounted beneath it.
	BaseURL string
	// Token is the memcode_ org key.
	Token string
	// Compose renders legacy-shaped side calls (Mode stamped, doctrine not yet
	// composed) — the CALLER owns doctrine.
	Compose func(wire.Request) (wire.Request, error)
}

// Transport is the memcode-dialect turn transport. A named wrapper over the
// shared compat engine so this package's public surface is its OWN type —
// that the dialect rides the generic engine is an implementation detail,
// free to change without touching consumers.
type Transport struct {
	*compat.Transport
}

// New returns the memcode-dialect turn transport: the shared compat engine
// with the memcode extensions on, mounted at {base}/v1.
func New(cfg Config) *Transport {
	return &Transport{compat.New(compat.Config{
		BaseURL: strings.TrimRight(cfg.BaseURL, "/") + "/v1",
		Token:   cfg.Token,
		Memcode: true,
		Compose: cfg.Compose,
	})}
}

// ── the routing control plane (GET /v1/models) ──────────────────────────────

// RoleModel is DELETED. It named which model played each ROUTING role
// (planner/reviewer/standard/classify) — the deployment half of the Automatic
// ladder. Selection has one input now: the session's pin.

// ModelFact is one servable model with the control-plane facts selection
// reads. Label is the wire model id (the value sent in `model`).
type ModelFact struct {
	Label     string
	Name      string // friendly display name ("Sonnet 5")
	Desc      string // one-line picker description
	Group     string // display family — presentation only
	Vendor    string // authoritative serving vendor — the selection/steering identity
	Window    int
	Vision    bool
	PDF       bool
	Reasoning bool
	Pinnable  bool // offered in the /model picker (every listed label serves)
	Byok      bool // this model's vendor is covered by the user's own key
}

// PinnableModel is one /model picker entry — the pinnable subset of the
// servable list, in the shape the picker renders.
type PinnableModel struct {
	Label  string
	Name   string
	Desc   string
	Group  string
	Window int
	Vision bool
	Byok   bool
}

// ModelsInfo is the decoded control plane: everything CLI-side selection needs
// for one hosted org, from one GET /v1/models call.
type ModelsInfo struct {
	Backend          string      // gateway provider mode ("hybrid" in prod)
	Vendors          []string    // configured strong vendors; [0] is the deployment default
	Models           []ModelFact // every servable label, catalog order
	CreditsExhausted bool        // empty wallet → selection must prefer keyed lanes

	// SubVendors is CLIENT-STAMPED (never decoded from the wire): vendors
	// served by attached subscription lanes, i.e. $0 serving paths that
	// selection should prefer. Empty on every wire-decoded snapshot, so all
	// gateway-parity behavior is byte-identical when no subs are attached.
	SubVendors map[string]bool `json:"-"`
}

// Fact returns the servable entry for a label.
func (m ModelsInfo) Fact(label string) (ModelFact, bool) {
	for _, f := range m.Models {
		if f.Label == label {
			return f, true
		}
	}
	return ModelFact{}, false
}

// ByokVendorSet returns the vendors the user has keys for, derived from the
// per-model byok stamps (a vendor is keyed iff any of its models is).
func (m ModelsInfo) ByokVendorSet() map[string]bool {
	out := map[string]bool{}
	for _, f := range m.Models {
		if f.Byok && f.Vendor != "" {
			out[f.Vendor] = true
		}
	}
	return out
}

// DefaultVendor is the deployment's default strong vendor (vendors[0]),
// "openai" when the gateway didn't report one.
func (m ModelsInfo) DefaultVendor() string {
	if len(m.Vendors) > 0 && m.Vendors[0] != "" {
		return m.Vendors[0]
	}
	return "openai"
}

// FetchModels asks the gateway for the routing control plane, decoding the
// SHARED wire types (providers/compat ModelList — the same declarations the
// gateway serves with). Network-bound; pass a bounded context.
func FetchModels(ctx context.Context, baseURL, token string) (ModelsInfo, error) {
	if baseURL == "" || token == "" {
		return ModelsInfo{}, fmt.Errorf("gateway not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/models", nil)
	if err != nil {
		return ModelsInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return ModelsInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		// The stored token is dead (expired/revoked). Sentinel so boot paths
		// can flip to signed-out proactively.
		return ModelsInfo{}, wire.ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return ModelsInfo{}, fmt.Errorf("gateway returned %s", resp.Status)
	}
	var list compat.ModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return ModelsInfo{}, err
	}
	var info ModelsInfo
	for _, d := range list.Data {
		f := ModelFact{Label: d.ID}
		if m := d.Memcode; m != nil {
			f.Name, f.Desc, f.Group, f.Vendor = m.Name, m.Desc, m.Group, m.Vendor
			f.Window, f.Vision, f.PDF, f.Reasoning = m.Window, m.Vision, m.PDF, m.Reasoning
			f.Pinnable, f.Byok = m.Pinnable, m.Byok
		}
		info.Models = append(info.Models, f)
	}
	if x := list.Memcode; x != nil {
		info.Backend, info.Vendors, info.CreditsExhausted = x.Backend, x.Vendors, x.CreditsExhausted
	}
	return info, nil
}
