// Package hooks runs user-defined shell commands at agent lifecycle points —
// the extensibility seam for policy and automation the prompt can't provide
// (deterministic guards, notifications, context injection). Configuration is
// plain JSON the user owns: ~/.memcode/hooks.json (user-wide) merged with
// <root>/.memcode/hooks.json (project; runs after user hooks).
//
// Events and semantics (deliberately Claude-Code-compatible so existing hook
// scripts port over):
//
//	session_start   payload {event, session_id, root}; combined stdout is
//	                injected into the system prompt as standing context.
//	pre_tool_use    payload {event, tool, input, session_id, root}. Exit 2
//	                BLOCKS the tool call and feeds stderr to the model as the
//	                reason. Any other non-zero exit is a non-blocking warning.
//	post_tool_use   payload adds {result, is_error}; exit codes are advisory.
//	session_end     payload {event, session_id, root}; fire-and-forget.
//
// The hook command runs through the platform shell with the payload as JSON on
// stdin, cwd = project root, and MEMCODE_HOOK_EVENT / MEMCODE_TOOL_NAME /
// MEMCODE_SESSION_ID / MEMCODE_PROJECT_DIR in the environment. Default timeout
// 60s per hook (config "timeout" in seconds overrides).
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Events.
const (
	SessionStart = "session_start"
	PreToolUse   = "pre_tool_use"
	PostToolUse  = "post_tool_use"
	SessionEnd   = "session_end"
)

const defaultTimeout = 60 * time.Second

// blockExitCode is the pre_tool_use veto: exit 2 = block (same as Claude Code).
const blockExitCode = 2

// Hook is one configured command.
type Hook struct {
	// Matcher is a regexp matched against the TOOL NAME for pre/post_tool_use
	// (full match; empty = every tool). Ignored for session events.
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds; 0 = default

	re *regexp.Regexp // compiled matcher; nil = match all
}

// config is the hooks.json shape: {"hooks": {"<event>": [ {...}, ... ]}}.
type config struct {
	Hooks map[string][]Hook `json:"hooks"`
}

// Set is the merged, compiled hook configuration for one session.
type Set struct {
	hooks     map[string][]Hook
	root      string
	sessionID string
	warnings  []string
}

// SetSessionID stamps the CURRENT session id onto the set so hook commands see
// it as MEMCODE_SESSION_ID. Callers re-stamp on use (the id changes across
// /resume and /fork while the loaded set is cached for the Session's lifetime).
func (s *Set) SetSessionID(id string) {
	if s != nil {
		s.sessionID = id
	}
}

// Load reads and merges the user-wide then project hooks files. Missing files
// are fine (empty set); malformed files or matchers become Warnings, never
// errors — a broken hooks.json must not take the agent down.
func Load(root string) *Set {
	s := &Set{hooks: map[string][]Hook{}, root: root}
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".memcode", "hooks.json"))
	}
	paths = append(paths, filepath.Join(root, ".memcode", "hooks.json"))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // absent is the normal case
		}
		var c config
		if err := json.Unmarshal(b, &c); err != nil {
			s.warnings = append(s.warnings, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		for event, hs := range c.Hooks {
			for _, h := range hs {
				if strings.TrimSpace(h.Command) == "" {
					continue
				}
				if h.Matcher != "" {
					re, err := regexp.Compile("^(?:" + h.Matcher + ")$")
					if err != nil {
						s.warnings = append(s.warnings, fmt.Sprintf("%s: bad matcher %q: %v", p, h.Matcher, err))
						continue
					}
					h.re = re
				}
				s.hooks[event] = append(s.hooks[event], h)
			}
		}
	}
	return s
}

// Empty reports whether no hooks are configured (the fast path).
func (s *Set) Empty() bool { return s == nil || len(s.hooks) == 0 }

// Warnings are non-fatal load problems (bad JSON, bad matchers) for surfacing
// once per session.
func (s *Set) Warnings() []string {
	if s == nil {
		return nil
	}
	return s.warnings
}

// Result is one hook execution's outcome.
type Result struct {
	Block   bool   // pre_tool_use exit 2
	Message string // block reason (stderr) or warning text
	Stdout  string // captured stdout (used by session_start context injection)
}

// Run executes every hook for event whose matcher accepts toolName, in config
// order, and returns their results. payload is marshalled once onto stdin.
func (s *Set) Run(ctx context.Context, event, toolName string, payload any) []Result {
	if s.Empty() {
		return nil
	}
	hs := s.hooks[event]
	if len(hs) == 0 {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	var out []Result
	for _, h := range hs {
		if h.re != nil && !h.re.MatchString(toolName) {
			continue
		}
		out = append(out, s.runOne(ctx, h, event, toolName, body))
	}
	return out
}

func (s *Set) runOne(ctx context.Context, h Hook, event, toolName string, stdin []byte) Result {
	timeout := defaultTimeout
	if h.Timeout > 0 {
		timeout = time.Duration(h.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", h.Command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", h.Command)
	}
	cmd.Dir = s.root
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = append(os.Environ(),
		"MEMCODE_HOOK_EVENT="+event,
		"MEMCODE_TOOL_NAME="+toolName,
		"MEMCODE_SESSION_ID="+s.sessionID,
		"MEMCODE_PROJECT_DIR="+s.root,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// On timeout, CommandContext kills only the shell — a child (`sleep 5` in a
	// `sh -c` hook) inherits the output pipes and keeps them open, so Wait
	// would block until the CHILD exits and the timeout wouldn't bound the
	// hook. WaitDelay makes Wait abandon the pipe drain shortly after the kill.
	cmd.WaitDelay = time.Second
	err := cmd.Run()

	res := Result{Stdout: truncate(stdout.String(), 8<<10)}
	if err == nil {
		return res
	}
	msg := strings.TrimSpace(truncate(stderr.String(), 4<<10))
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == blockExitCode && event == PreToolUse {
		if msg == "" {
			msg = "blocked by a pre_tool_use hook"
		}
		return Result{Block: true, Message: msg}
	}
	if ctx.Err() != nil {
		msg = fmt.Sprintf("hook timed out after %s", timeout)
	} else if msg == "" {
		msg = err.Error()
	}
	return Result{Message: fmt.Sprintf("hook %q: %s", h.Command, msg)}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… (truncated)"
}
