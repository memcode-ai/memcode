// Package browser drives a long-lived Chrome instance via the Chrome DevTools
// Protocol (chromedp) so the agent can fully interact with web pages as tools —
// the same dispatch as read_file/bash. It exposes the full browser interaction
// surface a user has: navigate, click, type, scroll, hover, keyboard, dropdowns,
// history, tabs, uploads, viewport, screenshots, console logs, and arbitrary JS.
//
// The Session wraps a persistent chromedp context created once (on New) and
// reused across tool calls — a single Chrome process for the whole agent
// session, torn down on Close. It tracks multiple tabs (each a child chromedp
// context) and a rolling console-log buffer captured via CDP Runtime events.
// The profile is EPHEMERAL (a fresh temp profile per launch): no existing
// cookies or logins, nothing persisted across sessions. It reuses
// browserrender.Find to locate the Chrome binary (no new dependency, no Node).
//
// Every operation runs under a bounded, caller-cancellable context derived
// from the tab's context: the timeout/cancel propagates into the in-flight CDP
// command (chromedp aborts it) while the tab itself survives for the next op.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/memcode-ai/memcode/internal/browserrender"
)

// maxConsoleEntries caps the rolling console buffer. The agent asks for console
// on demand (browser_console), and we keep the last N entries so a page that
// logs heavily doesn't grow the buffer unbounded.
const maxConsoleEntries = 200

// maxConsoleEntryLen bounds one captured entry (a page can console.log a
// megabyte string; the buffer must stay context-safe).
const maxConsoleEntryLen = 512

// opTimeout bounds every single browser operation. A page that never settles
// must never hang the agent turn.
const opTimeout = 30 * time.Second

// maxImageBytes caps a screenshot's payload (~1MB raw ≈ ~1.4MB base64). Larger
// captures are retried as JPEG at decreasing quality.
const maxImageBytes = 1 << 20

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
}

// consoleEntry is one captured console message (console.log/error/warn, an
// uncaught exception, or an auto-handled JS dialog).
type consoleEntry struct {
	Tab   int    // which tab index it came from
	Level string // "log", "error", "warn", "info", "debug", "exception", "dialog"
	Text  string // the message text (args joined)
	Time  time.Time
}

// Options configures a Session launch.
type Options struct {
	// Headless runs Chrome without a window — required when there is no desktop
	// session (a gateway job under launchd/systemd). Interactive sessions run
	// headed so the user can watch the agent work.
	Headless bool
}

// New launches a Chrome instance and returns a persistent Session with one
// open tab. The Chrome binary is discovered via browserrender.Find (which
// honors CHROME_PATH) — a clear error is returned when none is available.
func New(opt Options) (*Session, error) {
	path, ok := browserrender.Find()
	if !ok {
		if p := os.Getenv("CHROME_PATH"); p != "" {
			return nil, fmt.Errorf("CHROME_PATH is set to %q but no browser binary exists there — fix or unset it", p)
		}
		return nil, errors.New("Chrome not found — install Google Chrome or set CHROME_PATH to the browser binary")
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(path),
		chromedp.DisableGPU,
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
	)
	if opt.Headless {
		// The proven headless flag set from browserrender: new headless mode plus
		// enough stealth that ordinary sites serve real content.
		opts = append(opts,
			chromedp.Flag("headless", "new"),
			chromedp.Flag("disable-blink-features", "AutomationControlled"),
			chromedp.Flag("lang", "en-US,en"),
		)
	} else {
		opts = append(opts, chromedp.Flag("headless", false)) // visible window
	}
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
	// cancelBrow tears it down at session Close; because the session's whole CDP
	// connection lives on this target, tab 1 can never be closed individually.
	s.tabs = []*tabHandle{{ctx: browserCtx, cancel: nil}}
	s.active = 0

	// Start capturing console + dialog events from the first tab. Each new tab
	// gets its own listener in NewTab.
	s.listenTab(s.tabs[0])

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

// run executes chromedp actions on the active tab under a context that is (a)
// bounded by opTimeout and (b) cancelled when the caller's ctx is — both
// propagate INTO the in-flight CDP command (chromedp aborts it); the tab
// context itself is the parent and survives for the next operation. No
// goroutine wrapper that abandons a still-running CDP call.
func (s *Session) run(ctx context.Context, actions ...chromedp.Action) error {
	return s.runOn(ctx, s.currentTab(), opTimeout, actions...)
}

func (s *Session) runOn(ctx context.Context, tabCtx context.Context, timeout time.Duration, actions ...chromedp.Action) error {
	opCtx, cancel := context.WithTimeout(tabCtx, timeout)
	defer cancel()
	if ctx != nil {
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
	}
	err := chromedp.Run(opCtx, actions...)
	if err != nil && errors.Is(opCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("browser operation timed out after %s (the page may still be loading — try browser_wait or a screenshot)", timeout)
	}
	return err
}

// listenTab attaches CDP event listeners to a tab: console.log/error/warn and
// uncaught exceptions feed the rolling buffer, and JavaScript dialogs are
// auto-handled so they can never hang the tab (there is no human to click OK).
// Alerts and beforeunload are accepted (so the page and navigation proceed);
// confirm/prompt are dismissed (the conservative answer). The dialog text is
// recorded in the console buffer so the model sees what happened.
func (s *Session) listenTab(tab *tabHandle) {
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
			s.addConsole(consoleEntry{Tab: tabIdx, Level: "exception", Text: text, Time: time.Now()})
		case *page.EventJavascriptDialogOpening:
			accept := e.Type == page.DialogTypeAlert || e.Type == page.DialogTypeBeforeunload
			verb := "dismissed"
			if accept {
				verb = "accepted"
			}
			s.addConsole(consoleEntry{
				Tab:   tabIdx,
				Level: "dialog",
				Text:  fmt.Sprintf("auto-%s %s: %s", verb, e.Type, e.Message),
				Time:  time.Now(),
			})
			// Must respond from a goroutine — the listener runs on the CDP event
			// loop and HandleJavaScriptDialog is itself a CDP command.
			go func() {
				_ = chromedp.Run(tab.ctx, page.HandleJavaScriptDialog(accept))
			}()
		}
	})
}

// addConsole appends a console entry to the rolling buffer (thread-safe),
// bounding each entry's length.
func (s *Session) addConsole(e consoleEntry) {
	if len(e.Text) > maxConsoleEntryLen {
		e.Text = e.Text[:maxConsoleEntryLen] + "…"
	}
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
		out = append(out, fmt.Sprintf("[tab %d] %s: %s", e.Tab+1, e.Level, e.Text))
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
	return s.run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
}

// Back navigates the current tab to the previous page in browser history.
func (s *Session) Back(ctx context.Context) error {
	return s.run(ctx,
		chromedp.NavigateBack(),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
}

// Forward navigates the current tab to the next page in browser history.
func (s *Session) Forward(ctx context.Context) error {
	return s.run(ctx,
		chromedp.NavigateForward(),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
}

// WaitFor waits until an element matching the selector reaches the given state
// ("visible", "hidden", or "ready" = attached to the DOM), bounded by timeout
// (capped at 60s). This is how the agent settles SPA navigation instead of
// racing it.
func (s *Session) WaitFor(ctx context.Context, selector, state string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second // schema default
	}
	if timeout > 60*time.Second {
		timeout = 60 * time.Second // hard cap
	}
	var action chromedp.Action
	switch state {
	case "", "visible":
		action = chromedp.WaitVisible(selector, chromedp.ByQuery)
	case "hidden":
		action = chromedp.WaitNotVisible(selector, chromedp.ByQuery)
	case "ready":
		action = chromedp.WaitReady(selector, chromedp.ByQuery)
	default:
		return fmt.Errorf("wait: unknown state %q — use visible, hidden, or ready", state)
	}
	err := s.runOn(ctx, s.currentTab(), timeout, action)
	if err != nil && strings.Contains(err.Error(), "timed out") {
		return fmt.Errorf("wait: %q did not become %s within %s", selector, stateWord(state), timeout)
	}
	return err
}

func stateWord(state string) string {
	if state == "" {
		return "visible"
	}
	return state
}

// ─── Interaction ─────────────────────────────────────────────────────────────

// Click an element matching a CSS selector.
func (s *Session) Click(ctx context.Context, selector string) error {
	return s.run(ctx,
		chromedp.WaitVisible(selector, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
	)
}

// Type text into an element matching a CSS selector. By default the field is
// cleared first (what a user means by "type X into the box"); append=true
// keeps the existing value and appends.
func (s *Session) Type(ctx context.Context, selector, text string, appendTo bool) error {
	actions := []chromedp.Action{
		chromedp.WaitVisible(selector, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery),
	}
	if !appendTo {
		clear := fmt.Sprintf(`(function(){
			var el = document.querySelector(%q);
			if (el && ('value' in el)) { el.value = ''; el.dispatchEvent(new Event('input', {bubbles:true})); }
			return true;
		})()`, selector)
		var ok bool
		actions = append(actions, chromedp.Evaluate(clear, &ok))
	}
	actions = append(actions, chromedp.SendKeys(selector, text, chromedp.ByQuery))
	return s.run(ctx, actions...)
}

// Upload sets the files of a file input matching the selector. Path validation
// (project-root confinement, symlink resolution) is the CALLER's job — this
// layer only drives CDP.
func (s *Session) Upload(ctx context.Context, selector string, files []string) error {
	return s.run(ctx,
		chromedp.WaitReady(selector, chromedp.ByQuery),
		chromedp.SetUploadFiles(selector, files, chromedp.ByQuery),
	)
}

// Resize sets the viewport to the given CSS dimensions.
func (s *Session) Resize(ctx context.Context, width, height int) error {
	if width < 64 || width > 4096 || height < 64 || height > 4096 {
		return fmt.Errorf("resize: dimensions must be between 64 and 4096 (got %dx%d)", width, height)
	}
	return s.run(ctx, chromedp.EmulateViewport(int64(width), int64(height)))
}

// Hover moves the mouse over an element matching a CSS selector, triggering
// hover states, dropdown menus, tooltips, etc.
func (s *Session) Hover(ctx context.Context, selector string) error {
	// Scroll the element into view first, then dispatch a real mouseover via JS
	// (chromedp's MouseEvent needs coordinates; JS dispatch is simpler and
	// triggers DOM mouseover/mouseenter events the page listens for). CSS
	// :hover states may not trigger — this is DOM-event hover.
	js := fmt.Sprintf(`(function(){
		var el = document.querySelector(%q);
		if(!el) return "element not found: %s";
		el.scrollIntoView({block:'center'});
		el.dispatchEvent(new MouseEvent('mouseover', {bubbles:true}));
		el.dispatchEvent(new MouseEvent('mouseenter', {bubbles:true}));
		return "hovered";
	})()`, selector, selector)
	var result string
	if err := s.run(ctx, chromedp.Evaluate(js, &result)); err != nil {
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
	return s.run(ctx, chromedp.KeyEvent(rune))
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
	if err := s.run(ctx, chromedp.Evaluate(js, &result)); err != nil {
		return err
	}
	if result != "selected" {
		return fmt.Errorf("select: %s", result)
	}
	return nil
}

// Scroll scrolls the page by the given x and y deltas in CSS pixels and
// returns the resulting position ("scrolled to X,Y (viewport Hpx, total Tpx)")
// so the agent knows whether it hit the bottom.
func (s *Session) Scroll(ctx context.Context, dx, dy int) (string, error) {
	js := fmt.Sprintf(`(function(){
		window.scrollBy(%d, %d);
		return "scrolled to " + window.scrollX + "," + window.scrollY + " (viewport " + window.innerHeight + "px, total " + document.body.scrollHeight + "px)";
	})()`, dx, dy)
	var result string
	err := s.run(ctx, chromedp.Evaluate(js, &result))
	return result, err
}

// ScrollTo scrolls the element matching the selector into view (centered).
func (s *Session) ScrollTo(ctx context.Context, selector string) error {
	return s.run(ctx, chromedp.ScrollIntoView(selector, chromedp.ByQuery))
}

// ─── Inspection ──────────────────────────────────────────────────────────────

// Screenshot captures the current viewport (or the full scrollable page) and
// returns the image bytes plus their actual mime type. Captures above
// maxImageBytes are retried as JPEG at decreasing quality so a single
// screenshot can't flood the model's context.
func (s *Session) Screenshot(ctx context.Context, fullPage bool) ([]byte, string, error) {
	var buf []byte
	if fullPage {
		// FullScreenshot with quality<100 emits JPEG. Walk the quality ladder;
		// if even the lowest rung is over the cap, fail CLOSED — an oversized
		// image must never reach the context.
		for _, q := range []int{85, 50, 25} {
			if err := s.run(ctx, chromedp.FullScreenshot(&buf, q)); err != nil {
				return nil, "", err
			}
			if len(buf) <= maxImageBytes {
				return buf, "image/jpeg", nil
			}
		}
		return nil, "", fmt.Errorf("full-page screenshot is %dKB even at lowest quality (cap %dKB) — the page is too tall; capture the viewport instead and scroll", len(buf)/1024, maxImageBytes/1024)
	}
	if err := s.run(ctx, chromedp.CaptureScreenshot(&buf)); err != nil {
		return nil, "", err
	}
	if len(buf) <= maxImageBytes {
		return buf, "image/png", nil
	}
	// PNG too big (dense page) — recapture the viewport as JPEG down the
	// quality ladder; fail closed if it never fits.
	for _, q := range []int64{70, 40} {
		q := q
		err := s.run(ctx, chromedp.ActionFunc(func(cctx context.Context) error {
			b, err := page.CaptureScreenshot().
				WithFormat(page.CaptureScreenshotFormatJpeg).
				WithQuality(q).
				Do(cctx)
			if err != nil {
				return err
			}
			buf = b
			return nil
		}))
		if err != nil {
			return nil, "", err
		}
		if len(buf) <= maxImageBytes {
			return buf, "image/jpeg", nil
		}
	}
	return nil, "", fmt.Errorf("screenshot is %dKB even at lowest quality (cap %dKB) — resize the viewport smaller (browser_resize) and retry", len(buf)/1024, maxImageBytes/1024)
}

// Eval runs JavaScript in the page and returns the result rendered as a
// string: strings verbatim, numbers/booleans/objects/arrays as JSON,
// undefined/null as "undefined". Accepts both expressions ("1+1",
// "document.title") and statements ("var x = 5; x * 2").
func (s *Session) Eval(ctx context.Context, js string) (string, error) {
	out, err := s.evalWrapped(ctx, fmt.Sprintf("(function(){ return %s\n; })()", js))
	if err != nil && strings.Contains(err.Error(), "SyntaxError") {
		// Statements don't fit an expression return — run as a body; the result
		// is whatever the last `return` yields (or undefined).
		out, err = s.evalWrapped(ctx, fmt.Sprintf("(function(){ %s\n })()", js))
	}
	return out, err
}

func (s *Session) evalWrapped(ctx context.Context, wrapped string) (string, error) {
	var raw json.RawMessage
	err := s.run(ctx, chromedp.Evaluate(wrapped, &raw))
	if err != nil {
		if errors.Is(err, chromedp.ErrJSUndefined) || errors.Is(err, chromedp.ErrJSNull) {
			return "undefined", nil
		}
		return "", err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "undefined", nil
	}
	// Unquote plain strings so "hello" comes back as hello.
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str, nil
	}
	return trimmed, nil
}

// Text returns the visible text content of the current page (body.innerText).
func (s *Session) Text(ctx context.Context) (string, error) {
	var text string
	err := s.run(ctx, chromedp.Evaluate(`document.body ? document.body.innerText : ""`, &text))
	return strings.TrimSpace(text), err
}

// ─── Tab management ──────────────────────────────────────────────────────────

// TabInfo is a live snapshot of one tab.
type TabInfo struct {
	Index  int // 1-based
	Active bool
	URL    string
	Title  string
}

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
	tab := &tabHandle{ctx: tabCtx, cancel: tabCancel}
	s.tabs = append(s.tabs, tab)
	idx := len(s.tabs) - 1
	s.active = idx
	s.mu.Unlock()
	s.listenTab(tab)
	return idx + 1, nil // 1-based
}

// SwitchTab switches focus to the tab at the given 1-based index and returns
// its LIVE url and title (queried from the page, not cached).
func (s *Session) SwitchTab(ctx context.Context, index int) (url, title string, err error) {
	s.mu.Lock()
	if index < 1 || index > len(s.tabs) {
		n := len(s.tabs)
		s.mu.Unlock()
		return "", "", fmt.Errorf("tab %d does not exist (have %d tabs)", index, n)
	}
	s.active = index - 1
	tab := s.tabs[s.active]
	s.mu.Unlock()
	url, title = s.tabMeta(ctx, tab)
	return url, title, nil
}

// tabMeta queries a tab's live location and title, bounded by a short timeout
// so a wedged tab can't stall a listing.
func (s *Session) tabMeta(ctx context.Context, tab *tabHandle) (url, title string) {
	_ = s.runOn(ctx, tab.ctx, 3*time.Second,
		chromedp.Location(&url),
		chromedp.Title(&title),
	)
	if url == "" {
		url = "(no URL)"
	}
	if title == "" {
		title = "(untitled)"
	}
	return url, title
}

// CloseTab closes the tab at the given 1-based index via CDP (the real Chrome
// tab closes, not just our handle). Closing the ACTIVE tab deterministically
// activates the nearest lower index. Tab 1 hosts the session's CDP connection
// and can't be closed individually; neither can the last remaining tab.
// Returns the new active tab's 1-based index.
func (s *Session) CloseTab(index int) (newActive int, err error) {
	s.mu.Lock()
	if index < 1 || index > len(s.tabs) {
		n := len(s.tabs)
		s.mu.Unlock()
		return 0, fmt.Errorf("tab %d does not exist (have %d tabs)", index, n)
	}
	if len(s.tabs) == 1 {
		s.mu.Unlock()
		return 0, errors.New("can't close the last tab — the browser needs at least one")
	}
	if index == 1 {
		s.mu.Unlock()
		return 0, errors.New("tab 1 hosts the browser session and can't be closed — switch to it and navigate instead, or close the other tabs")
	}
	idx := index - 1
	tab := s.tabs[idx]
	s.tabs = append(s.tabs[:idx], s.tabs[idx+1:]...)
	// Deterministic active-tab fixup: closing the active tab activates the
	// nearest LOWER index; tabs above the closed one shift left.
	switch {
	case s.active == idx:
		s.active = idx - 1
	case s.active > idx:
		s.active--
	}
	newActive = s.active + 1
	s.mu.Unlock()

	// Close the real Chrome tab, then release the chromedp context.
	_ = s.runOn(context.Background(), tab.ctx, 5*time.Second, chromedp.ActionFunc(func(cctx context.Context) error {
		return page.Close().Do(cctx)
	}))
	if tab.cancel != nil {
		tab.cancel()
	}
	return newActive, nil
}

// ListTabs returns the current tabs with LIVE urls and titles.
func (s *Session) ListTabs(ctx context.Context) []TabInfo {
	s.mu.Lock()
	tabs := append([]*tabHandle(nil), s.tabs...)
	active := s.active
	s.mu.Unlock()
	out := make([]TabInfo, len(tabs))
	for i, t := range tabs {
		url, title := s.tabMeta(ctx, t)
		out[i] = TabInfo{Index: i + 1, Active: i == active, URL: url, Title: title}
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
