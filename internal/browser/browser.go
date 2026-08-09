// Package browser drives a long-lived Chrome instance via the Chrome DevTools
// Protocol (chromedp) so the agent can fully interact with web pages as tools —
// the same dispatch as read_file/bash. It exposes the full browser interaction
// surface a user has: navigate, click, type, scroll, hover, keyboard, dropdowns,
// history, tabs, screenshots, console logs, and arbitrary JS.
//
// The Session wraps a persistent chromedp context created once (on New) and
// reused across tool calls — a single Chrome process for the whole agent
// session, torn down on Close. It tracks multiple tabs (each a child chromedp
// context) and a rolling console-log buffer captured via CDP Runtime events.
// It reuses browserrender.Find to locate the Chrome binary (no new dependency,
// no Node).
package browser

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/memcode-ai/memcode/internal/browserrender"
)

// maxConsoleEntries caps the rolling console buffer. The agent asks for console
// on demand (browser_console), and we keep the last N entries so a page that
// logs heavily doesn't grow the buffer unbounded.
const maxConsoleEntries = 200

// Session is a persistent Chrome instance controlled via CDP, with multi-tab
// support and a rolling console-log buffer.
type Session struct {
	allocCtx    context.Context
	cancelAlloc context.CancelFunc
	browserCtx  context.Context
	cancelBrow  context.CancelFunc

	mu      sync.Mutex
	tabs    []*tabHandle   // ordered list of open tabs; tabs[activeTab] is current
	active  int            // index into tabs
	console []consoleEntry // rolling buffer of console messages (from all tabs)
}

// tabHandle wraps a per-tab chromedp context so each tab can be driven
// independently. The first tab is created in New; NewTab spawns more.
type tabHandle struct {
	ctx    context.Context
	cancel context.CancelFunc
	title  string // last known title (for listing)
	url    string // last known URL (for listing)
}

// consoleEntry is one captured console message (console.log/error/warn, or an
// uncaught exception).
type consoleEntry struct {
	Tab   int    // which tab index it came from
	Level string // "log", "error", "warn", "info", "debug", "exception"
	Text  string // the message text (args joined)
	Time  time.Time
}

// New launches a Chrome instance with a visible window (headed) and returns a
// persistent Session with one open tab. You can watch Chrome work as the agent
// navigates, clicks, and types. The Chrome binary is discovered via
// browserrender.Find — a clear error is returned when none is available.
func New() (*Session, error) {
	path, ok := browserrender.Find()
	if !ok {
		return nil, errors.New("Chrome not found — install Google Chrome or set CHROME_PATH")
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(path),
		chromedp.Flag("headless", false), // visible window — headed by default
		chromedp.DisableGPU,
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
	)
	// --no-sandbox is needed on Linux CI / containers (Chrome refuses to start
	// as root without it). Harmless elsewhere.
	if goruntime.GOOS == "linux" {
		opts = append(opts, chromedp.NoSandbox)
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, cancelBrow := chromedp.NewContext(allocCtx)

	// Eagerly start the browser so a launch failure surfaces NOW (at New), not on
	// the first Navigate. chromedp.NewContext is lazy — Run triggers the launch.
	//
	// IMPORTANT: prime the browser with browserCtx itself, NOT a timeout-wrapped
	// child. In this chromedp version the first Run that launches the browser
	// binds the browser's CDP connection to the context it was launched under;
	// canceling that context (which a defer'd timeout would do the instant New
	// returns) tears the browser back down ~1s later. That was the root cause of
	// every browser tool returning "context canceled" shortly after launch: the
	// priming timeout's cancellation killed Chrome. To bound the launch wait
	// without owning the browser, run the priming action in a goroutine and wait
	// on a separate timer — the browser lives under browserCtx for the session.
	primed := make(chan error, 1)
	go func() {
		primed <- chromedp.Run(browserCtx)
	}()
	select {
	case err := <-primed:
		if err != nil {
			cancelBrow()
			cancelAlloc()
			return nil, fmt.Errorf("Chrome failed to start: %w", err)
		}
	case <-time.After(30 * time.Second):
		cancelBrow()
		cancelAlloc()
		return nil, errors.New("Chrome failed to start: timed out waiting for launch")
	}

	s := &Session{
		allocCtx:    allocCtx,
		cancelAlloc: cancelAlloc,
		browserCtx:  browserCtx,
		cancelBrow:  cancelBrow,
	}
	// browserCtx IS the first tab's CDP target — reuse it directly as tab 0
	// rather than spawning a child context. Spawning a child context (NewContext)
	// creates a second Chrome tab, leaving the original about:blank orphaned.
	// cancelBrow tears it down at session Close, so no separate cancel needed.
	s.tabs = []*tabHandle{{ctx: browserCtx, cancel: nil, title: "New Tab"}}
	s.active = 0

	// Start capturing console events from the first tab. Each new tab gets its
	// own listener in NewTab.
	s.listenConsole(s.tabs[0])

	return s, nil
}

// currentTab returns the active tab's context (where all single-tab actions run).
func (s *Session) currentTab() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active < 0 || s.active >= len(s.tabs) {
		return s.browserCtx // fallback — shouldn't happen
	}
	return s.tabs[s.active].ctx
}

// listenConsole attaches a CDP Runtime event listener to a tab that captures
// console.log/error/warn and uncaught exceptions into the session's rolling
// buffer. It runs in a goroutine for the lifetime of the tab.
func (s *Session) listenConsole(tab *tabHandle) {
	tabIdx := -1
	s.mu.Lock()
	for i, t := range s.tabs {
		if t == tab {
			tabIdx = i
			break
		}
	}
	s.mu.Unlock()

	chromedp.ListenTarget(tab.ctx, func(ev any) {
		switch e := ev.(type) {
		case *cdpruntime.EventConsoleAPICalled:
			// Join the args' descriptions into a single text line. RemoteObject
			// values come as descriptions (strings, "3", "\"hello\"", "Object").
			var parts []string
			for _, arg := range e.Args {
				if arg.Description != "" {
					parts = append(parts, arg.Description)
				} else if arg.Value != nil {
					parts = append(parts, fmt.Sprintf("%v", arg.Value))
				} else if arg.Type != "" {
					parts = append(parts, string(arg.Type))
				}
			}
			s.addConsole(consoleEntry{
				Tab:   tabIdx,
				Level: string(e.Type),
				Text:  strings.Join(parts, " "),
				Time:  time.Now(),
			})
		case *cdpruntime.EventExceptionThrown:
			// Uncaught exception — the exception details carry the text.
			text := "Uncaught"
			if e.ExceptionDetails != nil {
				if e.ExceptionDetails.Text != "" {
					text = e.ExceptionDetails.Text
				} else if e.ExceptionDetails.Exception != nil && e.ExceptionDetails.Exception.Description != "" {
					text = e.ExceptionDetails.Exception.Description
				}
			}
			s.addConsole(consoleEntry{
				Tab:   tabIdx,
				Level: "exception",
				Text:  text,
				Time:  time.Now(),
			})
		}
	})
}

// addConsole appends a console entry to the rolling buffer (thread-safe).
func (s *Session) addConsole(e consoleEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.console = append(s.console, e)
	if len(s.console) > maxConsoleEntries {
		s.console = s.console[len(s.console)-maxConsoleEntries:]
	}
}

// Console returns the captured console messages, optionally filtered by level.
// When level is empty, all entries are returned. Each entry is formatted as
// "[tab N] LEVEL: text". Entries are returned oldest-first.
func (s *Session) Console(level string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	level = strings.ToLower(level)
	var out []string
	for _, e := range s.console {
		if level != "" && e.Level != level {
			continue
		}
		out = append(out, fmt.Sprintf("[tab %d] %s: %s", e.Tab, e.Level, e.Text))
	}
	return out
}

// ClearConsole empties the console buffer.
func (s *Session) ClearConsole() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.console = nil
}

// ─── Navigation ──────────────────────────────────────────────────────────────

// Navigate loads a URL in the current tab and waits for the body to be ready.
func (s *Session) Navigate(ctx context.Context, url string) error {
	tctx := s.currentTab()
	err := chromedp.Run(tctx,
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
	if err == nil {
		s.mu.Lock()
		if s.active < len(s.tabs) {
			s.tabs[s.active].url = url
		}
		s.mu.Unlock()
	}
	return err
}

// Back navigates the current tab to the previous page in browser history.
func (s *Session) Back(ctx context.Context) error {
	return chromedp.Run(s.currentTab(),
		chromedp.Evaluate(`history.back()`, nil),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
}

// Forward navigates the current tab to the next page in browser history.
func (s *Session) Forward(ctx context.Context) error {
	return chromedp.Run(s.currentTab(),
		chromedp.Evaluate(`history.forward()`, nil),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
}

// ─── Interaction ─────────────────────────────────────────────────────────────

// Click an element matching a CSS selector.
func (s *Session) Click(ctx context.Context, selector string) error {
	return chromedp.Run(s.currentTab(),
		chromedp.WaitVisible(selector, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
	)
}

// Type text into an element matching a CSS selector.
func (s *Session) Type(ctx context.Context, selector, text string) error {
	return chromedp.Run(s.currentTab(),
		chromedp.WaitVisible(selector, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
		chromedp.SendKeys(selector, text, chromedp.ByQuery),
	)
}

// Hover moves the mouse over an element matching a CSS selector, triggering
// hover states, dropdown menus, tooltips, etc.
func (s *Session) Hover(ctx context.Context, selector string) error {
	// Scroll the element into view first, then dispatch a real mouseover via JS
	// (chromedp's MouseEvent needs coordinates; JS dispatch is simpler and
	// triggers DOM mouseover/mouseenter events the page listens for).
	js := fmt.Sprintf(`(function(){
		var el = document.querySelector(%q);
		if(!el) return "element not found: %s";
		el.scrollIntoView({block:'center'});
		el.dispatchEvent(new MouseEvent('mouseover', {bubbles:true}));
		el.dispatchEvent(new MouseEvent('mouseenter', {bubbles:true}));
		return "hovered";
	})()`, selector, selector)
	var result string
	err := chromedp.Run(s.currentTab(),
		chromedp.Evaluate(js, &result),
	)
	if err != nil {
		return err
	}
	if result != "hovered" {
		return fmt.Errorf("hover: %s", result)
	}
	return nil
}

// PressKey sends a key event (e.g. "Enter", "Escape", "Tab", "ArrowDown") to the
// page. The key is sent to whatever element currently has focus. Use after
// browser_click or browser_type to submit forms, close modals, navigate
// dropdowns, etc.
func (s *Session) PressKey(ctx context.Context, key string) error {
	// Map common key names to chromedp's kb package constants. chromedp.KeyEvent
	// accepts the raw rune (e.g. kb.Enter = "\r") or a printable char.
	rune, ok := keyToRune[key]
	if !ok {
		// If it's a single printable char, send it directly.
		if len(key) == 1 {
			rune = key
		} else {
			return fmt.Errorf("press_key: unknown key %q — use a single char or one of: Enter, Escape, Tab, Backspace, Space, ArrowUp, ArrowDown, ArrowLeft, ArrowRight", key)
		}
	}
	return chromedp.Run(s.currentTab(),
		chromedp.KeyEvent(rune),
	)
}

// keyToRune maps named keys to chromedp's kb package rune constants.
var keyToRune = map[string]string{
	"Enter":      "\r",
	"Escape":     "\u001b",
	"Tab":        "\t",
	"Backspace":  "\b",
	"Space":      " ",
	"ArrowUp":    "\u0304",
	"ArrowDown":  "\u0301",
	"ArrowLeft":  "\u0302",
	"ArrowRight": "\u0303",
}

// Select picks an option in a <select> element by value (the option's value
// attribute). The selector must match a <select> element.
func (s *Session) Select(ctx context.Context, selector, value string) error {
	js := fmt.Sprintf(`(function(){
		var el = document.querySelector(%q);
		if(!el) return "element not found: %s";
		if(el.tagName !== 'SELECT') return "element is not a <select>: " + el.tagName;
		el.value = %q;
		el.dispatchEvent(new Event('change', {bubbles:true}));
		return "selected";
	})()`, selector, selector, value)
	var result string
	err := chromedp.Run(s.currentTab(),
		chromedp.Evaluate(js, &result),
	)
	if err != nil {
		return err
	}
	if result != "selected" {
		return fmt.Errorf("select: %s", result)
	}
	return nil
}

// Scroll scrolls the page by the given x and y deltas in CSS pixels. Positive y
// scrolls down; positive x scrolls right. Use (0, 500) to scroll down half a
// viewport, or a large y to reach the bottom.
func (s *Session) Scroll(ctx context.Context, dx, dy int) error {
	js := fmt.Sprintf(`(function(){
		window.scrollBy(%d, %d);
		return "scrolled to " + window.scrollX + "," + window.scrollY + " (page " + window.innerHeight + "px tall, total " + document.body.scrollHeight + "px)";
	})()`, dx, dy)
	var result string
	err := chromedp.Run(s.currentTab(),
		chromedp.Evaluate(js, &result),
	)
	if err != nil {
		return err
	}
	// result carries the post-scroll position — useful for the agent to know if
	// it hit the bottom. We don't return it as an error; the caller (tool handler)
	// gets it via a separate call or we could thread it. For now, scroll is
	// fire-and-forget; the agent can browser_screenshot to see where it is.
	_ = result
	return nil
}

// ScrollTo scrolls the element matching the selector into view (centered).
func (s *Session) ScrollTo(ctx context.Context, selector string) error {
	return chromedp.Run(s.currentTab(),
		chromedp.ScrollIntoView(selector, chromedp.ByQuery),
	)
}

// ─── Inspection ──────────────────────────────────────────────────────────────

// Screenshot captures the current viewport as a PNG. When fullPage is true it
// captures the entire scrollable page instead (can be very large — token cost).
func (s *Session) Screenshot(ctx context.Context, fullPage bool) ([]byte, error) {
	if fullPage {
		var buf []byte
		err := chromedp.Run(s.currentTab(),
			chromedp.FullScreenshot(&buf, 90),
		)
		return buf, err
	}
	var buf []byte
	err := chromedp.Run(s.currentTab(),
		chromedp.CaptureScreenshot(&buf),
	)
	return buf, err
}

// Eval runs a JavaScript expression and returns its result as a string.
func (s *Session) Eval(ctx context.Context, js string) (string, error) {
	var result string
	// Wrap in a function call so bare expressions work (chromedp.Evaluate expects
	// an expression that returns a value; wrapping in `(function(){ … })()` is
	// the idiomatic pattern and handles both expressions and statements).
	wrapped := fmt.Sprintf("(function(){ return %s; })()", js)
	err := chromedp.Run(s.currentTab(),
		chromedp.Evaluate(wrapped, &result),
	)
	return result, err
}

// Text returns the visible text content of the current page (body.innerText).
func (s *Session) Text(ctx context.Context) (string, error) {
	var text string
	err := chromedp.Run(s.currentTab(),
		chromedp.Evaluate(`document.body ? document.body.innerText : ""`, &text),
	)
	return strings.TrimSpace(text), err
}

// ─── Tab management ──────────────────────────────────────────────────────────

// NewTab opens a new tab (about:blank) and switches focus to it. Returns the
// tab index (1-based, for the agent's reference).
func (s *Session) NewTab(ctx context.Context) (int, error) {
	tabCtx, tabCancel := chromedp.NewContext(s.browserCtx)
	// Eagerly create the tab by running a no-op.
	if err := chromedp.Run(tabCtx); err != nil {
		tabCancel()
		return 0, fmt.Errorf("failed to create tab: %w", err)
	}
	s.mu.Lock()
	tab := &tabHandle{ctx: tabCtx, cancel: tabCancel, title: "New Tab", url: "about:blank"}
	s.tabs = append(s.tabs, tab)
	idx := len(s.tabs) - 1
	s.active = idx
	s.mu.Unlock()
	s.listenConsole(tab)
	return idx + 1, nil // 1-based
}

// SwitchTab switches focus to the tab at the given 1-based index. Returns the
// tab's current URL and title.
func (s *Session) SwitchTab(index int) (url, title string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 1 || index > len(s.tabs) {
		return "", "", fmt.Errorf("tab %d does not exist (have %d tabs)", index, len(s.tabs))
	}
	s.active = index - 1
	return s.tabs[s.active].url, s.tabs[s.active].title, nil
}

// CloseTab closes the tab at the given 1-based index. If closing the active tab,
// focus moves to the previous tab (or the first one). Closing the last tab is
// not allowed (the browser needs at least one tab).
func (s *Session) CloseTab(index int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 1 || index > len(s.tabs) {
		return fmt.Errorf("tab %d does not exist (have %d tabs)", index, len(s.tabs))
	}
	if len(s.tabs) == 1 {
		return errors.New("can't close the last tab — the browser needs at least one")
	}
	idx := index - 1
	tab := s.tabs[idx]
	// Cancel the tab's chromedp context (closes the CDP target). The actual tab
	// close is handled by chromedp canceling the target.
	if tab.cancel != nil {
		tab.cancel()
	}
	s.tabs = append(s.tabs[:idx], s.tabs[idx+1:]...)
	// Fix the active index.
	if s.active >= len(s.tabs) {
		s.active = len(s.tabs) - 1
	} else if s.active == idx {
		// We closed the active tab — move to the previous one.
		s.active--
		if s.active < 0 {
			s.active = 0
		}
	} else if s.active > idx {
		s.active-- // the active tab shifted left
	}
	return nil
}

// ListTabs returns the current tabs as "N. url — title" strings (1-based).
func (s *Session) ListTabs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.tabs))
	for i, t := range s.tabs {
		marker := "  "
		if i == s.active {
			marker = "▶ "
		}
		url := t.url
		if url == "" {
			url = "(no URL)"
		}
		title := t.title
		if title == "" {
			title = "(untitled)"
		}
		out[i] = fmt.Sprintf("%s%d. %s — %s", marker, i+1, url, title)
	}
	return out
}

// TabCount returns the number of open tabs.
func (s *Session) TabCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tabs)
}

// Close tears down the Chrome process and releases all CDP resources. Safe to
// call multiple times.
func (s *Session) Close() {
	s.mu.Lock()
	for _, t := range s.tabs {
		if t.cancel != nil {
			t.cancel()
		}
	}
	s.mu.Unlock()
	if s.cancelBrow != nil {
		s.cancelBrow()
	}
	if s.cancelAlloc != nil {
		s.cancelAlloc()
	}
}

// Alive reports whether the Chrome process backing this session is still
// reachable. The runtime calls this on the cached session before each browser
// tool call so that a Chrome that died (the user quit the window, the OS killed
// it, a crash) is detected and relaunched — without it, every tool call would
// drive a dead chromedp context and return "context canceled" forever.
//
// Two independent signals: chromedp's LostConnection channel (closed when the
// CDP websocket to Chrome drops) and a zero-signal probe of the OS process
// (ESRCH = the process is gone). Either being dead means the session is dead.
func (s *Session) Alive() bool {
	c := chromedp.FromContext(s.browserCtx)
	if c == nil || c.Browser == nil {
		return false
	}
	// LostConnection is closed when the websocket to Chrome drops — chromedp's
	// own definitive signal that Chrome is gone.
	select {
	case <-c.Browser.LostConnection:
		return false
	default:
	}
	// Belt-and-suspenders: confirm the OS process is still alive. A zero signal
	// (signal 0) checks existence without actually signaling — ESRCH means no
	// such process.
	if proc := c.Browser.Process(); proc != nil {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return false
		}
	}
	return true
}
