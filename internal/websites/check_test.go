package websites

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Only runtime LOADS from other domains are problems; links and metadata
// pointing elsewhere are fine.
func TestCheckDistExternalLoads(t *testing.T) {
	dist := t.TempDir()
	writeFile(t, filepath.Join(dist, "index.html"), `<!doctype html><html><head>
<link rel="canonical" href="https://elventech.memcode.app/">
<meta property="og:url" content="https://elventech.memcode.app/">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Inter">
<link rel="stylesheet" href="/assets/app.css">
</head><body>
<a href="https://linkedin.com/company/x">LinkedIn</a>
<img src="/assets/logo.svg" srcset="/a.png 1x, https://cdn.example.com/b.png 2x">
<script src="https://www.googletagmanager.com/gtag.js"></script>
</body></html>`)
	writeFile(t, filepath.Join(dist, "about", "index.html"), `<html><body><p>ok</p></body></html>`)
	writeFile(t, filepath.Join(dist, "assets", "app.css"), `@import url("https://fonts.googleapis.com/x.css");body{background:url(/bg.png)}.x{background:url('https://images.example.com/hero.jpg')}`)
	r := checkDist(dist)
	if r.OK || r.Pages != 2 {
		t.Fatalf("report %+v", r)
	}
	all := strings.Join(r.Problems, "\n")
	for _, want := range []string{"fonts.googleapis.com/css2", "googletagmanager", "cdn.example.com/b.png", "fonts.googleapis.com/x.css", "images.example.com/hero.jpg"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %s in:\n%s", want, all)
		}
	}
	for _, notWant := range []string{"linkedin", "elventech.memcode.app", "/bg.png"} {
		if strings.Contains(all, notWant) {
			t.Errorf("%s is not a runtime load:\n%s", notWant, all)
		}
	}
}

func TestCheckDistNeedsAHomePage(t *testing.T) {
	dist := t.TempDir()
	writeFile(t, filepath.Join(dist, "about", "index.html"), `<p>hi</p>`)
	if r := checkDist(dist); r.OK || !strings.Contains(r.Summary(), "home page") {
		t.Fatalf("%+v", r)
	}
	writeFile(t, filepath.Join(dist, "index.html"), `<p>hi</p>`)
	if r := checkDist(dist); !r.OK || !strings.Contains(r.Summary(), "Production build OK") {
		t.Fatalf("%+v", r)
	}
}
