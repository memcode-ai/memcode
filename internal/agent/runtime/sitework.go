package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/sitelook"
	"github.com/memcode-ai/memcode/internal/websites"
	"github.com/memcode-ai/memcode/internal/wire"
)

// A website worker is a sub-agent rooted at a site's folder (websites
// develop/create). Beyond the ordinary file tools it gets the `site` tool:
// it can SEE pages in a headless Chrome (a reference the user named, and the
// site it is building, through the running dev server), add packages through
// the dependency policy, and run the production build memcode.ai will run.

// siteWork is what a website worker knows about the site it builds.
type siteWork struct {
	dir     string           // the site's source folder (the worker's root)
	preview string           // the local dev server, e.g. http://localhost:5173/
	client  *websites.Client // for the current kit; nil works (falls back to the site's own)
	reg     *websites.Registry

	mu      sync.Mutex
	kit     *websites.Kit
	looks   int    // screens captured, for the summary
	lookDir string // where screenshots are saved for the user to open
}

func newSiteWork(dir, preview string, client *websites.Client) *siteWork {
	return &siteWork{dir: dir, preview: preview, client: client, reg: websites.NewRegistry(),
		lookDir: filepath.Join(dir, ".memcode", "looks")}
}

// siteWorkerDeny: package managers are reached only through site
// add_package, so every install passes the same policy as a push.
var siteWorkerDeny = []string{
	"npm install", "npm i", "npm add", "npm ci", "npm update", "npm uninstall", "npm remove", "npm rm", "npm link", "npm exec",
	"npx", "pnpm", "yarn", "bun", "git push",
}

func (w *siteWork) siteKit(ctx context.Context) websites.Kit {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.kit == nil {
		k := websites.SiteKit(ctx, w.client, w.dir)
		w.kit = &k
	}
	return *w.kit
}

// lookSeams lets tests drive the tool without Chrome.
var (
	siteBrowser = func(s *Session) (sitelook.Browser, error) { return s.browserOrInit() }
	siteCapture = sitelook.Capture
	siteInspect = sitelook.Inspect
	siteBuild   = websites.BuildCheck
)

func (s *Session) siteTool(ctx context.Context, input json.RawMessage) toolResult {
	if s.site == nil {
		return errResult("the site tool is only available while working on a memcode website")
	}
	var in tools.SiteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("site: bad input: " + err.Error())
	}
	switch in.Action {
	case "look", "inspect":
		return s.siteSee(ctx, in)
	case "add_package":
		return s.siteAddPackage(ctx, in.Package)
	case "check":
		return s.siteCheck(ctx)
	}
	return errResult("site: unknown action " + in.Action + " (look | inspect | add_package | check)")
}

// siteTarget resolves what to render: a public reference URL, or a route of
// the site through its local preview. Other local or private addresses are
// refused: the worker sees the web and its own site, not the user's network.
func (s *Session) siteTarget(in tools.SiteInput) (target, label string, err error) {
	if u := strings.TrimSpace(in.URL); u != "" {
		pu, perr := url.Parse(u)
		if perr != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			return "", "", fmt.Errorf("url must be a full http(s) address")
		}
		if isLocalURL(u) && !strings.HasPrefix(u, strings.TrimRight(s.site.preview, "/")) {
			return "", "", fmt.Errorf("only public pages and this site's own preview can be opened; use path for this site")
		}
		return u, strings.TrimPrefix(pu.Host, "www.") + strings.TrimRight(pu.EscapedPath(), "/"), nil
	}
	if s.site.preview == "" {
		return "", "", fmt.Errorf("this site's preview isn't running")
	}
	p := strings.TrimSpace(in.Path)
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(s.site.preview, "/") + p, p, nil
}

func (s *Session) siteSee(ctx context.Context, in tools.SiteInput) toolResult {
	target, label, err := s.siteTarget(in)
	if err != nil {
		return errResult(err.Error())
	}
	vp, ok := sitelook.ViewportNamed(in.Viewport)
	if !ok {
		return errResult("viewport must be desktop, laptop, tablet or mobile")
	}
	b, err := siteBrowser(s)
	if err != nil {
		return errResult("can't open a browser to look at pages: " + err.Error() + ". Tell the user; without Chrome the site can only be built blind.")
	}
	verb := "Look"
	if in.Action == "inspect" {
		verb = "Inspect"
	}
	if in.Action == "inspect" {
		r, err := siteInspect(ctx, b, target, vp)
		if err != nil {
			s.toolLine(true, verb, label+" · "+vp.Name, "failed", true)
			return errResult(err.Error())
		}
		s.toolLine(true, verb, label+" · "+vp.Name, fmt.Sprintf("%d sections, %d type styles", len(r.Sections), len(r.Type)), false)
		return textResult(r.Format())
	}
	look, err := siteCapture(ctx, b, target, vp, in.From, in.Screens)
	if err != nil && len(look.Screens) == 0 {
		s.toolLine(true, verb, label+" · "+vp.Name, "failed", true)
		return errResult(err.Error())
	}
	saved := s.saveLook(label, vp.Name, look)
	status := fmt.Sprintf("%d screen%s", len(look.Screens), plural(len(look.Screens)))
	if saved != "" {
		status += " → " + osc8FileLink(s.root, saved, filepath.Base(saved))
	}
	s.toolLine(true, verb, label+" · "+vp.Name, status, false)

	var blocks []wire.Block
	head := fmt.Sprintf("%s at %s %dx%d. Page is %dpx tall", target, vp.Name, vp.Width, vp.Height, look.PageHeight)
	if look.Title != "" {
		head += fmt.Sprintf(", title %q", look.Title)
	}
	head += "."
	if last := len(look.Screens); last > 0 {
		end := look.Screens[last-1].Top + vp.Height
		if end < look.PageHeight {
			head += fmt.Sprintf(" Shown down to %dpx; look again with from:%d to see more.", end, end)
		}
	}
	if len(look.Errors) > 0 {
		head += "\nConsole errors:\n  " + strings.Join(look.Errors, "\n  ")
	}
	if err != nil {
		head += "\nStopped early: " + err.Error()
	}
	blocks = append(blocks, wire.TextBlock(head))
	for _, sc := range look.Screens {
		blocks = append(blocks, wire.TextBlock(fmt.Sprintf("Screen at %d–%dpx:", sc.Top, sc.Top+vp.Height)), wire.ImageBlock(sc.Mime, sc.Image))
	}
	return toolResult{blocks: blocks}
}

// saveLook writes a capture's screens where the user can open them (the
// site's gitignored .memcode/looks) and returns the folder, "" on failure.
func (s *Session) saveLook(label, vp string, look sitelook.Look) string {
	w := s.site
	w.mu.Lock()
	w.looks += len(look.Screens)
	w.mu.Unlock()
	if len(look.Screens) == 0 {
		return ""
	}
	name := strings.Trim(nonFileChars.Replace(label), "-")
	if name == "" {
		name = "home"
	}
	dir := filepath.Join(w.lookDir, time.Now().Format("150405")+"-"+name+"-"+vp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	for i, sc := range look.Screens {
		ext := ".png"
		if sc.Mime == "image/jpeg" {
			ext = ".jpg"
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d%s", i+1, ext)), sc.Image, 0o644); err != nil {
			return ""
		}
	}
	return dir
}

var nonFileChars = strings.NewReplacer("/", "-", ":", "-", "?", "-", "&", "-", "=", "-", " ", "-", ".", "-")

func (s *Session) siteAddPackage(ctx context.Context, pkg string) toolResult {
	pkg = strings.TrimSpace(pkg)
	if pkg == "" {
		return errResult("add_package needs a package name")
	}
	if ok, reason := s.gate(ctx, permissions.Medium, false, ApprovalRequest{
		Title: "Add package", Label: "Add package", Detail: pkg + " to " + filepath.Base(filepath.Dir(s.site.dir)),
		Risk: permissions.Medium.String(),
	}); !ok {
		return errResult("adding " + pkg + " was denied: " + reason)
	}
	v, err := websites.Add(ctx, s.site.dir, pkg, "", s.site.siteKit(ctx), s.site.reg)
	switch {
	case err != nil:
		s.toolLine(true, "Package", "add "+pkg, "failed", true)
		return errResult(err.Error())
	case !v.OK:
		s.toolLine(true, "Package", "add "+pkg, "refused", true)
		return errResult(v.Summary() + ". Build it with the kit, or choose a package that meets the policy.")
	}
	s.toolLine(true, "Package", "add "+v.Name+"@"+v.Version, "", false)
	return textResult("Installed " + v.Summary() + ". The dev server picks it up; import it normally.")
}

// siteCheck answers "will this build and publish on memcode.ai?": the
// dependency policy, then the production build and its output rules.
func (s *Session) siteCheck(ctx context.Context) toolResult {
	report, ok := s.site.verify(ctx)
	status := "ok"
	if !ok {
		status = "problems"
	}
	s.toolLine(true, "Check", "production build", status, !ok)
	s.markVerified(ok)
	if !ok {
		return errResult(report)
	}
	return textResult(report)
}

// verify runs the dependency policy and the production build; ok reports
// whether the site would build and publish as it is.
func (w *siteWork) verify(ctx context.Context) (string, bool) {
	var b strings.Builder
	ok := true
	verdicts, err := websites.Check(ctx, w.dir, w.siteKit(ctx), w.reg)
	switch bad := websites.Problems(verdicts); {
	case err != nil:
		ok = false
		b.WriteString("Couldn't check dependencies: " + err.Error() + "\n")
	case len(bad) > 0:
		ok = false
		b.WriteString("Dependencies memcode.ai won't install:\n")
		for _, v := range bad {
			b.WriteString("- " + v.Summary() + "\n")
		}
	}
	r := siteBuild(ctx, w.dir)
	if !r.OK {
		ok = false
	}
	b.WriteString(r.Summary())
	return strings.TrimSpace(b.String()), ok
}
