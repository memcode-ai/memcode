// Package sitelook lets a website worker SEE pages: a reference the user
// pointed at, or the site it is building. Capture scrolls through a rendered
// page one viewport at a time (so scroll-triggered reveals play, as they do
// for a visitor) and returns each screen as an image; Inspect measures the
// rendered page's design system (type scale, palette, layout rhythm,
// sections, motion) from computed styles, which is what a designer reads off
// a page and what grepping its HTML and CSS can't recover.
//
// Both run memcode's own fixed scripts in an ephemeral headless Chrome; no
// model-written code runs in the page.
package sitelook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Browser is the slice of browser.Session this package drives.
type Browser interface {
	Emulate(ctx context.Context, width, height int, mobile bool) error
	Navigate(ctx context.Context, url string) error
	Eval(ctx context.Context, js string) (string, error)
	Screenshot(ctx context.Context, fullPage bool) ([]byte, string, error)
	ClearConsole()
	Console(level string) []string
}

// Viewport is a device size to render at.
type Viewport struct {
	Name          string
	Width, Height int
	Mobile        bool
}

// Viewports are the sizes a site is judged at.
var Viewports = []Viewport{
	{Name: "desktop", Width: 1440, Height: 900},
	{Name: "laptop", Width: 1280, Height: 800},
	{Name: "tablet", Width: 834, Height: 1112, Mobile: true},
	{Name: "mobile", Width: 390, Height: 844, Mobile: true},
}

// ViewportNamed finds a viewport by name ("" is desktop).
func ViewportNamed(name string) (Viewport, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = "desktop"
	}
	for _, v := range Viewports {
		if v.Name == name {
			return v, true
		}
	}
	return Viewport{}, false
}

// Timing knobs; tests shrink them.
var (
	loadSettle   = 1500 * time.Millisecond // entrance animations after load
	scrollSettle = 900 * time.Millisecond  // scroll-triggered reveals after each scroll
	fontWait     = 5 * time.Second
)

// MaxScreens bounds one capture.
const MaxScreens = 8

// Screen is one viewport-sized capture.
type Screen struct {
	Top   int // page offset (CSS px) of the screen's top edge
	Image []byte
	Mime  string
}

// Look is a captured page.
type Look struct {
	URL        string
	Title      string
	Viewport   Viewport
	PageHeight int
	Screens    []Screen
	Errors     []string // console errors and uncaught exceptions while loading
}

// load renders url at vp and waits for fonts and the opening animation.
func load(ctx context.Context, b Browser, url string, vp Viewport) error {
	if err := b.Emulate(ctx, vp.Width, vp.Height, vp.Mobile); err != nil {
		return err
	}
	b.ClearConsole()
	if err := b.Navigate(ctx, url); err != nil {
		return fmt.Errorf("couldn't open %s: %w", url, err)
	}
	deadline := time.Now().Add(fontWait)
	for time.Now().Before(deadline) {
		st, err := b.Eval(ctx, `document.fonts ? document.fonts.status : "loaded"`)
		if err != nil || st == "loaded" {
			break
		}
		if !sleep(ctx, 150*time.Millisecond) {
			return ctx.Err()
		}
	}
	if !sleep(ctx, loadSettle) {
		return ctx.Err()
	}
	return nil
}

// Capture renders url at vp and captures up to count screens starting at
// page offset from, scrolling between them.
func Capture(ctx context.Context, b Browser, url string, vp Viewport, from, count int) (Look, error) {
	if count <= 0 {
		count = 4
	}
	if count > MaxScreens {
		count = MaxScreens
	}
	if from < 0 {
		from = 0
	}
	if err := load(ctx, b, url, vp); err != nil {
		return Look{}, err
	}
	look := Look{URL: url, Viewport: vp}
	var m struct {
		Height int    `json:"h"`
		Title  string `json:"title"`
	}
	if raw, err := b.Eval(ctx, "(function(){"+scrollJS+"return JSON.stringify({h: __mc.height(), title: document.title});})()"); err == nil {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	look.Title, look.PageHeight = m.Title, m.Height
	last := -1
	for i := 0; i < count; i++ {
		want := from + i*vp.Height
		if i > 0 && look.PageHeight > 0 && want >= look.PageHeight {
			break
		}
		raw, err := b.Eval(ctx, "(function(){"+scrollJS+fmt.Sprintf("return String(__mc.scrollTo(%d));})()", want))
		if err != nil {
			return look, fmt.Errorf("scrolling the page: %w", err)
		}
		var top int
		fmt.Sscan(raw, &top)
		if top == last { // the page ends here; this screen would repeat the last
			break
		}
		last = top
		if !sleep(ctx, scrollSettle) {
			return look, ctx.Err()
		}
		img, mime, err := b.Screenshot(ctx, false)
		if err != nil {
			return look, fmt.Errorf("screenshot: %w", err)
		}
		look.Screens = append(look.Screens, Screen{Top: top, Image: img, Mime: mime})
	}
	look.Errors = consoleErrors(b)
	return look, nil
}

func consoleErrors(b Browser) []string {
	var out []string
	for _, lvl := range []string{"error", "exception"} {
		out = append(out, b.Console(lvl)...)
	}
	if len(out) > 10 {
		out = out[len(out)-10:]
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// scrollJS defines __mc: page height and scrolling that also works on pages
// whose content scrolls inside a container instead of the document.
const scrollJS = `var __mc = (function(){
  var se = document.scrollingElement || document.documentElement;
  function scroller(){
    if (se.scrollHeight > window.innerHeight + 4) return null;
    var best = null, bh = 0, els = document.body ? document.body.getElementsByTagName('*') : [];
    for (var i = 0; i < els.length && i < 4000; i++) {
      var el = els[i], cs = getComputedStyle(el);
      if ((cs.overflowY === 'auto' || cs.overflowY === 'scroll') && el.scrollHeight > el.clientHeight + 4 && el.clientHeight > window.innerHeight * 0.5 && el.scrollHeight > bh) { best = el; bh = el.scrollHeight; }
    }
    return best;
  }
  var box = scroller();
  return {
    height: function(){ return Math.round(box ? box.scrollHeight : Math.max(se.scrollHeight, document.body ? document.body.scrollHeight : 0)); },
    scrollTo: function(y){
      if (box) { box.style.scrollBehavior = 'auto'; box.scrollTop = y; return Math.round(box.scrollTop); }
      se.style.scrollBehavior = 'auto';
      window.scrollTo({top: y, left: 0, behavior: 'instant'}); return Math.round(window.scrollY);
    }
  };
})();
`
