package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/sitelook"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/websites"
	"github.com/memcode-ai/memcode/internal/wire"
)

// siteScript plays a website worker: it writes a component, looks at the
// page, and finishes. Requests that don't offer the site tool (anything but
// a worker) just end.
type siteScript struct {
	mu      sync.Mutex
	workers int             // worker runs started
	steps   map[int]int     // per worker run: next step
	first   []wire.Request  // each worker run's first request
	tools   map[string]bool // tools offered to the worker
}

func (p *siteScript) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	worker := false
	for _, d := range r.Tools {
		worker = worker || d.Name == tools.Site
	}
	if !worker {
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{wire.TextBlock("ok")}}, nil
	}
	if len(r.Messages) == 1 { // a new worker run
		p.workers++
		p.first = append(p.first, r)
		p.tools = map[string]bool{}
		for _, d := range r.Tools {
			p.tools[d.Name] = true
		}
	}
	if p.steps == nil {
		p.steps = map[int]int{}
	}
	step := p.steps[p.workers]
	p.steps[p.workers]++
	switch step {
	case 0:
		in, _ := json.Marshal(tools.EditFileInput{Path: "src/Hero.jsx", NewString: "export default function Hero() { return null }\n"})
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{{Type: "tool_use", ID: "e", Name: tools.EditFile, Input: in}}}, nil
	case 1:
		in, _ := json.Marshal(tools.SiteInput{Action: "look", Path: "/"})
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{{Type: "tool_use", ID: "l", Name: tools.Site, Input: in}}}, nil
	}
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{wire.TextBlock("Built the hero to match the reference.")}}, nil
}

func (p *siteScript) Stream(ctx context.Context, r wire.Request, _ wire.StreamHandler) (wire.Response, error) {
	return p.Complete(ctx, r)
}

// siteSession is localFirstSession with a scripted provider, auto mode, and
// its rendered output captured.
func siteSession(t *testing.T, prov *siteScript) (*Session, *bytes.Buffer) {
	t.Helper()
	ref, _ := localFirstSession(t) // sets up memcode.ai, HOME, and the Node/dev-server stubs
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	out := &bytes.Buffer{}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAuto, &lockedWriter{w: out})
	s.conversational, s.lastUserText, s.bgCtx = true, ref.lastUserText, ref.bgCtx
	return s, out
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func stubSiteSeeing(t *testing.T, builds ...websites.BuildReport) *int {
	t.Helper()
	oldB, oldC, oldBuild := siteBrowser, siteCapture, siteBuild
	siteBrowser = func(*Session) (sitelook.Browser, error) { return nil, nil }
	siteCapture = func(_ context.Context, _ sitelook.Browser, url string, vp sitelook.Viewport, _, _ int) (sitelook.Look, error) {
		return sitelook.Look{URL: url, Viewport: vp, PageHeight: 1800, Screens: []sitelook.Screen{
			{Top: 0, Image: []byte("\x89PNG one"), Mime: "image/png"},
			{Top: 900, Image: []byte("\x89PNG two"), Mime: "image/png"},
		}}, nil
	}
	calls := 0
	siteBuild = func(context.Context, string) websites.BuildReport {
		r := websites.BuildReport{OK: true, Pages: 2}
		if calls < len(builds) {
			r = builds[calls]
		}
		calls++
		return r
	}
	t.Cleanup(func() { siteBrowser, siteCapture, siteBuild = oldB, oldC, oldBuild })
	return &calls
}

// THE EXPERIENCE: a website change is made by a worker the user can watch,
// that is told the whole brief and its references, sees its pages, is held
// to the production build (and sent back once when it fails), and leaves one
// commit behind.
func TestDevelopIsVisibleSeesAndBuilds(t *testing.T) {
	prov := &siteScript{}
	s, out := siteSession(t, prov)
	builds := stubSiteSeeing(t, websites.BuildReport{Problems: []string{"`npm run build` failed"}, Output: "window is not defined"})

	r := websitesCall(s, tools.WebsitesInput{Action: "create", Name: "Elventech",
		Prompt:     "An AI consulting site. Past clients: Samsung, Apple. We are not backed by anyone.",
		References: []string{"https://ode.com"}})
	if r.isError {
		t.Fatal(r.text())
	}
	text := r.text()

	// The worker was told the brief verbatim, the references, the preview,
	// and the guide; it could see (site tool) but not publish or schedule.
	if prov.workers != 2 {
		t.Fatalf("worker runs = %d, want 2 (the change, then one repair after the failed check)", prov.workers)
	}
	task := prov.first[0].Messages[0].Blocks[0].Text
	for _, want := range []string{"Past clients: Samsung, Apple", "https://ode.com", "http://localhost:5173/", "You can SEE", "Never invent facts"} {
		if !strings.Contains(task, want) {
			t.Errorf("worker task is missing %q", want)
		}
	}
	if repair := prov.first[1].Messages[0].Blocks[0].Text; !strings.Contains(repair, "window is not defined") {
		t.Errorf("the repair round wasn't told what failed:\n%s", repair)
	}
	if !prov.tools[tools.Site] || prov.tools[tools.Websites] || prov.tools[tools.CapabilityTool] {
		t.Errorf("worker tools = %v; want site, and no websites/capability", prov.tools)
	}
	if *builds != 2 {
		t.Errorf("production checks = %d, want 2", *builds)
	}

	// The user watched it work: the worker's lines appear, nested.
	shown := out.String()
	for _, want := range []string{"│", "Hero.jsx", "Look(/ · desktop)", "on this machine", "builds"} {
		if !strings.Contains(shown, want) {
			t.Errorf("the user didn't see %q in:\n%s", want, shown)
		}
	}

	// The model learns what changed and that it builds.
	for _, want := range []string{"Production check: passes", "src/Hero.jsx", "Screenshots"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing %q:\n%s", want, text)
		}
	}

	l, ok := websites.FindLocal("Elventech")
	if !ok {
		t.Fatal("no local site")
	}
	log, _ := exec.Command("git", "-C", l.Source, "log", "--format=%s").Output()
	if !strings.Contains(string(log), "Change made with memcode: An AI consulting site") {
		t.Errorf("the change wasn't recorded as a commit:\n%s", log)
	}
	shots, _ := filepath.Glob(filepath.Join(l.Source, ".memcode", "looks", "*", "*.png"))
	if len(shots) < 2 {
		t.Errorf("screenshots weren't saved for the user: %v", shots)
	}
	if status, _ := exec.Command("git", "-C", l.Source, "status", "--porcelain").Output(); len(bytes.TrimSpace(status)) > 0 {
		t.Errorf("screenshots or state leaked into the site's repo:\n%s", status)
	}
}

// A look returns the screens as images the model sees, labeled by position.
func TestSiteLookReturnsScreens(t *testing.T) {
	stubSiteSeeing(t)
	s := newTodoSession(t)
	s.site = newSiteWork(t.TempDir(), "http://localhost:5173/", nil)
	in, _ := json.Marshal(tools.SiteInput{Action: "look", URL: "https://ode.com/about"})
	r := s.siteTool(context.Background(), in)
	if r.isError {
		t.Fatal(r.text())
	}
	images := 0
	for _, b := range r.blocks {
		if b.Type == "image" {
			images++
		}
	}
	if images != 2 || !strings.Contains(r.text(), "Screen at 900–1800px") {
		t.Fatalf("images=%d text=%s", images, r.text())
	}
}

// The worker sees the public web and its own preview, never the user's
// network.
func TestSiteTargets(t *testing.T) {
	s := newTodoSession(t)
	s.site = newSiteWork(t.TempDir(), "http://localhost:5173/", nil)
	for _, tc := range []struct {
		in   tools.SiteInput
		want string // "" = refused
	}{
		{tools.SiteInput{Path: "/about"}, "http://localhost:5173/about"},
		{tools.SiteInput{Path: "pricing"}, "http://localhost:5173/pricing"},
		{tools.SiteInput{}, "http://localhost:5173/"},
		{tools.SiteInput{URL: "https://ode.com"}, "https://ode.com"},
		{tools.SiteInput{URL: "http://localhost:5173/x"}, "http://localhost:5173/x"},
		{tools.SiteInput{URL: "http://192.168.1.20/admin"}, ""},
		{tools.SiteInput{URL: "http://localhost:8080/"}, ""},
		{tools.SiteInput{URL: "file:///etc/passwd"}, ""},
	} {
		got, _, err := s.siteTarget(tc.in)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%+v: want refused, got %s", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%+v = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

// Packages reach a site only through add_package (the policy); the worker's
// shell can't install them, however the command is dressed up.
func TestSiteWorkerCantRunPackageManagers(t *testing.T) {
	for _, cmd := range []string{"npm install gsap", "cd src && npm i three", "npx vite build", "sh -c 'yarn add x'", "pnpm add y", "git push origin main"} {
		if _, denied := permissions.DeniedBy(cmd, siteWorkerDeny); !denied {
			t.Errorf("%q should be denied to a site worker", cmd)
		}
	}
	for _, cmd := range []string{"npm run build", "ls src", "git status", "node -e 1"} {
		if _, denied := permissions.DeniedBy(cmd, siteWorkerDeny); denied {
			t.Errorf("%q should be allowed", cmd)
		}
	}
}

// A worker's output reads as nested under its marker: every line on the
// rail, blank runs collapsed, the routing line left to the parent.
func TestNestedOutput(t *testing.T) {
	var buf bytes.Buffer
	parent := newTodoSession(t)
	parent.out = &buf
	w := &nestedOutput{parent: parent}
	w.Write([]byte("\n\nfirst"))
	w.Write([]byte(" line\n\n\n\n\x1b[2m⇄ served by sonnet\x1b[m\nsecond\n"))
	got := stripANSI(buf.String())
	want := "  │ first line\n  │\n  │ second\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
