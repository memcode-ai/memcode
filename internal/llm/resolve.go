package llm

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// resolve.go — PHYSICAL RESOLUTION: lane → concrete model label, decided over
// the hosted routing control plane (GET /v1/models: roles, byok coverage,
// credits state, capabilities) plus the shared catalog (vendor tier triples,
// windows). Steering — prefer vendors the user brought keys for, never select
// an unfundable lane at $0 — is SELECTION policy here, moved from the gateway
// (steer.go, deleted) and proven against parity goldens
// (testdata/steer_goldens.json). The gateway can no longer reroute anything:
// what this file picks is what serves, or a typed error comes back.

// modelsTTL bounds how stale the control-plane snapshot may get before a
// refresh; invalidation (login, /apikeys, 402s) cuts it short.
const modelsTTL = 5 * time.Minute

// selection owns the control-plane snapshot and the resolution policy. One per
// Runner tree (forks share it, like the ledger).
type selection struct {
	mu      sync.Mutex
	info    provider.ModelsInfo
	at      time.Time
	haveNet bool // true when info came from the gateway (vs the catalog fallback)

	// fetch is the control-plane call, swappable in tests.
	fetch func(ctx context.Context) (provider.ModelsInfo, error)
}

func newSelection() *selection {
	return &selection{fetch: provider.FetchModels}
}

// Invalidate drops the snapshot so the next call refetches — hooked to /login,
// /apikeys mutations, and 402-class errors (credits state just changed).
func (s *selection) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at = time.Time{}
}

// models returns the current control-plane snapshot, refreshing over the
// network when stale. A failed fetch degrades to the last snapshot, or to a
// catalog-derived default (labels + capabilities only — no byok/credits
// facts), so a gateway hiccup never blocks selection: the gateway remains the
// enforcement backstop for anything the degraded snapshot got wrong.
func (s *selection) models(ctx context.Context) provider.ModelsInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.at.IsZero() && time.Since(s.at) < modelsTTL {
		return s.info
	}
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := s.fetch(fctx)
	if err == nil && len(info.Models) > 0 {
		s.info, s.at, s.haveNet = info, time.Now(), true
		return info
	}
	if s.haveNet { // keep serving the stale-but-real snapshot
		s.at = time.Now() // don't hammer a down gateway every call
		return s.info
	}
	s.info, s.at = catalogInfo(), time.Now()
	return s.info
}

// catalogInfo builds the degraded control-plane snapshot from the embedded
// catalog: every chat-capable label, no byok/credits/role facts. Selection
// still works (tier fallbacks resolve); the gateway enforces the rest.
func catalogInfo() provider.ModelsInfo {
	var info provider.ModelsInfo
	for _, m := range catalog.CatalogModels() {
		if m.Window <= 0 {
			continue
		}
		info.Models = append(info.Models, provider.ModelFact{
			Label: m.Label, Name: m.Name, Desc: m.Desc, Group: m.Group, Vendor: m.Vendor,
			Window: m.Window, Vision: m.Vision, PDF: m.PDF, Reasoning: m.Reasoning, Pinnable: m.Pinnable,
		})
	}
	return info
}

// resolved is one selection verdict.
type resolved struct {
	label  string // the concrete model label to request ("" only on err)
	pinned bool   // the user's explicit /model choice served as-is
	reason string // client-side absorb reason ("vision" | "document" | "context_overflow") for the ⇄ line
	err    error  // turn-fatal capability refusal (no capable lane reachable)
}

// resolveHosted maps an Intent (+ the request payload, for capability
// pre-checks) to a concrete label over the control plane. The straight port of
// the gateway's ResolveModelPinned → SteerResolvedModel → route-absorb chain,
// now one visible client-side decision.
func resolveHosted(it wire.Intent, req wire.Request, info provider.ModelsInfo) resolved {
	purpose := strings.TrimSpace(strings.ToLower(it.Purpose))

	// The pin path: a valid pinnable label serves every REAL purpose; utility
	// purposes and stale/unknown labels fall through to Automatic, so a stale
	// config never breaks a session.
	if it.Pin != "" && !utilityPurposes[purpose] {
		if f, ok := info.Fact(it.Pin); ok && f.Pinnable {
			return capabilityAdjust(it.Pin, true, it, req, info)
		}
		if _, ok := info.Fact(it.Pin); !ok {
			// Not in the servable list — maybe unknown, maybe a vendor outage.
			// Automatic takes the turn (the old fall-through behavior).
			it.Pin = ""
		}
	}

	// The semantic ladder, resolved over the deployment role config with the
	// session vendor's tier triple as the fallback.
	ln := laneFor(it)
	label := ""
	for _, role := range ln.roles {
		if l := info.Role(role); l != "" {
			label = l
			break
		}
	}
	if label == "" {
		label = catalog.VendorTier(sessionVendor(it.Vendor, info), ln.tier)
	}

	// Keyed-vendor steering (BYOK-first, ported from steer.go): an AUTOMATIC
	// turn resolved to a strong vendor the user has NO key for remaps to the
	// keyed preference's member at the SAME altitude. Explicit vendor choices
	// are never overridden; with no keys this is a byte-identical no-op.
	label = steerLabelWithLanes(label, it.Vendor, purpose, info)

	return capabilityAdjust(label, false, it, req, info)
}

// sessionVendor resolves the tier-fallback vendor: the session's explicit
// choice, else the deployment default. The persisted "openai" default counts
// as Automatic (it IS the default) — that also un-breaks steering, which the
// old always-stamped vendor silently disabled for every CLI session.
func sessionVendor(v string, info provider.ModelsInfo) string {
	if v != "" && v != info.DefaultVendor() {
		return v
	}
	return info.DefaultVendor()
}

// explicitVendor reports whether the session vendor blocks steering: any
// non-default choice is explicit intent.
func explicitVendor(v string, info provider.ModelsInfo) bool {
	return v != "" && v != info.DefaultVendor()
}

// byokVendorOrder returns every catalog vendor in catalog order (fireworks
// appended last if unseen) — the steering preference order, identical to the
// gateway's ByokVendors derivation.
func byokVendorOrder() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range catalog.CatalogModels() {
		v := m.Vendor
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

// keyedPreference returns the vendor a steered turn should serve on: the first
// keyed, tier-servable vendor in the configured order — the deployment default
// first, then catalog order. "" when the user has no servable key.
func keyedPreference(info provider.ModelsInfo) string {
	keyed := info.ByokVendorSet()
	if len(keyed) == 0 {
		return ""
	}
	for _, v := range append([]string{info.DefaultVendor()}, byokVendorOrder()...) {
		if keyed[v] && catalog.VendorTier(v, "balanced") != "" {
			return v
		}
	}
	return ""
}

// steerLabel is the port of SteerResolvedModel: remap an automatic selection
// on an unkeyed strong vendor to the keyed preference at the same altitude.
// Fireworks-owned labels pass through (the cheap lane serves BYOK-first on its
// own; the $0 case is handled by fundability in capabilityAdjust).
func steerLabel(label, vendor string, info provider.ModelsInfo) string {
	if explicitVendor(vendor, info) {
		return label
	}
	keyed := info.ByokVendorSet()
	if len(keyed) == 0 {
		return label
	}
	owner := catalog.ModelVendor(label)
	if owner == "" || owner == "fireworks" || keyed[owner] {
		return label
	}
	pref := keyedPreference(info)
	if pref == "" || pref == owner {
		return label
	}
	alt := catalog.TierAltitude(label)
	if alt == "" {
		return label // outside the owning triple (shouldn't happen unpinned) — leave it
	}
	return tierMember(pref, alt, false, 0)
}

// steerVendor names the vendor an ABSORB (vision/document/overflow/$0) should
// target: the session vendor when keyed (or no keys at all), else the keyed
// preference — the client-side equivalent of the gateway's steerTier.
func steerVendor(vendorChoice string, info provider.ModelsInfo) string {
	sv := sessionVendor(vendorChoice, info)
	if explicitVendor(vendorChoice, info) {
		return sv
	}
	keyed := info.ByokVendorSet()
	if len(keyed) == 0 || keyed[sv] {
		return sv
	}
	if pref := keyedPreference(info); pref != "" {
		return pref
	}
	return sv
}

// tierMember returns vendor's tier member, with the fireworks capability
// fixups the synthetic tier had (an image turn moves to the first member with
// vision, an oversized turn to the first member whose window fits).
func tierMember(vendor, alt string, needVision bool, estimate int) string {
	label := catalog.VendorTier(vendor, alt)
	if label == "" {
		return ""
	}
	members := []string{catalog.VendorTier(vendor, "frontier"), catalog.VendorTier(vendor, "balanced"), catalog.VendorTier(vendor, "cheap")}
	if needVision {
		if m, ok := catalog.LookupModel(label); ok && !m.Vision {
			for _, cand := range members {
				if cm, ok := catalog.LookupModel(cand); ok && cm.Vision {
					label = cand
					break
				}
			}
		}
	}
	if estimate > 0 {
		if m, ok := catalog.LookupModel(label); ok && m.Window > 0 && estimate > m.Window {
			for _, cand := range members {
				if cm, ok := catalog.LookupModel(cand); ok && cm.Window >= estimate {
					label = cand
					break
				}
			}
		}
	}
	return label
}

// capabilityAdjust runs the pre-flight capability checks the gateway used to
// absorb server-side: an image on a no-vision model, a document on a model
// without native PDF input, a prompt past the model's window. The remap is now
// a VISIBLE client decision (the reason feeds the ⇄ line); the gateway's typed
// errors remain the enforcement backstop.
func capabilityAdjust(label string, pinned bool, it wire.Intent, req wire.Request, info provider.ModelsInfo) resolved {
	fact := func(l string) provider.ModelFact {
		if f, ok := info.Fact(l); ok {
			return f
		}
		if m, ok := catalog.LookupModel(l); ok {
			return provider.ModelFact{Label: m.Label, Vendor: m.Vendor, Window: m.Window, Vision: m.Vision, PDF: m.PDF}
		}
		return provider.ModelFact{Label: l}
	}
	out := resolved{label: label, pinned: pinned}
	f := fact(label)
	estimate := estimateTokens(req)
	sv := steerVendor(it.Vendor, info)

	if hasBlock(req, "image") && !f.Vision {
		out.label = tierMember(sv, "balanced", true, 0)
		out.reason = "vision"
	}
	if hasBlock(req, "document") && !fact(out.label).PDF {
		target, err := documentVendor(sv, it.Vendor, info)
		if err != nil {
			return resolved{err: err}
		}
		out.label = catalog.VendorTier(target, "balanced")
		out.reason = "document"
	}
	if w := fact(out.label).Window; w > 0 && estimate > w {
		out.label = tierMember(sv, "balanced", hasBlock(req, "image"), estimate)
		if out.reason == "" {
			out.reason = "context_overflow"
		}
	}
	// The $0 fundability rule (mustSteerAtZero): an AUTOMATIC selection must
	// never target an unfunded lane — with an empty wallet and BYOK keys, an
	// unkeyed candidate remaps to the keyed preference's balanced member (the
	// old credits_byok absorb, now up-front). Pins are exempt: an unkeyed pin
	// at $0 gets the gateway's clean 402 naming the vendor, never a coercion.
	if !pinned && info.CreditsExhausted {
		keyed := info.ByokVendorSet()
		funded := len(keyed) > 0 || len(info.SubVendors) > 0
		if funded && !fundedVendor(fact(out.label).Vendor, keyed, info) && !explicitVendor(it.Vendor, info) {
			// Sub lanes are the cheapest funded path ($0 and no key metering).
			pref := subPreference(info)
			reason := "credits_sub"
			if pref == "" {
				pref, reason = keyedPreference(info), "credits_byok"
			}
			if pref != "" && pref != fact(out.label).Vendor {
				out.label = tierMember(pref, "balanced", hasBlock(req, "image"), estimate)
				if out.reason == "" {
					out.reason = reason
				}
			}
		}
	}
	if out.label == "" {
		out.label = label // defensive: never return empty without err
	}
	return out
}

// documentVendor picks the vendor whose balanced tier takes a PDF natively,
// keyed-aware — the port of the gateway's documentTier. Turn-fatal when
// nothing PDF-capable is reachable (fireworks-only BYOK at $0).
func documentVendor(sv, vendorChoice string, info provider.ModelsInfo) (string, error) {
	pdfBalanced := func(v string) bool {
		lbl := catalog.VendorTier(v, "balanced")
		m, ok := catalog.LookupModel(lbl)
		return ok && m.PDF
	}
	keyed := info.ByokVendorSet()
	if len(keyed) == 0 {
		if pdfBalanced(sv) {
			return sv, nil
		}
		return info.DefaultVendor(), nil
	}
	if pdfBalanced(sv) && (keyed[sv] || !info.CreditsExhausted) {
		return sv, nil
	}
	for _, v := range append([]string{info.DefaultVendor()}, byokVendorOrder()...) {
		if v == "fireworks" || !keyed[v] || catalog.VendorTier(v, "balanced") == "" {
			continue
		}
		if pdfBalanced(v) {
			return v, nil
		}
	}
	if !info.CreditsExhausted {
		return info.DefaultVendor(), nil
	}
	var vendors []string
	for _, v := range byokVendorOrder() {
		if v != "fireworks" && pdfBalanced(v) {
			vendors = append(vendors, v)
		}
	}
	return "", fmt.Errorf("this turn needs PDF support, which your API keys don't cover — add credits at memcode.ai/account/billing or add a %s key with /apikeys",
		strings.Join(vendors, "/"))
}

// hasBlock reports whether any message block (top-level or inside a
// tool_result's ContentBlocks) has the given type — the same check the
// gateway's capability gates run.
func hasBlock(req wire.Request, t string) bool {
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type == t {
				return true
			}
			for _, cb := range b.ContentBlocks {
				if cb.Type == t {
					return true
				}
			}
		}
	}
	return false
}

// estimateTokens is the pre-flight size estimate (~4 chars/token with a 1.25
// safety margin — the gateway's exact formula, kept so the client-side
// overflow pre-check fires where the server-side one did).
func estimateTokens(req wire.Request) int {
	n := len(req.System) + len(req.SystemVolatile)
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			n += len(b.Text) + len(b.Content) + len(b.Input) + len(b.Thinking)
		}
	}
	return n / 4 * 125 / 100
}
