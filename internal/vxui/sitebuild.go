package vxui

import (
	"context"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/authflow"
)

// announceSiteBuild reports a finished website build in the transcript and,
// when it succeeded, asks whether to open the preview. The preview link
// carries a private access token, so it is minted only when the user wants
// it, and never enters the conversation. Runs on the UI thread; the question
// is asked off it (the ask blocks until answered).
func (s *appState) announceSiteBuild(r runtime.SiteBuildReport) {
	took := elapsedShort(r.Took)
	switch {
	case r.Unknown:
		s.sysln(fmt.Sprintf("  ⊙ %s is %s", r.Slug, r.Detail))
		return
	case r.Failed:
		detail := strings.TrimSpace(r.Detail)
		if detail == "" {
			detail = "no reason given"
		}
		s.sysln(fmt.Sprintf("  ✗ %s build failed after %s: %s", r.Slug, took, clipLine(detail, 160)))
		return
	}
	s.sysln(fmt.Sprintf("  ✓ %s finished building in %s", r.Slug, took))
	if s.askFn == nil {
		return
	}
	ctx := s.w.ctx
	go func() {
		resp := s.askFn(ctx, runtime.AskRequest{
			Question: r.Slug + " is built. Open the preview?",
			Options: []runtime.AskOption{
				{Label: "Open it in my browser"},
				{Label: "Show me the link", Description: "a private link that works for 6 hours"},
				{Label: "Not now"},
			},
		})
		if resp.Answer != "Open it in my browser" && resp.Answer != "Show me the link" {
			return
		}
		url, err := s.w.sess.SitePreviewURL(context.WithoutCancel(ctx), r.SiteID)
		if err != nil {
			s.rt.Dispatch(func() { s.sysln("  couldn't get the preview link: " + err.Error()) })
			return
		}
		if resp.Answer == "Open it in my browser" && authflow.OpenBrowser(url) == nil {
			return
		}
		s.rt.Dispatch(func() { s.sysln("  preview: " + url) })
	}()
}

// clipLine shortens a one-line message for the transcript.
func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
