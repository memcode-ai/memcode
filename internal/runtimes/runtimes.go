// Package runtimes models where an autonomous task's inference actually runs.
//
// Five things are kept deliberately separate, because collapsing them is how a
// credential someone happens to have becomes authority a daemon quietly spends:
//
//	AVAILABILITY   a runtime exists on this machine
//	AUTHORIZATION  the user has explicitly allowed tasks to use it
//	CAPABILITY     it can actually serve what this task needs
//	SELECTION      which authorized, capable runtime to use
//	FALLBACK       what may be tried when the RUNTIME itself fails
//
// Availability never implies authorization. A Claude Code or Codex login lives
// in another tool's files; the user signed into THAT tool, not into a scheduler
// that will spend their quota at 3am unattended. Discovering one may produce an
// offer, never an entitlement — the same line internal/provider/credsource.go
// draws for interactive sessions, held here for unattended ones.
package runtimes

import (
	"sort"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/subscription/claudesub"
	"github.com/memcode-ai/memcode/internal/subscription/codex"
	"github.com/memcode-ai/memcode/internal/subscription/copilot"
	"github.com/memcode-ai/memcode/internal/subscription/grok"
)

// Kind says what sort of thing a runtime is, which is mostly a statement about
// who pays and how.
type Kind string

const (
	// KindSubscription is a login the user already holds in another tool. Running
	// a task on one costs nothing beyond the subscription they already bought,
	// which is the entire reason to prefer it — and the entire reason it needs
	// explicit permission.
	KindSubscription Kind = "subscription"
	// KindHosted is memcode's own gateway: always available, metered per token.
	KindHosted Kind = "hosted"
)

// Runtime is an execution backend a task can run on.
type Runtime struct {
	ID      string
	Display string
	Kind    Kind
	// CredentialSource is the value MEMCODE_CREDENTIAL_SOURCE takes to select
	// this backend, empty for the hosted gateway.
	CredentialSource string
	// Vendor is whose models it serves, for the capability check. Empty means
	// "any" — the hosted gateway serves the whole catalog.
	Vendor string
	// DefaultModel serves a task that asked for `auto`.
	DefaultModel string
	// ZeroMarginalCost is true when a run adds nothing to a bill. This is what
	// "prefer a subscription" actually means, and it is deliberately NOT called
	// "cheapest": cheapest implies a cost comparison across latency, quality,
	// context limits and quota that nobody can make honestly from here.
	ZeroMarginalCost bool
	// Available reports presence on this machine. A pure check — it must never
	// activate anything.
	Available func() bool
}

// Hosted is the memcode gateway: the one runtime that is always available and
// never needs authorizing, because it is memcode's own metered service rather
// than someone else's credential.
const Hosted = "memcode-hosted"

var registry = []Runtime{
	{
		ID: "claude-code", Display: "Claude Code subscription", Kind: KindSubscription,
		CredentialSource: "claude-sub", Vendor: "anthropic",
		DefaultModel: catalog.ModelSonnet, ZeroMarginalCost: true,
		Available: claudesub.Available,
	},
	{
		ID: "codex", Display: "Codex / ChatGPT subscription", Kind: KindSubscription,
		CredentialSource: "codex", Vendor: "openai",
		DefaultModel: catalog.ModelTerra, ZeroMarginalCost: true,
		Available: codex.Available,
	},
	{
		ID: "copilot", Display: "GitHub Copilot subscription", Kind: KindSubscription,
		CredentialSource: "copilot", Vendor: "openai",
		DefaultModel: "gpt-4o", ZeroMarginalCost: true,
		Available: copilot.Available,
	},
	{
		ID: "grok", Display: "SuperGrok / X Premium+ subscription", Kind: KindSubscription,
		CredentialSource: "grok", Vendor: "grok",
		DefaultModel: catalog.ModelGrok47, ZeroMarginalCost: true,
		Available: grok.Available,
	},
	{
		ID: Hosted, Display: "memcode (hosted, metered)", Kind: KindHosted,
		CredentialSource: "", Vendor: "",
		DefaultModel: "", ZeroMarginalCost: false,
		Available: func() bool { return true },
	},
}

// All returns every known runtime, in preference-neutral registry order.
func All() []Runtime { return append([]Runtime(nil), registry...) }

// Get looks up a runtime by id.
func Get(id string) (Runtime, bool) {
	for _, r := range registry {
		if r.ID == id {
			return r, true
		}
	}
	return Runtime{}, false
}

// IDs lists every known runtime id, sorted, for validation messages.
func IDs() []string {
	out := make([]string, 0, len(registry))
	for _, r := range registry {
		out = append(out, r.ID)
	}
	sort.Strings(out)
	return out
}

// Detect reports which runtimes are PRESENT on this machine.
//
// Presence only. Nothing here reads a token, opens a session, or spends
// anything, and a runtime appearing in this list has no authority whatsoever —
// it is a candidate to OFFER the user, and that is all.
func Detect() []string {
	var out []string
	for _, r := range registry {
		if r.Available != nil && r.Available() {
			out = append(out, r.ID)
		}
	}
	return out
}

// Need is what a task requires of whatever runs it.
type Need struct {
	// Model is a pinned catalog model, or "" / "auto" for no constraint.
	Model string
}

// ModelAuto means the task expressed no preference.
const ModelAuto = "auto"

// CanServe reports whether this runtime satisfies a task's needs.
//
// The check is deliberately narrow: a task that pinned a specific model needs a
// runtime whose vendor actually serves it, and everything else is served by
// anyone. Inventing finer capability claims would mean asserting things about
// latency, quota and tool support that cannot be verified from here.
func (r Runtime) CanServe(n Need) bool {
	if n.Model == "" || n.Model == ModelAuto {
		return true
	}
	if r.Kind == KindHosted {
		return true // the gateway serves the whole catalog
	}
	m, ok := catalog.LookupModel(n.Model)
	if !ok {
		// An unknown model id is only safe to send somewhere that can refuse it
		// intelligently, which is the gateway.
		return false
	}
	return m.Vendor == r.Vendor
}

// ResolveModel picks the model this runtime will actually serve.
func (r Runtime) ResolveModel(n Need) string {
	if n.Model != "" && n.Model != ModelAuto {
		return n.Model
	}
	return r.DefaultModel // "" for hosted: the catalog's own default applies
}
