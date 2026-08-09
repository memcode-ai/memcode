package browser

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestBrowserNavigateClickScreenshotEval launches a real Chrome (via chromedp),
// drives a local test server, and asserts the core tool operations work.
// Gated behind CHROME_TEST=1 so CI without Chrome skips it.
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

	sess, err := New() // visible window (headed)
	if err != nil {
		t.Skipf("Chrome not available: %v", err)
	}
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

	// Screenshot — assert PNG magic bytes.
	png, err := sess.Screenshot(ctx, false)
	if err != nil {
		t.Fatalf("Screenshot failed: %v", err)
	}
	if len(png) < 4 || !bytes.HasPrefix(png, []byte{0x89, 0x50, 0x4E, 0x47}) {
		t.Errorf("screenshot is not a PNG, first bytes: % x", png[:min(4, len(png))])
	}

	// Eval — a simple expression.
	result, err := sess.Eval(ctx, "1+1")
	if err != nil {
		t.Fatalf("Eval failed: %v", err)
	}
	if strings.TrimSpace(result) != "2" {
		t.Errorf("Eval(1+1) = %q, want \"2\"", result)
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

	sess, err := New()
	if err != nil {
		t.Skipf("Chrome not available: %v", err)
	}
	defer sess.Close()

	if err := sess.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("Navigate failed: %v", err)
	}

	// Scroll down — should move the page.
	if err := sess.Scroll(ctx, 0, 500); err != nil {
		t.Fatalf("Scroll failed: %v", err)
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

	sess, err := New()
	if err != nil {
		t.Skipf("Chrome not available: %v", err)
	}
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

	// Switch back to tab 1 — its content should be page1.
	if _, _, err := sess.SwitchTab(1); err != nil {
		t.Fatalf("SwitchTab(1) failed: %v", err)
	}
	text, _ := sess.Text(ctx)
	if !strings.Contains(text, "/page1") {
		t.Errorf("tab 1 text should contain /page1, got %q", text)
	}

	// List tabs — should show 2.
	tabs := sess.ListTabs()
	if len(tabs) != 2 {
		t.Errorf("ListTabs returned %d, want 2", len(tabs))
	}

	// Close tab 2 — should leave 1 tab.
	if err := sess.CloseTab(2); err != nil {
		t.Fatalf("CloseTab(2) failed: %v", err)
	}
	if sess.TabCount() != 1 {
		t.Errorf("after closing, expected 1 tab, got %d", sess.TabCount())
	}

	// Can't close the last tab.
	if err := sess.CloseTab(1); err == nil {
		t.Errorf("CloseTab on last tab should error")
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

	sess, err := New()
	if err != nil {
		t.Skipf("Chrome not available: %v", err)
	}

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
	sess2, err := New()
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
