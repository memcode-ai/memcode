package provider

import (
	"context"
	"os"
	"sync"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/providers/memcode"
)

// models.go — the CLI's view of the memcode routing CONTROL PLANE. The
// protocol client (GET /v1/models decode over the shared wire types) lives in
// the memcode provider package (providers/memcode); this file keeps the
// CLI-side env resolution and the picker convenience, plus type aliases so
// the runtime/selection/vxui keep their existing names.

type (
	ModelFact     = memcode.ModelFact
	PinnableModel = memcode.PinnableModel
	ModelsInfo    = memcode.ModelsInfo
)

// FetchModels asks the gateway for the routing control plane, resolving the
// endpoint + credential from the environment the way every CLI surface does.
// Every successful fetch also refreshes the pin list CachedPins serves, so the
// boot fetch is what lets /model open instantly.
func FetchModels(ctx context.Context) (ModelsInfo, error) {
	info, err := memcode.FetchModels(ctx, envOr(EnvAPIURL, DefaultAPIURL), os.Getenv(EnvAPIToken))
	if err == nil {
		pinCache.Lock()
		pinCache.pins, pinCache.ok = pinsFrom(info), true
		pinCache.Unlock()
	}
	return info, err
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// pinCache holds the pinnable list from the last successful gateway fetch.
var pinCache struct {
	sync.Mutex
	pins []PinnableModel
	ok   bool
}

// AvailablePins asks the gateway which concrete models the /model picker may
// offer (the pinnable subset of the servable list). On gateway outage or a
// lane-only session it falls back to CachedPins, so /model remains a real
// picker instead of collapsing to free-text.
func AvailablePins(ctx context.Context) []PinnableModel {
	info, err := FetchModels(ctx)
	if err != nil {
		return CachedPins()
	}
	return pinsFrom(info)
}

// CachedPins returns the picker rows without touching the network: the last
// list the gateway gave, or the embedded catalog before any fetch succeeded.
func CachedPins() []PinnableModel {
	pinCache.Lock()
	defer pinCache.Unlock()
	if pinCache.ok {
		return append([]PinnableModel(nil), pinCache.pins...)
	}
	return catalogPins()
}

func pinsFrom(info ModelsInfo) []PinnableModel {
	var out []PinnableModel
	for _, f := range info.Models {
		if !f.Pinnable {
			continue
		}
		out = append(out, PinnableModel{Label: f.Label, Name: f.Name, Desc: f.Desc,
			Group: f.Group, Window: f.Window, Vision: f.Vision, Byok: f.Byok})
	}
	return out
}

func catalogPins() []PinnableModel {
	var out []PinnableModel
	for _, m := range catalog.CatalogModels() {
		if !m.Pinnable {
			continue
		}
		out = append(out, PinnableModel{
			Label: m.Label, Name: m.Name, Desc: m.Desc,
			Group: m.Group, Window: m.Window, Vision: m.Vision,
		})
	}
	return out
}
