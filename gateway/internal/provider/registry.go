package provider

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/memcode-ai/memcode/catalog"
)

// registry.go binds the gateway to the SHARED model catalog (models.json in the
// SDK's common package — the single source of truth for windows/vision/pricing/
// picker flags, embedded there) and loads the gateway-only ROLE config
// (config.json — which model plays which job). A model swap is "edit the SDK's
// models.json (or config.json for a role) + redeploy"; nothing about a model is
// hardcoded in Go. resolve.go selects a model by role; hybrid.go looks up the
// resolved model's properties (window/vision) here.

//go:embed config.json
var configData []byte

// ModelSpec is a model's static capabilities — the gateway's view of one shared-
// catalog entry (see catalog.CatalogModel; same fields, provider-local name kept
// so call sites read naturally).
type ModelSpec struct {
	ID        string
	Label     string // client-facing short name (e.g. "haiku", "glm-5p1") — the only id the CLI sees
	Name      string // friendly display name ("Sonnet 5") — the /model picker's name column
	Desc      string // one-line picker description ("1M context · Efficient for routine tasks")
	Window    int
	Vision    bool
	PDF       bool // accepts PDFs natively on the LLM call; without it a document turn absorbs
	Reasoning bool
	Pinnable  bool   // offered in the /model picker; Intent.Pin only honors these
	Group     string // picker display family ("OpenAI", "Claude", "Kimi", …)
}

// modelCatalog is the shared catalog + the gateway role map, loaded once at package init.
type modelCatalog struct {
	byID    map[string]ModelSpec
	byLabel map[string]ModelSpec // labels are the wire's model namespace
	ordered []ModelSpec          // catalog order — the /model picker's display order
	roles   map[string]string
}

var reg = mustLoadCatalog(configData)

func mustLoadCatalog(config []byte) *modelCatalog {
	var c struct {
		Roles map[string]string `json:"roles"`
	}
	if err := json.Unmarshal(config, &c); err != nil {
		panic(fmt.Sprintf("provider: bad config.json: %v", err))
	}
	models := catalog.CatalogModels()
	cat := &modelCatalog{
		byID:    make(map[string]ModelSpec, len(models)),
		byLabel: make(map[string]ModelSpec, len(models)),
		ordered: make([]ModelSpec, 0, len(models)),
		roles:   c.Roles,
	}
	for _, m := range models {
		spec := ModelSpec{
			ID: m.ID, Label: m.Label, Name: m.Name, Desc: m.Desc, Window: m.Window,
			Vision: m.Vision, PDF: m.PDF, Reasoning: m.Reasoning, Pinnable: m.Pinnable, Group: m.Group,
		}
		cat.ordered = append(cat.ordered, spec)
		cat.byID[spec.ID] = spec
		if spec.Label == "" {
			continue
		}
		// The SDK already panics on duplicate labels at its own load; this guard
		// stays as defense in depth for the same reason — labels are the pin
		// namespace and an ambiguous label would pin the wrong vendor's model.
		if _, dup := cat.byLabel[spec.Label]; dup {
			panic(fmt.Sprintf("provider: duplicate model label %q in the shared catalog", spec.Label))
		}
		cat.byLabel[spec.Label] = spec
	}
	return cat
}

// spec returns the catalog entry for a model id. An unknown id gets the zero spec (Window 0 =
// no overflow guard; Vision false = images escalate to the strong tier) — a misconfigured id
// fails SAFE rather than silently sending images to a model that can't read them.
func (c *modelCatalog) spec(id string) ModelSpec { return c.byID[id] }

// role returns the model id configured for a role (planner/reviewer/standard/classify), or "" if unset.
func (c *modelCatalog) role(name string) string { return c.roles[name] }

// specByLabel resolves a client-facing label ("sonnet", "glm-5p2") back to its
// catalog entry — the wire model gate's lookup. ok=false for unknown labels.
func (c *modelCatalog) specByLabel(label string) (ModelSpec, bool) {
	spec, ok := c.byLabel[label]
	return spec, ok
}
