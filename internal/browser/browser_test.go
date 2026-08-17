package browser

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestBrowserNavigateClickScreenshotEval launches a real Chrome (via chromedp),
// drives a local test server, and asserts the core tool operations work.
// Gated behind CHROME_TEST=1 so CI without Chrome skips it.
// newTestSession launches Chrome headless (CI has no display) or skips when
// Chrome isn't available.
func newTestSession(t *testing.T) *Session {
	t.Helper()
	sess, err := New(Options{Headless: true})
	if err != nil {
		t.Skipf("Chrome not available: %v", err)
	}
	return sess
}

func TestBrowserNavigateClickScreenshotEval(t *testing.T) {
	if os.Getenv("CHROME_TEST") != "1" {
		t.Skip("skipping browser test (set CHROME_TEST=1 to run)")
	}

	// A tiny page with a button that adds text when clicked.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><body>
<h1 id="title">Hello Browser</h1>
<button id="btn">Click me</button>
<p id="result" style="display:none">clicked!</p>
<script>document.getElementById("btn").onclick=function(){
document.getElementById("result").style.display="block";};</script>
</body></html>`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess := newTestSession(t)
	defer sess.Close()

	// Navigate to the test page.
	if err := sess.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("Navigate failed: %v", err)
	}

	// Read the page text — should contain the title.
	text, err := sess.Text(ctx)
	if err != nil {
		t.Fatalf("Text failed: %v", err)
	}
	if !strings.Contains(text, "Hello Browser") {
		t.Errorf("page text missing title, got: %q", text)
	}

	// Click the button — the hidden result should appear.
	if err := sess.Click(ctx, "#btn"); err != nil {
		t.Fatalf("Click failed: %v", err)
	}
	text, _ = sess.Text(ctx)
	if !strings.Contains(text, "clicked!") {
		t.Errorf("after click, page text missing result, got: %q", text)
	}

	// Screenshot — assert PNG magic bytes and declared mime.
	img, mime, err := sess.Screenshot(ctx, false)
	if err != nil {
		t.Fatalf("Screenshot failed: %v", err)
	}
	if mime == "image/png" && (len(img) < 4 || !bytes.HasPrefix(img, []byte{0x89, 0x50, 0x4E, 0x47})) {
		t.Errorf("screenshot declared PNG but isn't, first bytes: % x", img[:min(4, len(img))])
	}

	// Eval — every result shape it advertises: numbers, strings, objects,
	// undefined, and statements.
	for _, c := range []struct{ js, want string }{
		{"1+1", "2"},
		{"document.title", ""}, // no <title> on the page — empty string
		{"({a: 1})", `{"a":1}`},
		{"[1,2]", "[1,2]"},
		{"undefined", "undefined"},
		{"var x = 5; x * 2", "undefined"}, // statement body without return
		{"let y = 7; return y * 3", "21"}, // statement body with return
	} {
		got, err := sess.Eval(ctx, c.js)
		if err != nil {
			t.Errorf("Eval(%q) failed: %v", c.js, err)
			continue
		}
		if strings.TrimSpace(got) != c.want {
			t.Errorf("Eval(%q) = %q, want %q", c.js, got, c.want)
		}
	}
}

// TestBrowserDialogsAndWait: JS dialogs are auto-handled (they must never hang
// the tab), their text lands in the console buffer, and browser_wait settles
// async DOM changes.
func TestBrowserDialogsAndWait(t *testing.T) {
	if os.Getenv("CHROME_TEST") != "1" {
		t.Skip("skipping browser test (set CHROME_TEST=1 to run)")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><body>
<button id="alert" onclick="alert('hi there')">alert</button>
<button id="confirm" onclick="document.getElementById('answer').textContent = confirm('sure?') ? 'yes' : 'no'">confirm</button>
<p id="answer"></p>
<button id="later" onclick="setTimeout(function(){var p=document.createElement('p');p.id='appeared';p.textContent='late content';document.body.appendChild(p)}, 300)">later</button>
</body></html>`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sess := newTestSession(t)
	defer sess.Close()

	if err := sess.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	// alert() must not hang the tab; it is auto-accepted.
	if err := sess.Click(ctx, "#alert"); err != nil {
		t.Fatalf("Click #alert: %v", err)
	}
	// The tab must still be responsive.
	if _, err := sess.Text(ctx); err != nil {
		t.Fatalf("tab wedged after alert: %v", err)
	}
	// confirm() is auto-dismissed → the page sees "no".
	if err := sess.Click(ctx, "#confirm"); err != nil {
		t.Fatalf("Click #confirm: %v", err)
	}
	if err := sess.WaitFor(ctx, "#answer", "visible", 5*time.Second); err != nil {
		t.Fatalf("WaitFor #answer: %v", err)
	}
	ans, _ := sess.Eval(ctx, "document.getElementById('answer').textContent")
	if strings.TrimSpace(ans) != "no" {
		t.Errorf("confirm should be auto-dismissed (no), got %q", ans)
	}
	time.Sleep(200 * time.Millisecond)
	dialogs := strings.Join(sess.Console("dialog"), "\n")
	if !strings.Contains(dialogs, "hi there") || !strings.Contains(dialogs, "sure?") {
		t.Errorf("dialog text must land in the console buffer, got: %q", dialogs)
	}
	// browser_wait settles async content.
	if err := sess.Click(ctx, "#later"); err != nil {
		t.Fatalf("Click #later: %v", err)
	}
	if err := sess.WaitFor(ctx, "#appeared", "visible", 5*time.Second); err != nil {
		t.Fatalf("WaitFor #appeared: %v", err)
	}
	// And a wait that can't succeed times out with a clear error, fast.
	if err := sess.WaitFor(ctx, "#never", "visible", 1*time.Second); err == nil {
		t.Error("WaitFor on a missing selector must time out with an error")
	}
}

// TestBrowserTypeUploadResize: type clears by default and appends on request;
// upload sets a file input; resize changes the viewport.
func TestBrowserTypeUploadResize(t *testing.T) {
	if os.Getenv("CHROME_TEST") != "1" {
		t.Skip("skipping browser test (set CHROME_TEST=1 to run)")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><body>
<input id="inp" type="text" value="OLD">
<input id="file" type="file">
</body></html>`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sess := newTestSession(t)
	defer sess.Close()

	if err := sess.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	// Default: clears the existing value.
	if err := sess.Type(ctx, "#inp", "new", false); err != nil {
		t.Fatalf("Type: %v", err)
	}
	if v, _ := sess.Eval(ctx, "document.getElementById('inp').value"); strings.TrimSpace(v) != "new" {
		t.Errorf("Type must clear first: value = %q, want new", v)
	}
	// Append keeps the value.
	if err := sess.Type(ctx, "#inp", "er", true); err != nil {
		t.Fatalf("Type append: %v", err)
	}
	if v, _ := sess.Eval(ctx, "document.getElementById('inp').value"); strings.TrimSpace(v) != "newer" {
		t.Errorf("Type append: value = %q, want newer", v)
	}
	// Upload a real temp file.
	f := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(f, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sess.Upload(ctx, "#file", []string{f}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if n, _ := sess.Eval(ctx, "document.getElementById('file').files.length"); strings.TrimSpace(n) != "1" {
		t.Errorf("Upload: files.length = %q, want 1", n)
	}
	// Resize the viewport.
	if err := sess.Resize(ctx, 390, 844); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if w, _ := sess.Eval(ctx, "window.innerWidth"); strings.TrimSpace(w) != "390" {
		t.Errorf("Resize: innerWidth = %q, want 390", w)
	}
	if err := sess.Resize(ctx, 10, 10); err == nil {
		t.Error("Resize below bounds must be rejected")
	}
}

// TestBrowserScrollHoverSelectPress launches Chrome and drives a richer page
// that exercises the interaction primitives added in the "full browser" build:
// scroll, hover (CSS :hover state), select (dropdown), press_key (Enter on a
// form), and console capture. Gated behind CHROME_TEST=1.
func TestBrowserScrollHoverSelectPress(t *testing.T) {
	if os.Getenv("CHROME_TEST") != "1" {
		t.Skip("skipping browser test (set CHROME_TEST=1 to run)")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><body>
<div id="spacer" style="height:2000px;background:#eee">scroll past me</div>
<div id="hover-target" onmouseover="this.style.color='red'">hover me</div>
<select id="dd"><option value="a">A</option><option value="b">B</option></select>
<form id="form" onsubmit="event.preventDefault();document.getElementById('submitted').style.display='block'">
<input id="inp" type="text"><button type="submit">submit</button></form>
<p id="submitted" style="display:none">submitted!</p>
<script>console.log('hello console');console.error('oops');</script>
</body></html>`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess := newTestSession(t)
	defer sess.Close()

	if err := sess.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("Navigate failed: %v", err)
	}

	// Scroll down — should move the page and report the position.
	pos, err := sess.Scroll(ctx, 0, 500)
	if err != nil {
		t.Fatalf("Scroll failed: %v", err)
	}
	if !strings.Contains(pos, "scrolled to") {
		t.Errorf("Scroll must report the resulting position, got %q", pos)
	}

	// Hover — should turn the target red.
	if err := sess.Hover(ctx, "#hover-target"); err != nil {
		t.Fatalf("Hover failed: %v", err)
	}
	color, _ := sess.Eval(ctx, "getComputedStyle(document.getElementById('hover-target')).color")
	if !strings.Contains(color, "rgb(255, 0, 0)") && !strings.Contains(color, "255, 0, 0") {
		t.Errorf("hover didn't turn target red, color=%q", color)
	}

	// Select — should set the dropdown value.
	if err := sess.Select(ctx, "#dd", "b"); err != nil {
		t.Fatalf("Select failed: %v", err)
	}
	val, _ := sess.Eval(ctx, "document.getElementById('dd').value")
	if strings.TrimSpace(val) != "b" {
		t.Errorf("Select didn't set value to b, got %q", val)
	}

	// Press Enter on the form after focusing the input.
	if err := sess.Click(ctx, "#inp"); err != nil {
		t.Fatalf("Click #inp failed: %v", err)
	}
	if err := sess.PressKey(ctx, "Enter"); err != nil {
		t.Fatalf("PressKey failed: %v", err)
	}
	text, _ := sess.Text(ctx)
	if !strings.Contains(text, "submitted!") {
		t.Errorf("Enter didn't submit the form; text=%q", text)
	}

	// Console — should capture the console.log and console.error.
	// Give the listener a moment to process events.
	time.Sleep(200 * time.Millisecond)
	logs := sess.Console("")
	allLogs := strings.Join(logs, "\n")
	if !strings.Contains(allLogs, "hello console") {
		t.Errorf("console didn't capture console.log('hello console'); got: %q", allLogs)
	}
	if !strings.Contains(allLogs, "oops") {
		t.Errorf("console didn't capture console.error('oops'); got: %q", allLogs)
	}
}

// TestBrowserTabs exercises multi-tab management: open a new tab, switch
// between tabs, list them, and close one. Gated behind CHROME_TEST=1.
func TestBrowserTabs(t *testing.T) {
	if os.Getenv("CHROME_TEST") != "1" {
		t.Skip("skipping browser test (set CHROME_TEST=1 to run)")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fmt.Sprintf("<html><body><h1>%s</h1></body></html>", r.URL.Path)))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess := newTestSession(t)
	defer sess.Close()

	if err := sess.Navigate(ctx, srv.URL+"/page1"); err != nil {
		t.Fatalf("Navigate page1 failed: %v", err)
	}
	if sess.TabCount() != 1 {
		t.Errorf("expected 1 tab, got %d", sess.TabCount())
	}

	// Open a new tab — should be tab 2 and active.
	idx, err := sess.NewTab(ctx)
	if err != nil {
		t.Fatalf("NewTab failed: %v", err)
	}
	if idx != 2 {
		t.Errorf("NewTab returned %d, want 2", idx)
	}
	if sess.TabCount() != 2 {
		t.Errorf("expected 2 tabs, got %d", sess.TabCount())
	}

	// Navigate in the new tab.
	if err := sess.Navigate(ctx, srv.URL+"/page2"); err != nil {
		t.Fatalf("Navigate page2 failed: %v", err)
	}

	// Switch back to tab 1 — live url/title, and its content is page1.
	url, _, err := sess.SwitchTab(ctx, 1)
	if err != nil {
		t.Fatalf("SwitchTab(1) failed: %v", err)
	}
	if !strings.Contains(url, "/page1") {
		t.Errorf("SwitchTab must report the LIVE url, got %q", url)
	}
	text, _ := sess.Text(ctx)
	if !strings.Contains(text, "/page1") {
		t.Errorf("tab 1 text should contain /page1, got %q", text)
	}

	// List tabs — live urls, active marker on tab 1.
	tabs := sess.ListTabs(ctx)
	if len(tabs) != 2 {
		t.Fatalf("ListTabs returned %d, want 2", len(tabs))
	}
	if !tabs[0].Active || tabs[0].Index != 1 || !strings.Contains(tabs[0].URL, "/page1") {
		t.Errorf("tab 1 listing wrong: %+v", tabs[0])
	}
	if !strings.Contains(tabs[1].URL, "/page2") {
		t.Errorf("tab 2 must list its LIVE url, got %+v", tabs[1])
	}

	// Open a third tab (active), then close it: the ACTIVE tab close must
	// deterministically activate the nearest lower index.
	if _, err := sess.NewTab(ctx); err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	newActive, err := sess.CloseTab(3)
	if err != nil {
		t.Fatalf("CloseTab(3): %v", err)
	}
	if newActive != 2 {
		t.Errorf("closing the active tab 3 must activate tab 2, got %d", newActive)
	}

	// Close tab 2 — the REAL Chrome tab must close too (target count drops).
	if _, err := sess.CloseTab(2); err != nil {
		t.Fatalf("CloseTab(2) failed: %v", err)
	}
	if sess.TabCount() != 1 {
		t.Errorf("after closing, expected 1 tab, got %d", sess.TabCount())
	}

	// Tab 1 hosts the session and can't be closed; nor can the last tab.
	if _, err := sess.CloseTab(1); err == nil {
		t.Errorf("CloseTab on tab 1 / last tab should error")
	}
}

// TestBrowserAliveRecovery verifies the dead-Chrome recovery path: after the
// Chrome process is killed, Alive() must report false (so browserOrInit can
// relaunch instead of serving a corpse forever). Then a fresh New() must
// produce a working session again. Gated behind CHROME_TEST=1 — it launches a
// real Chrome and kills it.
func TestBrowserAliveRecovery(t *testing.T) {
	if os.Getenv("CHROME_TEST") != "1" {
		t.Skip("skipping browser test (set CHROME_TEST=1 to run)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess := newTestSession(t)

	// A live session should report alive and actually work.
	if !sess.Alive() {
		t.Fatal("freshly launched session reports not alive")
	}
	if err := sess.Navigate(ctx, "about:blank"); err != nil {
		t.Fatalf("Navigate on live session failed: %v", err)
	}

	// Kill the Chrome process — simulate the user quitting the window / a crash.
	c := chromedp.FromContext(sess.browserCtx)
	if c == nil || c.Browser == nil {
		t.Fatal("no chromedp browser context to kill")
	}
	proc := c.Browser.Process()
	if proc == nil {
		t.Fatal("chromedp browser has no OS process")
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill Chrome process: %v", err)
	}

	// LostConnection is closed asynchronously when the websocket drops; give it
	// a moment. Alive() should flip false once Chrome is gone.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !sess.Alive() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sess.Alive() {
		t.Fatal("Alive() still true after Chrome process was killed — recovery would never trigger")
	}
	// A dead session's Navigate must fail (this is what was returning
	// "context canceled" to the agent before the fix).
	if err := sess.Navigate(ctx, "about:blank"); err == nil {
		t.Fatal("Navigate on dead session unexpectedly succeeded")
	}
	sess.Close()

	// Recovery: a fresh New() launches a new Chrome that works again.
	sess2, err := New(Options{Headless: true})
	if err != nil {
		t.Fatalf("recovery New() failed: %v", err)
	}
	defer sess2.Close()
	if !sess2.Alive() {
		t.Fatal("recovered session reports not alive")
	}
	if err := sess2.Navigate(ctx, "about:blank"); err != nil {
		t.Fatalf("Navigate on recovered session failed: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
