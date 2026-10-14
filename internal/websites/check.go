// check.go — will this site build on memcode.ai? Answered here, before a push.
//
// The local dev server renders in the browser; the hosted build pre-renders
// every route in Node (vite-react-ssg). Code that touches window at import
// time, a missing package, or a broken route works in the preview and fails
// the build. BuildCheck runs the same `npm run build` the builder runs and
// applies the same output rules, so the user finds out now, not after a push.
package websites

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// MaxSiteBytes is the hosted builder's cap on a site's built output.
const MaxSiteBytes int64 = 50 << 20

// BuildReport is the outcome of a local production build.
type BuildReport struct {
	OK       bool
	Pages    int
	Bytes    int64
	Problems []string // what must be fixed before the site will build/serve
	Output   string   // the build's output tail, when it failed
}

// Summary describes the report in a few lines.
func (r BuildReport) Summary() string {
	if r.OK {
		return fmt.Sprintf("Production build OK: %d page(s), %.1f MB.", r.Pages, float64(r.Bytes)/(1<<20))
	}
	var b strings.Builder
	b.WriteString("The production build has problems:\n")
	for _, p := range r.Problems {
		b.WriteString("- " + p + "\n")
	}
	if r.Output != "" {
		b.WriteString("\nBuild output (tail):\n" + r.Output)
	}
	return strings.TrimSpace(b.String())
}

// BuildCheck runs the site's production build into dist/ (gitignored) and
// checks what it produced.
func BuildCheck(ctx context.Context, dir string) BuildReport {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "npm", "run", "build")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "FORCE_COLOR=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return BuildReport{Problems: []string{"`npm run build` failed (this is the build memcode.ai runs)"}, Output: lastLines(string(out), 40)}
	}
	return checkDist(filepath.Join(dir, "dist"))
}

// checkDist applies the builder's output rules to a built dist/.
func checkDist(dist string) BuildReport {
	r := BuildReport{}
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err != nil {
		r.Problems = append(r.Problems, "dist/index.html is missing: the site has no home page")
	}
	seen := map[string]bool{}
	_ = filepath.WalkDir(dist, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a vanished file is not a site problem
		}
		if fi, err := d.Info(); err == nil {
			r.Bytes += fi.Size()
		}
		rel, _ := filepath.Rel(dist, path)
		var refs []string
		switch strings.ToLower(filepath.Ext(path)) {
		case ".html":
			r.Pages++
			refs = externalHTMLRefs(path)
		case ".css":
			refs = externalCSSRefs(path)
		}
		for _, u := range refs {
			if !seen[u] && len(seen) < 8 {
				seen[u] = true
				r.Problems = append(r.Problems, fmt.Sprintf("%s loads %s at runtime; published sites can't load from other domains (ship it in the project instead)", rel, u))
			}
		}
		return nil
	})
	if r.Bytes > MaxSiteBytes {
		r.Problems = append(r.Problems, fmt.Sprintf("the built site is %.1f MB; the limit is %d MB", float64(r.Bytes)/(1<<20), MaxSiteBytes>>20))
	}
	r.OK = len(r.Problems) == 0
	return r
}

// loadAttrs are the attributes through which a page LOADS something at
// runtime. Links to other sites (a, canonical, og:url) are navigation and
// metadata, not loads, and are fine.
var loadAttrs = map[string][]string{
	"script": {"src"},
	"img":    {"src", "srcset"},
	"source": {"src", "srcset"},
	"video":  {"src", "poster"},
	"audio":  {"src"},
	"iframe": {"src"},
	"embed":  {"src"},
	"object": {"data"},
}

// loadingRels are <link rel> values that fetch the href.
var loadingRels = map[string]bool{
	"stylesheet": true, "preload": true, "modulepreload": true, "icon": true,
	"apple-touch-icon": true, "manifest": true, "prefetch": true,
}

func externalHTMLRefs(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	z := html.NewTokenizer(f)
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return out
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		t := z.Token()
		attrs := map[string]string{}
		for _, a := range t.Attr {
			attrs[strings.ToLower(a.Key)] = a.Val
		}
		if t.Data == "link" {
			for _, rel := range strings.Fields(strings.ToLower(attrs["rel"])) {
				if loadingRels[rel] && isExternal(attrs["href"]) {
					out = append(out, attrs["href"])
					break
				}
			}
			continue
		}
		for _, name := range loadAttrs[t.Data] {
			for _, u := range urlsIn(name, attrs[name]) {
				if isExternal(u) {
					out = append(out, u)
				}
			}
		}
	}
}

// urlsIn splits a srcset into its URLs; any other attribute is one URL.
func urlsIn(attr, val string) []string {
	if attr != "srcset" {
		return []string{strings.TrimSpace(val)}
	}
	var out []string
	for _, cand := range strings.Split(val, ",") {
		if f := strings.Fields(cand); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

// externalCSSRefs finds url(...) and @import targets on other domains.
func externalCSSRefs(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	css := string(raw)
	var out []string
	for _, marker := range []string{"url(", "@import"} {
		rest := css
		for {
			i := strings.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			arg := strings.TrimLeft(rest, " \t\"'(")
			if strings.HasPrefix(arg, "url(") {
				arg = strings.TrimLeft(arg[4:], " \t\"'")
			}
			end := strings.IndexAny(arg, "\"') ;\n")
			if end < 0 {
				end = len(arg)
			}
			if u := arg[:end]; isExternal(u) {
				out = append(out, u)
			}
		}
	}
	return out
}

func isExternal(u string) bool {
	u = strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "//")
}
