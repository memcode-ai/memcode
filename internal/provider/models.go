package provider

import (
	"context"
	"os"

	"github.com/memcode-ai/memcode/internal/providers/memcode"
)

// models.go — the CLI's view of the memcode routing CONTROL PLANE. The
// protocol client (GET /v1/models decode over the shared wire types) lives in
// the memcode provider package (providers/memcode); this file keeps the
// CLI-side env resolution and the picker convenience, plus type aliases so
// the runtime/selection/vxui keep their existing names.

type (
	RoleModel     = memcode.RoleModel
	ModelFact     = memcode.ModelFact
	PinnableModel = memcode.PinnableModel
	ModelsInfo    = memcode.ModelsInfo
)

// FetchModels asks the gateway for the routing control plane, resolving the
// endpoint + credential from the environment the way every CLI surface does.
func FetchModels(ctx context.Context) (ModelsInfo, error) {
	return memcode.FetchModels(ctx, envOr(EnvAPIURL, DefaultAPIURL), os.Getenv(EnvAPIToken))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// AvailablePins asks the gateway which concrete models the /model picker may
// offer (the pinnable subset of the servable list). Empty on error — the
// picker then shows Automatic only.
func AvailablePins(ctx context.Context) []PinnableModel {
	info, err := FetchModels(ctx)
	if err != nil {
		return nil
	}
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
