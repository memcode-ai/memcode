package provider

import "sync"

// strong.go — the vendor → strong-provider map. Since the
// all-policy-client-side migration the tier TRIPLES (frontier/balanced/cheap)
// are catalog data (models.json "tiers", read by the CLI's selection policy);
// the gateway keeps only the construction-time map of vendors it holds keys
// for, so route() can hand a resolved raw id to the provider that serves it.

// StrongTier bundles a strong provider with its vendor label.
type StrongTier struct {
	Vendor   string
	Provider StrongProvider
}

// StrongTiers is the vendor → StrongTier map the Hybrid router consults. A
// vendor is present ONLY when its API key was set at NewFromEnv time — so
// /v1/models never lists a label the gateway can't actually serve.
type StrongTiers map[string]StrongTier

// StrongTierFor resolves a tier from a vendor name. The empty string falls
// back to the deployment default (SetDefaultVendor); an unknown or unkeyed
// vendor falls back the same way, then to any configured tier as a last
// resort. Used by the side channels (websearch/webfetch), which have no
// per-turn model context.
func (st StrongTiers) StrongTierFor(vendor string) StrongTier {
	if vendor == "" {
		vendor = defaultVendor()
	}
	if tier, ok := st[vendor]; ok {
		return tier
	}
	if tier, ok := st[defaultVendor()]; ok {
		return tier
	}
	for _, tier := range st { // last resort: any configured tier
		return tier
	}
	return StrongTier{}
}

// defaultStrongVendor is the deployment's default strong vendor: "openai" in
// hybrid mode, the vendor itself in pure single-vendor modes.
var (
	defaultStrongVendor = "openai"
	vendorMu            sync.RWMutex
)

// SetDefaultVendor pins the deployment default. Called by NewFromEnv per backend.
func SetDefaultVendor(v string) {
	if v == "" {
		v = "openai"
	}
	vendorMu.Lock()
	defer vendorMu.Unlock()
	defaultStrongVendor = v
}

// defaultVendor returns the current default strong vendor under the read lock.
func defaultVendor() string {
	vendorMu.RLock()
	defer vendorMu.RUnlock()
	return defaultStrongVendor
}
