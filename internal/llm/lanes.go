package llm

import (
	"strings"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Lane FACTS, not lane policy. Attached subscriptions and own-key vendors are
// $0 serving paths, so this file folds them into the control-plane snapshot:
// which vendors are servable, which are keyed, and what the default is when
// there is no gateway.
//
// It does NOT choose a model. The steering that once lived alongside this
// (Automatic preferring keyed vendors, the $0 fundability remap) went with the
// routing removal in v0.29.0 — the pin is the only selection authority now, and
// a lane can make a model reachable or unreachable but never preferred.

// applyLaneFacts stamps the control-plane snapshot with the local lane
// reality: sub vendors join SubVendors; own-key vendors merge into the BYOK
// keyed set (same rank as gateway-held keys); without a gateway the servable
// set clamps to lane vendors and the first lane's vendor becomes the default.
func applyLaneFacts(info provider.ModelsInfo, lanes []provider.LaneInfo, gateway bool) provider.ModelsInfo {
	if len(lanes) == 0 {
		return info
	}
	out := info
	out.Models = append([]provider.ModelFact(nil), info.Models...)
	subs := map[string]bool{}
	ownkey := map[string]bool{}
	var order []string
	for _, ln := range lanes {
		if ln.Vendor == "" {
			continue
		}
		order = append(order, ln.Vendor)
		if ln.Kind == "sub" {
			subs[ln.Vendor] = true
		} else {
			ownkey[ln.Vendor] = true
		}
	}
	out.SubVendors = subs
	for i := range out.Models {
		if ownkey[out.Models[i].Vendor] {
			out.Models[i].Byok = true
		}
	}
	if !gateway {
		var clamped []provider.ModelFact
		laneVendor := map[string]bool{}
		for _, v := range order {
			laneVendor[v] = true
		}
		for _, f := range out.Models {
			if laneVendor[f.Vendor] {
				clamped = append(clamped, f)
			}
		}
		out.Models = clamped
		out.Vendors = order
	}
	return out
}

// NOTE: subPreference / steerLabelWithLanes lived here and are DELETED.
// They remapped an AUTOMATIC selection onto a vendor the user had a
// subscription or BYOK key for — selection policy, and therefore routing.
// With one pinned model there is nothing to steer: the user picked the vendor
// when they picked the model. Credential lanes below still decide WHICH
// credential pays for that vendor, which is a different question and stays.

// fundedVendor reports a vendor with a no-credit serving path: a gateway BYOK
// key, a local key lane (merged into Byok above), or a subscription lane.
func fundedVendor(v string, keyed map[string]bool, info provider.ModelsInfo) bool {
	return keyed[v] || info.SubVendors[v]
}

// scrubForeignThinking drops thinking/redacted_thinking blocks FOREIGN to the
// serving vendor: per-turn family switching (a lane turn after a gateway
// Claude turn, a mid-call fallback hop) can otherwise replay one vendor's
// reasoning into another vendor's request — a hard 400. Per-block and
// ID-aware, matching the adapters' own wire hygiene: OpenAI reasoning items
// carry "rs_" ids and survive an openai serve; Anthropic thinking blocks
// (no "rs_" prefix) survive an anthropic serve; every other vendor has no
// reasoning round-trip, so all thinking is dropped there. Copy-on-write:
// shared history slices are never mutated.
func scrubForeignThinking(req *wire.Request, servingLabel string) {
	vendor := catalog.ModelVendor(servingLabel)
	drop := func(b wire.Block) bool {
		if b.Type != "thinking" && b.Type != "redacted_thinking" {
			return false
		}
		openaiNative := strings.HasPrefix(b.ID, "rs_")
		switch vendor {
		case "anthropic":
			return openaiNative
		case "openai":
			return !openaiNative
		default:
			return true
		}
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != "assistant" {
			continue
		}
		dirty := false
		for _, b := range m.Blocks {
			if drop(b) {
				dirty = true
				break
			}
		}
		if !dirty {
			continue
		}
		kept := make([]wire.Block, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			if drop(b) {
				continue
			}
			kept = append(kept, b)
		}
		m.Blocks = kept
	}
}
