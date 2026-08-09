package llm

// parity_test.go — the routing-port proof: the CLI's semantic ladder +
// physical resolution + steering must reproduce EVERY decision the gateway's
// resolve.go/steer.go made, row for row, against goldens generated from that
// code immediately before its deletion (testdata/*.json — see the deleted
// api/internal/provider/goldens_gen_test.go at the migration commit). Same
// technique as the doctrine port's live-compile parity.

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

type ladderGolden struct {
	Purpose    string `json:"purpose"`
	Mode       string `json:"mode"`
	Risk       string `json:"risk"`
	Difficulty string `json:"difficulty"`
	Reasoning  string `json:"reasoning"`
	Vendor     string `json:"vendor"`
	Pin        string `json:"pin"`
	Label      string `json:"label"`
	Pinned     bool   `json:"pinned"`
}

type steerGolden struct {
	Byok             []string `json:"byok"`
	CreditsExhausted bool     `json:"credits_exhausted"`
	ExplicitVendor   string   `json:"explicit_vendor"`
	Model            string   `json:"model"`
	Label            string   `json:"label"`
}

func loadGoldens[T any](t *testing.T, name string) []T {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("goldens missing: %v", err)
	}
	var out []T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// prodInfo mirrors the production hybrid deployment the goldens were generated
// under: config.json roles (as labels), all four strong vendors configured
// with openai the default, byok per the given vendor list.
func prodInfo(byok []string) provider.ModelsInfo {
	keyed := map[string]bool{}
	for _, v := range byok {
		keyed[v] = true
	}
	info := provider.ModelsInfo{
		Backend: "hybrid",
		Vendors: []string{"openai", "anthropic", "gemini", "grok"},
		Roles: []provider.RoleModel{
			{Role: "planner", Label: "glm-5p2"},
			{Role: "reviewer", Label: "luna"},
			{Role: "standard", Label: "glm-5p2"},
			{Role: "classify", Label: "gpt-oss-120b"},
		},
	}
	for _, m := range catalog.CatalogModels() {
		if m.Window <= 0 {
			continue
		}
		info.Models = append(info.Models, provider.ModelFact{
			Label: m.Label, Vendor: m.Vendor, Window: m.Window,
			Vision: m.Vision, PDF: m.PDF, Pinnable: m.Pinnable,
			Byok: keyed[m.Vendor],
		})
	}
	return info
}

func TestLadderParityWithGateway(t *testing.T) {
	rows := loadGoldens[ladderGolden](t, "ladder_goldens.json")
	info := prodInfo(nil)
	for _, g := range rows {
		it := wire.Intent{Purpose: g.Purpose, Mode: g.Mode, Risk: g.Risk,
			Difficulty: g.Difficulty, Reasoning: wire.Effort(g.Reasoning),
			Vendor: g.Vendor, Pin: g.Pin}
		res := resolveHosted(it, wire.Request{}, info)
		if res.err != nil {
			t.Fatalf("%+v: unexpected error %v", g, res.err)
		}
		if res.label != g.Label || res.pinned != g.Pinned {
			t.Errorf("intent %+v: resolved (%q, pinned=%v), gateway resolved (%q, pinned=%v)",
				it, res.label, res.pinned, g.Label, g.Pinned)
		}
	}
	t.Logf("ladder parity: %d rows reproduced", len(rows))
}

func TestSteerParityWithGateway(t *testing.T) {
	rows := loadGoldens[steerGolden](t, "steer_goldens.json")
	for _, g := range rows {
		info := prodInfo(g.Byok)
		info.CreditsExhausted = g.CreditsExhausted
		got := steerLabel(g.Model, g.ExplicitVendor, info)
		if got != g.Label {
			t.Errorf("steer(byok=%v credits=%v vendor=%q, %q) = %q, gateway steered %q",
				g.Byok, g.CreditsExhausted, g.ExplicitVendor, g.Model, got, g.Label)
		}
	}
	t.Logf("steering parity: %d rows reproduced", len(rows))
}
