package provider

import "testing"

// The shared adapters must keep satisfying the gateway's StrongProvider
// surface (ModelProvider + Streamer + WebSearcher + WebFetcher + Model()) —
// the extraction moved the implementation, not the contract.
func TestSharedAdaptersAreStrongProviders(t *testing.T) {
	var _ StrongProvider = (*OpenAI)(nil)
	var _ StrongProvider = (*Grok)(nil)
}
