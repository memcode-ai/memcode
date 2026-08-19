package vxui

import (
	"github.com/memcode-ai/memcode/internal/artifacts"
)

// artifactsSlash lists the org's published artifact pages — a thin, read-only
// view of the artifacts the agent publishes with its artifact tool. Open and
// delete live in `memcode artifacts`, which the output points at; the TUI
// stays a viewer. Fetch runs off the UI thread and prints via the runner
// dispatch, matching the async slash idiom (websitesSlash).
func (s *appState) artifactsSlash() {
	c, err := artifacts.New()
	if err != nil {
		s.sysln(err.Error())
		return
	}
	s.sysln("◆ artifacts  fetching…")
	go func() {
		list, err := c.List(s.w.ctx)
		s.rt.Dispatch(func() {
			if err != nil {
				s.sysln("✗ " + err.Error())
				return
			}
			if len(list) == 0 {
				s.sysln("No artifacts yet — ask the agent to publish one with the artifact tool")
				return
			}
			s.sysln(artifacts.RenderList(list))
			s.sysln("open one in the browser with `memcode artifacts open <id>`")
		})
	}()
}
