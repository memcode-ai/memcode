package tools

import "github.com/memcode-ai/memcode/internal/wire"

// Site is the website worker's own tool: it SEES pages (look, inspect), adds
// packages through the dependency policy, and runs the production-build
// check. Advertised only to a website worker, never to an ordinary session.
const Site = "site"

// SiteInput drives the site tool.
type SiteInput struct {
	Action   string `json:"action"`             // look | inspect | add_package | check
	URL      string `json:"url,omitempty"`      // look/inspect: a reference page (http/https)
	Path     string `json:"path,omitempty"`     // look/inspect: a route of the site being built ("/", "/about")
	Viewport string `json:"viewport,omitempty"` // desktop (default) | laptop | tablet | mobile
	From     int    `json:"from,omitempty"`     // look: page offset in px to start at
	Screens  int    `json:"screens,omitempty"`  // look: how many viewport-high screens (default 4, max 8)
	Package  string `json:"package,omitempty"`  // add_package: npm name, optionally name@version
}

// SiteDef is the site tool's schema.
func SiteDef() wire.ToolDef {
	return wire.ToolDef{
		Name: Site,
		Description: "See and verify the website you're building. " +
			"look: render a page in a real browser and return screenshots, one per viewport-high screen, scrolling between them so scroll animations play (url for a reference site, path for this site's own pages). " +
			"inspect: measure a rendered page's design system (fonts, type scale with sizes/weights/tracking, palette by area, content width, gutters, section spacing, sections top to bottom, buttons, radii, motion libraries). " +
			"add_package: install an npm package (kit packages always; anything else only if it passes the safety policy). " +
			"check: run the production build memcode.ai runs and report anything that would break or be blocked when published.",
		InputSchema: obj(map[string]any{
			"action":   enum("what to do", []string{"look", "inspect", "add_package", "check"}),
			"url":      str("look/inspect: a reference page, http(s)"),
			"path":     str("look/inspect: a route of the site you're building, like / or /about"),
			"viewport": enum("look/inspect: device size (default desktop 1440x900)", []string{"desktop", "laptop", "tablet", "mobile"}),
			"from":     integer("look: page offset in px to start at (default 0)"),
			"screens":  integer("look: how many screens to capture (default 4, max 8)"),
			"package":  str("add_package: npm package name, optionally name@version"),
		}, "action"),
	}
}
