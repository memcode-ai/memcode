package runtime

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/websites"
)

// Website builds run for minutes on memcode.ai. In an interactive session the
// session watches each build it queued, so the user sees it in the status bar
// and hears when it finishes, instead of the model asking them to "check back".
// The watch polls the build list; it never spends a model turn.

// siteBuildPoll is how often a watched build's status is checked. A var so
// tests can poll fast.
var siteBuildPoll = 5 * time.Second

// siteBuildGiveUp ends a watch that has run longer than any build can; the
// report then says to check later rather than claiming an outcome.
const siteBuildGiveUp = 45 * time.Minute

// SiteBuild is a build the session is watching, for the status bar.
type SiteBuild struct {
	Slug    string
	Started time.Time
}

// SiteBuildReport is a watched build that finished (or was given up on).
type SiteBuildReport struct {
	Slug    string
	SiteID  string
	Failed  bool
	Detail  string // the build's summary, or its error
	Took    time.Duration
	Unknown bool // the watch gave up; the outcome is not known
}

type siteBuildWatches struct {
	mu      sync.Mutex
	running map[string]SiteBuild // build ID → watch
	reports []SiteBuildReport
}

// watchSiteBuild starts watching a queued build. Interactive sessions only:
// a one-shot run has no status bar and nobody to tell, so the model keeps
// checking there.
func (s *Session) watchSiteBuild(client *websites.Client, site websites.Site, build websites.Build) {
	if !s.conversational || s.bgCtx == nil || build.ID == "" {
		return
	}
	w := &s.siteBuilds
	w.mu.Lock()
	if w.running == nil {
		w.running = map[string]SiteBuild{}
	}
	started := time.Now()
	w.running[build.ID] = SiteBuild{Slug: site.Slug, Started: started}
	w.mu.Unlock()

	go func() {
		report := SiteBuildReport{Slug: site.Slug, SiteID: site.ID, Unknown: true,
			Detail: "still running after " + siteBuildGiveUp.String() + "; check it later"}
		t := time.NewTicker(siteBuildPoll)
		defer t.Stop()
		deadline := time.After(siteBuildGiveUp)
	poll:
		for {
			select {
			case <-s.bgCtx.Done():
				w.mu.Lock()
				delete(w.running, build.ID)
				w.mu.Unlock()
				return
			case <-deadline:
				break poll
			case <-t.C:
				ctx, cancel := context.WithTimeout(s.bgCtx, 20*time.Second)
				builds, err := client.ListBuilds(ctx, site.ID)
				cancel()
				if err != nil {
					continue // a transient error; the next poll tries again
				}
				for _, b := range builds {
					if b.ID != build.ID {
						continue
					}
					switch b.Status {
					case "completed":
						report = SiteBuildReport{Slug: site.Slug, SiteID: site.ID, Detail: b.Summary}
						break poll
					case "failed":
						report = SiteBuildReport{Slug: site.Slug, SiteID: site.ID, Failed: true, Detail: b.Error}
						break poll
					}
				}
			}
		}
		report.Took = time.Since(started)
		w.mu.Lock()
		delete(w.running, build.ID)
		w.reports = append(w.reports, report)
		w.mu.Unlock()
	}()
}

// RunningSiteBuilds lists the builds being watched, oldest first.
func (s *Session) RunningSiteBuilds() []SiteBuild {
	w := &s.siteBuilds
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]SiteBuild, 0, len(w.running))
	for _, b := range w.running {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// DrainSiteBuildReports returns the watched builds that finished since the
// last call, each once. Polled by the UI.
func (s *Session) DrainSiteBuildReports() []SiteBuildReport {
	w := &s.siteBuilds
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.reports
	w.reports = nil
	return out
}

// SitePreviewURL mints a fresh preview link for a site. It carries a private
// access token, so it is for opening in the user's browser or showing to the
// user, not for the model's context.
func (s *Session) SitePreviewURL(ctx context.Context, siteID string) (string, error) {
	client, err := websites.New()
	if err != nil {
		return "", err
	}
	return client.Preview(ctx, siteID)
}
