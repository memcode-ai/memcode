package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/browser"
)

// browserOrInit returns the persistent Chrome session, lazily launching it on the
// first browser tool call. The session is reused across all subsequent browser
// tool calls and torn down at session end (CloseBrowser). Interactive sessions
// launch Chrome headed (a visible window, EPHEMERAL profile — no logins carried
// over); gateway/service sessions launch headless (no desktop needed). A clear
// error — not a chromedp stack trace — is returned when Chrome isn't installed.
//
// If the previously launched Chrome has died (the user quit the window, the OS
// killed it, a crash), the cached session is torn down and a fresh Chrome is
// relaunched in its place — otherwise every tool call would drive a dead chromedp
// context and return "context canceled" forever. This is the recovery path: no
// restart of the whole CLI is needed to bring the browser back.
func (s *Session) browserOrInit() (*browser.Session, error) {
	if s.browserSession != nil {
		// Cached — but verify the Chrome process is still alive. A dead session
		// is worse than no session: it can't recover on its own.
		if !s.browserSession.Alive() {
			s.printf("  ↻ Chrome went away — relaunching…\n")
			s.browserSession.Close()
			s.browserSession = nil
			// Fall through to a fresh New().
		} else {
			return s.browserSession, nil
		}
	}
	sess, err := browser.New(browser.Options{Headless: s.browserHeadless})
	if err != nil {
		if s.browserHeadless {
			return nil, fmt.Errorf("%s (this is a gateway job — Chrome must be installed on the GATEWAY machine for this agent's browser toolset)", err)
		}
		return nil, err
	}
	s.browserSession = sess
	return sess, nil
}

// browserGate applies the permission gate to a Medium browser action (navigate,
// click, type — they act on the world). Read-only browser tools (screenshot, text)
// are Safe and don't call this; browser_eval gates SEPARATELY at Dangerous because it
// runs arbitrary JS (can read document.cookie / POST authed page data out). Returns an
// error result when denied.
func (s *Session) browserGate(ctx context.Context, label, detail string) (*toolResult, bool) {
	if ok, reason := s.gate(ctx, permissions.Medium, false, ApprovalRequest{
		Title: label, Label: label, Detail: detail, Risk: permissions.Medium.String(),
	}); !ok {
		tr := errResult("browser action denied: " + reason)
		return &tr, false
	}
	return nil, true
}

func (s *Session) browserNavigateTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserNavigateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	url := strings.TrimSpace(in.URL)
	if url == "" {
		return errResult("browser_navigate needs a `url`.")
	}
	if tr, ok := s.browserGate(ctx, "Browser navigate", url); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "navigate", url, false)
	if err := sess.Navigate(ctx, url); err != nil {
		return errResult("navigate failed: " + err.Error())
	}
	return textResult("navigated to " + url)
}

func (s *Session) browserClickTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserClickInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	sel := strings.TrimSpace(in.Selector)
	if sel == "" {
		return errResult("browser_click needs a `selector`.")
	}
	if tr, ok := s.browserGate(ctx, "Browser click", sel); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "click", sel, false)
	if err := sess.Click(ctx, sel); err != nil {
		return errResult("click failed: " + err.Error())
	}
	return textResult("clicked " + sel)
}

func (s *Session) browserTypeTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserTypeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	sel := strings.TrimSpace(in.Selector)
	if sel == "" {
		return errResult("browser_type needs a `selector`.")
	}
	if tr, ok := s.browserGate(ctx, "Browser type", sel); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "type", sel, false)
	if err := sess.Type(ctx, sel, in.Text, in.Append); err != nil {
		return errResult("type failed: " + err.Error())
	}
	verb := "typed"
	if in.Append {
		verb = "appended"
	}
	return textResult(fmt.Sprintf("%s %d chars into %s", verb, len(in.Text), sel))
}

// browserScreenshotTool captures the current page as a PNG image and returns it as
// an image block — the payoff for Layer A (tool results that carry vision). The
// model SEES the screenshot.
func (s *Session) browserScreenshotTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserScreenshotInput
	if err := json.Unmarshal(input, &in); err != nil {
		s.printf("⚠ malformed tool input (%v) — using defaults\n", err)
	} // optional full_page — all fields optional
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "screenshot", "", false)
	img, mime, err := sess.Screenshot(ctx, in.FullPage)
	if err != nil {
		return errResult("screenshot failed: " + err.Error())
	}
	// Return the image as a tool_result content block — the provider converters
	// emit it as an image block in the tool_result content union (Anthropic vision).
	return imageResult(mime, img)
}

func (s *Session) browserEvalTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserEvalInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	js := strings.TrimSpace(in.Script)
	if js == "" {
		return errResult("browser_eval needs a `script`.")
	}
	// Arbitrary JS in a headed, real-profile browser is strictly more powerful than
	// click/type — it can read cookies/localStorage and exfiltrate authed page data. Gate
	// at Dangerous (prompts in ask + auto; auto-runs only under explicit allow-all), and it
	// must NOT be parallel-safe (see the tool registry).
	if ok, reason := s.gate(ctx, permissions.Dangerous, false, ApprovalRequest{
		Title: "Browser eval", Label: "Browser eval", Detail: clip(js, 80),
		Risk: permissions.Dangerous.String(),
	}); !ok {
		return errResult("browser_eval denied: " + reason)
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "eval", clip(js, 60), false)
	result, err := sess.Eval(ctx, js)
	if err != nil {
		return errResult("eval failed: " + err.Error())
	}
	return textResult(truncate(result, maxToolOutput))
}

func (s *Session) browserTextTool(ctx context.Context, input json.RawMessage) toolResult {
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "text", "", false)
	text, err := sess.Text(ctx)
	if err != nil {
		return errResult("text failed: " + err.Error())
	}
	return textResult(truncate(text, maxToolOutput))
}

func (s *Session) browserScrollTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserScrollInput
	if err := json.Unmarshal(input, &in); err != nil {
		s.printf("⚠ malformed tool input (%v) — using defaults\n", err)
	}
	if tr, ok := s.browserGate(ctx, "Browser scroll", fmt.Sprintf("dx=%d dy=%d", in.DX, in.DY)); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "scroll", fmt.Sprintf("dx=%d dy=%d", in.DX, in.DY), false)
	pos, err := sess.Scroll(ctx, in.DX, in.DY)
	if err != nil {
		return errResult("scroll failed: " + err.Error())
	}
	return textResult(pos)
}

func (s *Session) browserPressKeyTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserPressKeyInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	key := strings.TrimSpace(in.Key)
	if key == "" {
		return errResult("browser_press_key needs a `key`.")
	}
	if tr, ok := s.browserGate(ctx, "Browser press key", key); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "press_key", key, false)
	if err := sess.PressKey(ctx, key); err != nil {
		return errResult("press_key failed: " + err.Error())
	}
	return textResult("pressed " + key)
}

func (s *Session) browserHoverTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserHoverInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	sel := strings.TrimSpace(in.Selector)
	if sel == "" {
		return errResult("browser_hover needs a `selector`.")
	}
	if tr, ok := s.browserGate(ctx, "Browser hover", sel); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "hover", sel, false)
	if err := sess.Hover(ctx, sel); err != nil {
		return errResult("hover failed: " + err.Error())
	}
	return textResult("hovered " + sel)
}

func (s *Session) browserSelectTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserSelectInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	sel := strings.TrimSpace(in.Selector)
	if sel == "" {
		return errResult("browser_select needs a `selector`.")
	}
	if tr, ok := s.browserGate(ctx, "Browser select", sel+"="+in.Value); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "select", sel+"="+in.Value, false)
	if err := sess.Select(ctx, sel, in.Value); err != nil {
		return errResult("select failed: " + err.Error())
	}
	return textResult(fmt.Sprintf("selected %q in %s", in.Value, sel))
}

func (s *Session) browserBackTool(ctx context.Context, input json.RawMessage) toolResult {
	if tr, ok := s.browserGate(ctx, "Browser back", "navigate to previous page"); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "back", "", false)
	if err := sess.Back(ctx); err != nil {
		return errResult("back failed: " + err.Error())
	}
	return textResult("navigated back")
}

func (s *Session) browserForwardTool(ctx context.Context, input json.RawMessage) toolResult {
	if tr, ok := s.browserGate(ctx, "Browser forward", "navigate to next page"); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "forward", "", false)
	if err := sess.Forward(ctx); err != nil {
		return errResult("forward failed: " + err.Error())
	}
	return textResult("navigated forward")
}

func (s *Session) browserConsoleTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserConsoleInput
	if err := json.Unmarshal(input, &in); err != nil {
		s.printf("⚠ malformed tool input (%v) — using defaults\n", err)
	} // optional level filter
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "console", in.Level, false)
	lines := sess.Console(in.Level)
	if len(lines) == 0 {
		return textResult("(console is empty)")
	}
	return textResult(strings.Join(lines, "\n"))
}

func (s *Session) browserNewTabTool(ctx context.Context, input json.RawMessage) toolResult {
	if tr, ok := s.browserGate(ctx, "Browser new tab", "open a new tab"); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "new_tab", "", false)
	idx, err := sess.NewTab(ctx)
	if err != nil {
		return errResult("new_tab failed: " + err.Error())
	}
	return textResult(fmt.Sprintf("opened tab %d (now active)", idx))
}

func (s *Session) browserSwitchTabTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserSwitchTabInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	if in.Index < 1 {
		return errResult("browser_switch_tab needs a 1-based `index`.")
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "switch_tab", fmt.Sprintf("→ %d", in.Index), false)
	url, title, err := sess.SwitchTab(ctx, in.Index)
	if err != nil {
		return errResult("switch_tab failed: " + err.Error())
	}
	return textResult(fmt.Sprintf("switched to tab %d: %s — %s", in.Index, url, title))
}

func (s *Session) browserCloseTabTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserCloseTabInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	if in.Index < 1 {
		return errResult("browser_close_tab needs a 1-based `index`.")
	}
	if tr, ok := s.browserGate(ctx, "Browser close tab", fmt.Sprintf("close tab %d", in.Index)); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "close_tab", fmt.Sprintf("× %d", in.Index), false)
	newActive, err := sess.CloseTab(in.Index)
	if err != nil {
		return errResult("close_tab failed: " + err.Error())
	}
	return textResult(fmt.Sprintf("closed tab %d — tab %d is now active", in.Index, newActive))
}

func (s *Session) browserListTabsTool(ctx context.Context, input json.RawMessage) toolResult {
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "list_tabs", "", false)
	tabs := sess.ListTabs(ctx)
	if len(tabs) == 0 {
		return textResult("(no tabs)")
	}
	lines := make([]string, len(tabs))
	for i, t := range tabs {
		marker := "  "
		if t.Active {
			marker = "▶ "
		}
		lines[i] = fmt.Sprintf("%s%d. %s — %s", marker, t.Index, t.URL, t.Title)
	}
	return textResult(strings.Join(lines, "\n"))
}

// browserWaitTool waits for a selector to reach a state (visible/hidden/ready)
// — how the agent settles SPA navigation instead of racing it. Read-only.
func (s *Session) browserWaitTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserWaitInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	sel := strings.TrimSpace(in.Selector)
	if sel == "" {
		return errResult("browser_wait needs a `selector`.")
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	s.toolLine(true, "Browser", "wait", sel, false)
	if err := sess.WaitFor(ctx, sel, in.State, timeout); err != nil {
		return errResult("wait failed: " + err.Error())
	}
	state := in.State
	if state == "" {
		state = "visible"
	}
	return textResult(fmt.Sprintf("%s is now %s", sel, state))
}

// browserUploadTool sets a file input's files. The file must resolve INSIDE
// the project root after symlink evaluation — a symlink inside the repo
// pointing at ~/.ssh resolves outside and is refused. Same trust posture as
// the file tools.
func (s *Session) browserUploadTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserUploadInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	sel := strings.TrimSpace(in.Selector)
	path := strings.TrimSpace(in.Path)
	if sel == "" || path == "" {
		return errResult("browser_upload needs a `selector` and a `path`.")
	}
	// Gate BEFORE any path resolution: the tool must not act as a host
	// file-existence oracle. The approval shows the path as requested.
	if tr, ok := s.browserGate(ctx, "Browser upload", fmt.Sprintf("%s → %s", path, sel)); !ok {
		return *tr
	}
	resolved, size, err := resolveUploadPath(s.root, path)
	if err != nil {
		return errResult("upload refused: " + err.Error())
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "upload", filepath.Base(resolved), false)
	if err := sess.Upload(ctx, sel, []string{resolved}); err != nil {
		return errResult("upload failed: " + err.Error())
	}
	return textResult(fmt.Sprintf("uploaded %s (%d bytes) into %s", filepath.Base(resolved), size, sel))
}

// errUploadOutside is the ONE refusal for any path outside the project — the
// same message whether the target exists or not, so a refused upload leaks
// nothing about the host filesystem.
var errUploadOutside = errors.New("the path is outside the project root — only files inside the project can be uploaded")

// resolveUploadPath confines an upload to the project root in two layers:
// LEXICAL first — an absolute path outside the root, or a dot-dot escape, is
// refused before the filesystem is touched at all (no existence oracle) —
// then symlink resolution, which must also land inside the root. Only regular
// files are eligible.
func resolveUploadPath(root, path string) (string, int64, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", 0, err
	}
	inside := func(base, p string) bool {
		rel, err := filepath.Rel(base, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	// Layer 1: lexical, zero filesystem access.
	var candidate string
	if filepath.IsAbs(path) {
		candidate = filepath.Clean(path)
	} else {
		candidate = filepath.Join(rootAbs, path)
	}
	if !inside(rootAbs, candidate) {
		return "", 0, errUploadOutside
	}
	// Layer 2: resolve symlinks (root and file) — the resolved file must still
	// be inside the resolved root, so a symlink inside the repo pointing at
	// ~/.ssh is refused with the SAME generic error.
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", 0, err
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", 0, fmt.Errorf("cannot resolve %s: %w", path, err)
	}
	if !inside(rootReal, real) {
		return "", 0, errUploadOutside
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", 0, err
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", path)
	}
	return real, fi.Size(), nil
}

// browserResizeTool sets the viewport dimensions (responsive testing).
func (s *Session) browserResizeTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BrowserResizeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	if in.Width <= 0 || in.Height <= 0 {
		return errResult("browser_resize needs `width` and `height`.")
	}
	if tr, ok := s.browserGate(ctx, "Browser resize", fmt.Sprintf("%dx%d", in.Width, in.Height)); !ok {
		return *tr
	}
	sess, err := s.browserOrInit()
	if err != nil {
		return errResult(err.Error())
	}
	s.toolLine(true, "Browser", "resize", fmt.Sprintf("%dx%d", in.Width, in.Height), false)
	if err := sess.Resize(ctx, in.Width, in.Height); err != nil {
		return errResult("resize failed: " + err.Error())
	}
	return textResult(fmt.Sprintf("viewport is now %dx%d", in.Width, in.Height))
}
