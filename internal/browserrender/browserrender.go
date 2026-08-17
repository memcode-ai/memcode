// Package browserrender is memcode's OPTIONAL local browser-render capability: the
// last-resort fetch tier for JavaScript-rendered pages that raw GET and the
// server-side web_fetch (both no-JS) cannot read. It is NOT bundled — it drives a
// Chrome/Chromium already on the machine via headless --dump-dom (no Node, no new
// dependency). A managed, on-demand-downloaded browser can be added later under
// ~/.memcode/tools/browser-render without changing callers.
//
// Rendering executes the page's JavaScript locally, so callers MUST gate it behind
// explicit user consent — use only for pages the user trusts.
package browserrender

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/chromedp/chromedp"
)

// ManagedDir is where an on-demand-installed browser would live (future installer).
func ManagedDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".memcode", "tools", "browser-render")
	}
	return ".memcode/tools/browser-render"
}

// Find returns the path to a usable Chrome/Chromium and whether one was found. It
// checks a managed install first, then PATH, then the OS's standard locations.
func Find() (string, bool) {
	// CHROME_PATH pins the browser binary explicitly — highest priority, and an
	// invalid value falls through to discovery rather than silently winning.
	if p := os.Getenv("CHROME_PATH"); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	// A managed install (future `memcode capabilities install browser-render`).
	for _, p := range managedCandidates() {
		if isExecutable(p) {
			return p, true
		}
	}
	names := []string{
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome",
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, true
		}
	}
	for _, p := range osCandidates() {
		if isExecutable(p) {
			return p, true
		}
	}
	return "", false
}

// Available reports whether a browser renderer is usable right now.
func Available() bool {
	_, ok := Find()
	return ok
}

// Render loads url in headless Chrome via the DevTools protocol — executing the
// page's JavaScript and WAITING for the client render to settle — and returns the
// rendered DOM HTML. (A plain `--dump-dom` captures the empty shell before SPA
// content loads, so we drive it properly with chromedp.) The caller must obtain
// user consent first, since this runs arbitrary page JS locally.
func Render(ctx context.Context, url string) (string, error) {
	path, ok := Find()
	if !ok {
		return "", errNoBrowser
	}
	// Render like a REAL Chrome so sites that gate content on "is this a bot?" serve
	// it. Three levers, in order of impact:
	//  1. --headless=new (Chrome 109+): the full renderer with no window — nearly
	//     indistinguishable from headful, unlike legacy --headless which sites detect
	//     trivially (this is THE fix when headful works but headless is blocked).
	//  2. a real Chrome UA (the default contains "HeadlessChrome" — an instant tell).
	//  3. disable AutomationControlled (hides navigator.webdriver).
	// This defeats common detection, NOT hard Cloudflare-style protection — for that we
	// fall back honestly (the caller reports "rendered but no readable content").
	const ua = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(path),
		chromedp.Flag("headless", "new"), // overrides the default's legacy --headless
		chromedp.DisableGPU,
		chromedp.UserAgent(ua),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("lang", "en-US,en"),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()
	bctx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()
	tctx, cancelTimeout := context.WithTimeout(bctx, 60*time.Second)
	defer cancelTimeout()

	// Poll until the body has real text (client render settled), capped so a slow or
	// uncooperative SPA can't hang the turn.
	settle := chromedp.ActionFunc(func(ctx context.Context) error {
		deadline := time.Now().Add(10 * time.Second)
		for {
			var n int
			if err := chromedp.Evaluate(`(document.body&&document.body.innerText)?document.body.innerText.trim().length:0`, &n).Do(ctx); err != nil {
				return err
			}
			if n > 200 || time.Now().After(deadline) {
				return nil
			}
			time.Sleep(300 * time.Millisecond)
		}
	})

	var html string
	err := chromedp.Run(tctx,
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),
		settle,
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	)
	if err != nil {
		return "", err
	}
	return html, nil
}

type browserErr string

func (e browserErr) Error() string { return string(e) }

const errNoBrowser = browserErr("no Chrome/Chromium found — install one or run `memcode capabilities install browser-render`")

func isExecutable(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func managedCandidates() []string {
	d := ManagedDir()
	return []string{
		filepath.Join(d, "chrome"),
		filepath.Join(d, "chromium"),
		filepath.Join(d, "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
		filepath.Join(d, "chrome-headless-shell"),
	}
}

func osCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
		}
	case "windows":
		var c []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if base := os.Getenv(env); base != "" {
				c = append(c, filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
		return c
	default: // linux & friends
		return []string{
			"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable",
			"/usr/bin/chromium", "/usr/bin/chromium-browser", "/snap/bin/chromium",
			"/usr/local/bin/chrome",
		}
	}
}
