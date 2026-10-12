package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/jobs"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/websites"
)

// Local-first websites in an interactive CLI session, the way git works:
//
//	create   → a site on this machine only (~/.memcode/websites/<name>)
//	develop  → change it locally; a dev server shows changes live
//	push     → create it on memcode.ai (first time) or update it there
//	publish  → make it live (pushing first if memcode.ai is behind)
//
// Nothing on memcode.ai changes until the user crosses that boundary. Remote
// prompt builds (`build`) stay available when the user asks for one.
//
// Site work runs in a worker rooted at the site's folder: its file tools are
// confined there, so the session's own write boundary is never widened.

// siteWorkerGuide is what a site worker is told about the project. It keeps
// local work publishable: the hosted builder and the live site enforce the
// same rules.
const siteWorkerGuide = `You are changing a static website project in this folder.

The stack: Vite + React in plain JSX (never TypeScript), pre-rendered to
static HTML per route by vite-react-ssg; Tailwind CSS v4 (src/index.css);
shadcn-style components in src/components/ui; lucide-react icons;
react-router-dom.

- Pages live in src/pages/. Register each in the routes array in src/App.jsx
  with a clean path ('pricing', not 'pricing.html'). Link with clean absolute
  paths (<Link to="/pricing">, href="/pricing").
- The published site cannot load anything from other domains: no CDN fonts,
  images, scripts, analytics, or API calls. Ship everything inside the
  project; images go in public/, and uploaded assets are served at
  /assets/<name>.
- Don't edit package.json, the lockfile, vite.config.js or jsconfig.json, and
  don't install packages or run builds. A dev server is already running and
  shows your changes live.

When you're done, describe what changed in two to four plain sentences.`

// devServer is a site's local dev server, run as a background shell.
type devServer struct {
	jobID int
	url   string
}

var localURLRE = regexp.MustCompile(`http://localhost:\d+/?`)

// websitesPushRisk: a push changes the draft on memcode.ai (and creates the
// site the first time), never what the public sees.
const websitesPushRisk = permissions.Medium

// shellQuote single-quotes one word for sh.
func shellQuote(w string) string { return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'" }

// Seams for tests: Node and a real dev server are the machine's business.
var (
	nodeReady      = (*Session).ensureNode
	startDevServer = (*Session).ensureDevServer
)

// devServerWait bounds how long a dev server may take to report its URL
// (the first start installs dependencies).
var devServerWait = 3 * time.Minute

// localSite finds the managed working copy for a site, pulling a site that
// exists only on memcode.ai into one first.
func (s *Session) localSite(ctx context.Context, client *websites.Client, key string) (websites.LocalSite, error) {
	if l, ok := websites.FindLocal(key); ok {
		return l, nil
	}
	site, err := client.Resolve(ctx, key)
	if err != nil {
		return websites.LocalSite{}, fmt.Errorf("no site named %q here or on memcode.ai", key)
	}
	if l, ok := websites.FindLocal(site.ID); ok {
		return l, nil
	}
	ws, err := websites.ManagedWorkspace(site.ID)
	if err != nil {
		return websites.LocalSite{}, err
	}
	if !ws.SourceExists() {
		if _, err := client.Pull(ctx, site.Slug, ws.SourceDir); err != nil {
			return websites.LocalSite{}, err
		}
	}
	l, ok := websites.FindLocal(site.ID)
	if !ok {
		return websites.LocalSite{}, fmt.Errorf("pulled %s but couldn't read its local copy", site.Slug)
	}
	return l, nil
}

// ensureNode makes sure npm is on PATH, offering to install Node.js with
// Homebrew when it isn't. Reports false (with why) when it stays missing.
func (s *Session) ensureNode(ctx context.Context) (bool, string) {
	if _, err := exec.LookPath("npm"); err == nil {
		return true, ""
	}
	const why = "Node.js isn't installed, and it's needed to run the site locally. Install it from https://nodejs.org, or build the site on memcode.ai instead (websites{action:\"build\"})."
	if _, err := exec.LookPath("brew"); err != nil || s.ask == nil {
		return false, why
	}
	resp := s.ask(ctx, AskRequest{
		Question: "Websites run locally with Node.js, which isn't installed. Install it now with Homebrew?",
		Options:  []AskOption{{Label: "Install Node.js"}, {Label: "Not now"}},
	})
	if resp.Answer != "Install Node.js" {
		return false, "The user chose not to install Node.js. " + why
	}
	s.printf("  installing Node.js with Homebrew…\n")
	if out, err := exec.CommandContext(ctx, "brew", "install", "node").CombinedOutput(); err != nil {
		return false, "Installing Node.js failed: " + clip(strings.TrimSpace(string(out)), 300)
	}
	if _, err := exec.LookPath("npm"); err != nil {
		return false, "Node.js was installed but npm isn't on PATH yet; the user may need to open a new terminal."
	}
	return true, ""
}

// ensureDevServer starts the site's dev server as a background shell (or
// reuses a running one) and returns its local URL. started reports a fresh
// start, the moment to offer opening it.
func (s *Session) ensureDevServer(ctx context.Context, l websites.LocalSite) (url string, started bool, err error) {
	if ds, ok := s.devServers[l.Source]; ok {
		for _, v := range s.bgJobs.List() {
			if v.ID == ds.jobID && v.Status == jobs.Running {
				return ds.url, false, nil
			}
		}
	}
	self, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	command := shellQuote(self) + " websites dev " + shellQuote(l.Source) + " --no-open"
	v, err := jobs.Start(s.bgJobs, s.bgCtx, command, l.Source)
	if err != nil {
		return "", false, err
	}
	deadline := time.Now().Add(devServerWait)
	for time.Now().Before(deadline) {
		if out, ok := s.bgJobs.Tail(v.ID, 200); ok {
			if m := localURLRE.FindString(out); m != "" {
				if s.devServers == nil {
					s.devServers = map[string]devServer{}
				}
				s.devServers[l.Source] = devServer{jobID: v.ID, url: m}
				return m, true, nil
			}
		}
		for _, j := range s.bgJobs.List() {
			if j.ID == v.ID && j.Status != jobs.Running {
				out, _ := s.bgJobs.Tail(v.ID, 30)
				return "", false, fmt.Errorf("the dev server stopped: %s", clip(strings.TrimSpace(out), 400))
			}
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return "", false, fmt.Errorf("the dev server didn't start within %s", devServerWait)
}

// offerLocalPreview asks whether to open a freshly started local preview.
// A localhost link carries no secret, so it is fine in the conversation.
func (s *Session) offerLocalPreview(ctx context.Context, name, url string) string {
	if s.ask == nil {
		return ""
	}
	resp := s.ask(ctx, AskRequest{
		Question: name + " is running at " + url + ". Open it?",
		Options:  []AskOption{{Label: previewOpen}, {Label: "Not now"}},
	})
	if resp.Answer == previewOpen {
		if err := openBrowser(url); err == nil {
			return " The user opened it in their browser."
		}
	}
	return ""
}

// createLocal makes a site on this machine only, then develops it from the
// prompt. Nothing is created on memcode.ai.
func (s *Session) createLocal(ctx context.Context, client *websites.Client, name, prompt string) toolResult {
	if strings.TrimSpace(name) == "" {
		name = siteNameFromPrompt(prompt)
	}
	l, err := client.InitLocal(ctx, name)
	if err != nil {
		return errResult("websites create failed: " + err.Error())
	}
	s.toolLine(true, "Websites", "create", l.Meta.Name+" (local)", false)
	return s.develop(ctx, client, l, prompt, true)
}

// developSite changes an existing site locally (pulling it first if it only
// exists on memcode.ai).
func (s *Session) developSite(ctx context.Context, client *websites.Client, key, prompt string) toolResult {
	l, err := s.localSite(ctx, client, key)
	if err != nil {
		return errResult(err.Error())
	}
	return s.develop(ctx, client, l, prompt, false)
}

func (s *Session) develop(ctx context.Context, client *websites.Client, l websites.LocalSite, prompt string, created bool) toolResult {
	ok, why := nodeReady(s, ctx)
	if !ok {
		return errResult(why)
	}
	caughtUp, _ := client.CatchUp(ctx, l) // start from memcode.ai's latest when this copy has nothing of its own
	url, started, err := startDevServer(s, ctx, l)
	if err != nil {
		return errResult("couldn't start the local preview: " + err.Error())
	}
	var b strings.Builder
	if created {
		fmt.Fprintf(&b, "Created %q on this machine only, at %s. It is not on memcode.ai until the user pushes it.\n", l.Meta.Name, l.Source)
	}
	if caughtUp {
		b.WriteString("Brought the local copy up to date with memcode.ai first.\n")
	}
	if strings.TrimSpace(prompt) != "" {
		s.toolLine(true, "Websites", "develop", l.Label(), false)
		res, err := s.spawnAgent(ctx, AgentSpec{
			Task:    siteWorkerGuide + "\n\nThe change to make:\n" + prompt,
			Root:    l.Source,
			Purpose: llm.Agent,
		})
		if err != nil {
			return errResult("the site change failed: " + err.Error())
		}
		fmt.Fprintf(&b, "Changed locally: %s\n", strings.TrimSpace(res.Text))
	}
	fmt.Fprintf(&b, "Local preview (live, updates as files change): %s\n", url)
	if started {
		b.WriteString(s.offerLocalPreview(ctx, l.Label(), url))
	}
	if l.Pushed() {
		fmt.Fprintf(&b, "\nThis site is on memcode.ai as %s; local changes reach it only when the user pushes.", l.Meta.Slug)
	} else {
		b.WriteString("\nWhen the user wants it online: websites{action:\"push\"} creates it on memcode.ai; publish makes it live.")
	}
	return textResult(b.String())
}

// pushSite uploads a local copy, creating the site on memcode.ai the first
// time, and watches the rebuild of its draft.
func (s *Session) pushSite(ctx context.Context, client *websites.Client, key string) toolResult {
	l, ok := websites.FindLocal(key)
	if !ok {
		return errResult("no local copy of " + key + " to push; work on it with websites{action:\"develop\"} first")
	}
	detail := "uploads local changes; the draft rebuilds on memcode.ai"
	if !l.Pushed() {
		detail = "creates the site on memcode.ai and uploads it"
	}
	if denied := s.gateWebsites(ctx, "push", websitesPushRisk, "Push website", l.Label(), detail); denied != nil {
		return *denied
	}
	site, merged, err := pushMerging(ctx, client, l)
	if err != nil {
		return errResult("websites push failed: " + err.Error())
	}
	s.toolLine(true, "Websites", "push", site.Slug, false)
	created := !l.Pushed()
	if b, ok := latestActiveBuild(ctx, client, site.ID); ok {
		s.watchSiteBuild(client, site, b)
	}
	verb := "Updated"
	if created {
		verb = "Created"
	}
	note := ""
	if merged {
		note = " memcode.ai had changes this copy didn't (from a build there); they were merged in first, so nothing was lost."
	}
	return textResult(fmt.Sprintf("%s %s on memcode.ai and uploaded the code; its draft is rebuilding.%s %s Publishing makes it live.",
		verb, site.Slug, note, s.buildFollowUp(site.Slug)))
}

// pushMerging pushes, merging in hosted changes first when memcode.ai has
// moved on (a push replaces the hosted repo, so it must contain them).
func pushMerging(ctx context.Context, client *websites.Client, l websites.LocalSite) (websites.Site, bool, error) {
	site, err := client.Push(ctx, l.Source)
	if !errors.Is(err, websites.ErrBehind) {
		return site, false, err
	}
	if err := client.MergeHosted(ctx, l); err != nil {
		return websites.Site{}, false, fmt.Errorf("%w. Tell the user: the changes need merging by hand in %s (or work from the hosted version)", err, l.Source)
	}
	site, err = client.Push(ctx, l.Source)
	return site, err == nil, err
}

// latestActiveBuild finds the build a push just queued.
func latestActiveBuild(ctx context.Context, client *websites.Client, siteID string) (websites.Build, bool) {
	builds, err := client.ListBuilds(ctx, siteID)
	if err != nil {
		return websites.Build{}, false
	}
	for _, b := range builds {
		if b.Status == "pending" || b.Status == "running" {
			return b, true
		}
	}
	return websites.Build{}, false
}

// pushBeforePublish brings memcode.ai up to date with a local copy that is
// ahead of it, and waits for the draft to finish rebuilding. publish calls it
// after its own approval, so pushing needs no second prompt.
func (s *Session) pushBeforePublish(ctx context.Context, client *websites.Client, l websites.LocalSite) (websites.Site, error) {
	site, _, err := pushMerging(ctx, client, l)
	if err != nil {
		return websites.Site{}, err
	}
	s.toolLine(true, "Websites", "push", site.Slug, false)
	b, ok := latestActiveBuild(ctx, client, site.ID)
	if !ok {
		return site, nil
	}
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return site, ctx.Err()
		case <-time.After(siteBuildPoll):
		}
		builds, err := client.ListBuilds(ctx, site.ID)
		if err != nil {
			continue
		}
		for _, x := range builds {
			if x.ID != b.ID {
				continue
			}
			switch x.Status {
			case "completed":
				return site, nil
			case "failed":
				return site, fmt.Errorf("the draft didn't build: %s", x.Error)
			}
		}
	}
	return site, fmt.Errorf("the draft is still building after 15 minutes")
}

// zipSite writes a shareable zip of a site's project to the session's folder.
func (s *Session) zipSite(key string) toolResult {
	l, ok := websites.FindLocal(key)
	if !ok {
		return errResult("no local copy of " + key + " to zip; work on it with websites{action:\"develop\"} first")
	}
	name := l.Meta.Slug
	if name == "" {
		name = filepath.Base(l.Root)
	}
	out := filepath.Join(s.root, name+".zip")
	n, err := websites.Zip(l.Source, out)
	if err != nil {
		return errResult("websites zip failed: " + err.Error())
	}
	s.toolLine(true, "Websites", "zip", l.Label(), false)
	return textResult(fmt.Sprintf("Saved %s (%d files; no node_modules or build output).", out, n))
}

// siteNameFromPrompt makes a short site name when only a prompt was given.
func siteNameFromPrompt(prompt string) string {
	words := strings.Fields(prompt)
	if len(words) > 4 {
		words = words[:4]
	}
	if n := strings.Join(words, " "); n != "" {
		return n
	}
	return "my site"
}
