package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/websites"
)

// buildServer reports build b1 of site s1 as `running` for the first n polls,
// then as `final` (completed or failed).
func buildServer(t *testing.T, n int32, final string) http.HandlerFunc {
	var polls int32
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/website/s1/builds" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		status := "running"
		if atomic.AddInt32(&polls, 1) > n {
			status = final
		}
		json.NewEncoder(w).Encode(map[string]any{"builds": []map[string]any{{
			"id": "b1", "status": status, "summary": "built the bakery site", "error": "vite exited 1",
		}}})
	}
}

func watchingSession(t *testing.T, h http.HandlerFunc) *Session {
	old := siteBuildPoll
	siteBuildPoll = 10 * time.Millisecond
	t.Cleanup(func() { siteBuildPoll = old })
	s := websitesSession(t, h)
	s.conversational = true
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.bgCtx = ctx
	return s
}

func waitReport(t *testing.T, s *Session) SiteBuildReport {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r := s.DrainSiteBuildReports(); len(r) > 0 {
			if len(r) != 1 {
				t.Fatalf("want one report, got %d", len(r))
			}
			return r[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the watched build never reported")
	return SiteBuildReport{}
}

// A queued build shows in the status bar while it runs and reports once when
// it finishes, with no model turn involved.
func TestWatchedBuildShowsWhileRunningAndReportsOnce(t *testing.T) {
	s := watchingSession(t, buildServer(t, 3, "completed"))
	client, err := websites.New()
	if err != nil {
		t.Fatal(err)
	}
	s.watchSiteBuild(client, websites.Site{ID: "s1", Slug: "crumb"}, websites.Build{ID: "b1", Status: "pending"})
	if b := s.RunningSiteBuilds(); len(b) != 1 || b[0].Slug != "crumb" {
		t.Fatalf("running = %+v, want crumb", b)
	}
	r := waitReport(t, s)
	if r.Failed || r.Unknown || r.Slug != "crumb" || r.Detail != "built the bakery site" {
		t.Fatalf("report = %+v", r)
	}
	if len(s.RunningSiteBuilds()) != 0 {
		t.Fatal("a finished build must leave the status bar")
	}
	if len(s.DrainSiteBuildReports()) != 0 {
		t.Fatal("a report drains once")
	}
}

func TestWatchedBuildReportsFailure(t *testing.T) {
	s := watchingSession(t, buildServer(t, 0, "failed"))
	client, _ := websites.New()
	s.watchSiteBuild(client, websites.Site{ID: "s1", Slug: "crumb"}, websites.Build{ID: "b1"})
	r := waitReport(t, s)
	if !r.Failed || r.Detail != "vite exited 1" {
		t.Fatalf("report = %+v", r)
	}
}

// A one-shot run has no status bar: nothing is watched, and the model is told
// to check progress itself.
func TestOneShotRunDoesNotWatch(t *testing.T) {
	s := websitesSession(t, buildServer(t, 0, "completed"))
	client, _ := websites.New()
	s.watchSiteBuild(client, websites.Site{ID: "s1", Slug: "crumb"}, websites.Build{ID: "b1"})
	if len(s.RunningSiteBuilds()) != 0 {
		t.Fatal("a one-shot run must not watch builds")
	}
	if !strings.Contains(s.buildFollowUp("crumb"), `action:"builds"`) {
		t.Fatalf("one-shot follow-up = %q", s.buildFollowUp("crumb"))
	}
}

const testPreviewURL = "https://preview--crumb.memcode.app/?__memcode_token=SECRET"

// The preview link carries a private token: in an interactive session the
// user is offered to open it, and the link never reaches the model.
func TestPreviewIsOfferedNotPasted(t *testing.T) {
	var opened string
	old := openBrowser
	openBrowser = func(u string) error { opened = u; return nil }
	t.Cleanup(func() { openBrowser = old })

	for _, tc := range []struct {
		answer     string
		wantOpened bool
		wantShown  bool
	}{
		{previewOpen, true, false},
		{previewLink, false, true},
		{"Not now", false, false},
	} {
		opened = ""
		var out bytes.Buffer
		s := newTodoSession(t)
		s.out = &out
		s.conversational = true
		s.ask = func(context.Context, AskRequest) AskResponse { return AskResponse{Answer: tc.answer} }
		r := s.offerPreview(context.Background(), "crumb", testPreviewURL, false)
		if strings.Contains(r.text(), "SECRET") {
			t.Errorf("%s: the token reached the model: %s", tc.answer, r.text())
		}
		if (opened != "") != tc.wantOpened {
			t.Errorf("%s: opened = %q", tc.answer, opened)
		}
		if strings.Contains(out.String(), "SECRET") != tc.wantShown {
			t.Errorf("%s: shown to user = %v, want %v", tc.answer, strings.Contains(out.String(), "SECRET"), tc.wantShown)
		}
	}
}

// When the user asks for the link itself, or there is no one to ask, the
// link is returned.
func TestPreviewRevealAndOneShotReturnTheLink(t *testing.T) {
	s := newTodoSession(t)
	s.conversational = true
	s.ask = func(context.Context, AskRequest) AskResponse {
		t.Fatal("reveal must not ask")
		return AskResponse{}
	}
	if r := s.offerPreview(context.Background(), "crumb", testPreviewURL, true); !strings.Contains(r.text(), testPreviewURL) {
		t.Fatalf("reveal: %s", r.text())
	}
	oneShot := newTodoSession(t)
	if r := oneShot.offerPreview(context.Background(), "crumb", testPreviewURL, false); !strings.Contains(r.text(), testPreviewURL) {
		t.Fatalf("one-shot: %s", r.text())
	}
}
