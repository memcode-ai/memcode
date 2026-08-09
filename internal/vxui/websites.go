package vxui

import (
	"github.com/memcode-ai/memcode/internal/websites"
)

// websitesSlash lists the org's AI-built websites — a thin, read-only view of
// the www Websites feature. The heavy lifting (pull/push/publish) lives in
// `memcode websites`, which the output points at; the TUI stays a viewer.
// Fetch runs off the UI thread and prints via the runner dispatch, matching
// the async slash idiom (planAskAdvisor).
func (s *appState) websitesSlash() {
	c, err := websites.New()
	if err != nil {
		s.sysln(err.Error())
		return
	}
	s.sysln("◆ websites  fetching…")
	go func() {
		sites, err := c.List(s.w.ctx)
		s.rt.Dispatch(func() {
			if err != nil {
				s.sysln("✗ " + err.Error())
				return
			}
			if len(sites) == 0 {
				s.sysln("No websites yet — create one at memcode.ai/websites")
				return
			}
			s.sysln(websites.RenderList(sites))
			s.sysln("pull one locally with `memcode websites pull <slug>` — then run memcode inside it to iterate")
		})
	}()
}
