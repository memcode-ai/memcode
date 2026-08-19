package provider

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/catalog"
)

// Subscriptions and exported API keys are FAMILY LANES, not modes: each
// attached credential serves exactly its vendor's models, per-turn, while the
// gateway (when logged in) serves everything else. A lane is one conn dialed
// through the existing dialEndpoint machinery — its identity headers and
// transport are baked per lane, never shared.

type laneKind uint8

const (
	laneSub    laneKind = iota // wizard-consented subscription (memcode auth …)
	laneOwnKey                 // ambient exported API key (ANTHROPIC_API_KEY, …)
)

func (k laneKind) String() string {
	if k == laneOwnKey {
		return "ownkey"
	}
	return "sub"
}

type lane struct {
	vendor string // catalog vendor family this lane serves
	kind   laneKind
	ep     Endpoint
	c      *conn
}

// LaneInfo is the UI/ledger surface of an attached lane.
type LaneInfo struct {
	Vendor string // "anthropic" | "openai" | "grok"
	Name   string // endpoint name: "claude-sub", "codex", "ownkey:anthropic", …
	Kind   string // "sub" | "ownkey"
	Model  string // the lane's default model id
}

func (l lane) info() LaneInfo {
	return LaneInfo{Vendor: l.vendor, Name: l.backendName(), Kind: l.kind.String(), Model: l.ep.Model}
}

// backendName is the per-turn provenance stamp: sub lanes keep their endpoint
// names (claude-sub/codex/copilot/grok-sub — ServingLabel and the $0 cost
// display already key on them); own-key lanes are "ownkey:<vendor>".
func (l lane) backendName() string {
	if l.kind == laneOwnKey {
		return "ownkey:" + l.vendor
	}
	return l.ep.Name
}

// buildLanes constructs the lane set at boot: consented subscription sources
// in list order (first lane per vendor wins — codex before copilot is a
// deliberate, test-pinned choice), then own-key lanes for vendors no sub
// covers (sub > key, per-vendor).
func buildLanes() []lane {
	var lanes []lane
	haveVendor := map[string]bool{}
	for _, src := range AttachedSources() {
		vendor := SourceVendor(src)
		if vendor == "" || haveVendor[vendor] {
			continue
		}
		ep, ok := resolveSourceFn(src)
		if !ok {
			continue // boot warns via SelectedSourcesUnresolved
		}
		haveVendor[vendor] = true
		lanes = append(lanes, lane{vendor: vendor, kind: laneSub, ep: ep, c: dialLane(ep)})
	}
	for _, v := range ownKeyVendors {
		if haveVendor[v.name] {
			continue
		}
		if key := strings.TrimSpace(os.Getenv(v.env)); key != "" {
			ep := Endpoint{Name: v.name, BaseURL: v.baseURL, Key: key, Model: sourceModel(v.defModel)}
			haveVendor[v.name] = true
			lanes = append(lanes, lane{vendor: v.name, kind: laneOwnKey, ep: ep, c: dialLane(ep)})
		}
	}
	return lanes
}

// resolveSourceFn is a test seam over resolveSource (real resolvers read
// other tools' credential stores).
var resolveSourceFn = resolveSource

// dialLane is a test seam over dialEndpoint (native adapters pin real hosts).
var dialLane = func(ep Endpoint) *conn { return dialEndpoint(ep) }

// laneWireModel translates the routed pin into the id the vendor's API
// accepts: catalog labels ("opus") become raw ids ("claude-opus-5"); unknown
// pins pass through verbatim (an off-catalog copilot roster model). The
// gateway path never calls this — it keeps catalog labels.
func laneWireModel(pin string) string {
	if m, ok := catalog.LookupModel(pin); ok {
		return m.ID
	}
	return pin
}

// ErrNoLane: the turn's model belongs to a vendor with no serving path — no
// gateway login and no lane for that family. Terminal for the fallback walk;
// the message carries the remedies.
type ErrNoLane struct {
	Model    string
	Vendor   string
	Attached []LaneInfo
}

func (e *ErrNoLane) Error() string {
	var have []string
	for _, l := range e.Attached {
		if l.Kind == "ownkey" {
			have = append(have, "your "+l.Vendor+" key")
		} else {
			have = append(have, ServingLabel(l.Name)+" subscription")
		}
	}
	attached := "none"
	if len(have) > 0 {
		attached = strings.Join(have, ", ")
	}
	vendor := e.Vendor
	if vendor == "" {
		vendor = "an unknown provider"
	}
	return fmt.Sprintf("%s is served by %s — no matching credential attached (attached: %s). Run `memcode login` for full routing, or /model to pick an attached family",
		e.Model, vendor, attached)
}

// ErrLaneExhausted: a lane's credential hit its quota/rate window (HTTP
// 429/402 after the adapter's own retries). The provider NEVER reroutes on
// its own — the runtime raises the fallback-choice card and, on consent,
// reissues via CompleteOnGateway/StreamOnGateway.
type ErrLaneExhausted struct {
	Lane        LaneInfo
	Status      int
	ResetAt     time.Time // zero when unknown
	CanFallback bool      // a gateway base exists for a consented reissue
	Err         error
}

func (e *ErrLaneExhausted) Error() string {
	msg := fmt.Sprintf("your %s %s hit its usage limit", ServingLabel(e.Lane.Name), credentialNoun(e.Lane.Kind))
	if !e.ResetAt.IsZero() {
		msg += " (resets ~" + e.ResetAt.Format("15:04") + ")"
	}
	return msg
}

func (e *ErrLaneExhausted) Unwrap() error { return e.Err }

func credentialNoun(kind string) string {
	if kind == "ownkey" {
		return "API key"
	}
	return "subscription"
}

// LaneBackendVendor maps a per-turn Backend stamp to its lane identity:
// ("anthropic","sub",true) for "claude-sub", ("openai","ownkey",true) for
// "ownkey:openai", ok=false for gateway/exclusive stamps.
func LaneBackendVendor(backend string) (vendor, kind string, ok bool) {
	if strings.HasPrefix(backend, "ownkey:") {
		return strings.TrimPrefix(backend, "ownkey:"), "ownkey", true
	}
	if SubscriptionEndpointName(backend) {
		return SourceVendor(canonicalSourceAliases[ServingLabel(backend)]), "sub", true
	}
	return "", "", false
}

// BackendServingLabel renders the per-turn "via X" for a lane-stamped
// backend, "" for gateway/exclusive stamps (callers fall back to "memcode"
// or the exclusive endpoint's name).
func BackendServingLabel(backend string) string {
	v, kind, ok := LaneBackendVendor(backend)
	if !ok {
		return ""
	}
	if kind == "ownkey" {
		return "your " + v + " key"
	}
	return ServingLabel(backend)
}
