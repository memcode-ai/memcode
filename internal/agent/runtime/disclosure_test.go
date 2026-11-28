package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// Publishing puts workspace content on a public link: Auto mode asks, and a
// denial means nothing reaches the server.
func TestArtifactPublishAsksInAuto(t *testing.T) {
	s, hits := artifactSession(t)
	s.SetMode(permissions.ModeAuto)
	asked := 0
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		asked++
		return ApprovalDecision{Allow: false}
	}
	path := writeArtifactFile(t, s, "page.html", "<h1>hi</h1>")
	res := artifactCall(s, tools.ArtifactInput{Action: "publish", Path: path, Title: "x"})
	if asked != 1 || !res.isError || *hits != 0 {
		t.Fatalf("Auto must ask before publishing: asked=%d err=%v hits=%d", asked, res.isError, *hits)
	}
}

// A job answering a group chat can't publish at all — not in allow-all, not
// with a remembered approval — and nothing is uploaded.
func TestSharedAudienceRefusesPublishing(t *testing.T) {
	s, hits := artifactSession(t)
	s.SetMode(permissions.ModeAllowAll)
	s.approvedArtifacts = true
	s.SetSharedAudience(true)
	path := writeArtifactFile(t, s, "page.html", "<h1>hi</h1>")
	res := artifactCall(s, tools.ArtifactInput{Action: "publish", Path: path, Title: "x"})
	if !res.isError || !strings.Contains(res.text(), "group chat") || *hits != 0 {
		t.Fatalf("group job published: err=%v %q hits=%d", res.isError, res.text(), *hits)
	}
	if r, _ := websiteActionRisk("share"); r < permissions.Dangerous {
		t.Fatal("a website share link is external disclosure: must be Dangerous")
	}
}

// Credential files never become public pages, even when approved.
func TestArtifactRefusesCredentialFiles(t *testing.T) {
	s, hits := artifactSession(t)
	s.SetMode(permissions.ModeAllowAll)
	path := writeArtifactFile(t, s, ".env", "API_KEY=x")
	res := artifactCall(s, tools.ArtifactInput{Action: "publish", Path: path, Title: "x"})
	if !res.isError || *hits != 0 {
		t.Fatalf("credentials file was publishable: %q", res.text())
	}
}
