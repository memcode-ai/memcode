package runtime

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/llm"
)

// The tooled plan reviewer (read-only + purpose=review) is a strict claim-verifier: it gets
// file reads / grep / diff, but NEVER bash (no shell execution) and nothing mutating or egressing.
func TestReviewSessionToolWhitelist(t *testing.T) {
	s := newTodoSession(t)
	s.readOnly = true
	s.purpose = llm.Review
	defs := s.toolDefs()
	for _, allowed := range []string{tools.ReadFile, tools.Ripgrep, tools.ListDir, tools.GitDiff, tools.CodeQuery} {
		if !hasTool(defs, allowed) {
			t.Errorf("review session must offer %q", allowed)
		}
	}
	// No shell — even read-only bash — and no edits/egress/sub-agents.
	for _, banned := range []string{tools.Bash, tools.EditFile, tools.WebSearch, tools.Fetch, tools.Explore, tools.AskUser} {
		if hasTool(defs, banned) {
			t.Errorf("review session must NOT offer %q (claim-verifier, no shell/mutation/egress)", banned)
		}
	}
	// A plain read-only explorer (no review purpose) still gets the inspect shell — the
	// whitelist is review-specific, it didn't tighten the generic scout.
	s.purpose = llm.Explore
	if !hasTool(s.toolDefs(), tools.Bash) {
		t.Error("a generic read-only explorer should still get the read-only bash inspect shell")
	}
}

func TestExploreOfferedExceptInExplorers(t *testing.T) {
	s := newTodoSession(t)
	// Normal chat: explore IS offered now — a review/research turn can fan out
	// parallel verifiers instead of a linear surface pass.
	if !hasTool(s.toolDefs(), tools.Explore) {
		t.Error("normal chat should offer explore (verification/research fan-out)")
	}
	// Plan mode: still offered (that's how it fans out research).
	enterPlanForTest(s, "")
	if !hasTool(s.toolDefs(), tools.Explore) {
		t.Error("plan mode should offer explore")
	}
	// Read-only explorers: never (no nesting).
	s.planCtl.Cancel()
	s.readOnly = true
	if hasTool(s.toolDefs(), tools.Explore) {
		t.Error("explorers must NOT offer explore (no nesting)")
	}
}

func TestWebToolsAvailableExceptExplorers(t *testing.T) {
	s := newTodoSession(t)
	// Normal chat: web tools are available — the MODEL decides when to use them
	// (governed by the repo-first prompt policy), not a brittle keyword pre-gate.
	if !hasTool(s.toolDefs(), tools.WebSearch) || !hasTool(s.toolDefs(), tools.Fetch) {
		t.Error("normal chat should expose web tools so the model can decide")
	}
	// Plan mode (research): still available.
	enterPlanForTest(s, "")
	if !hasTool(s.toolDefs(), tools.WebSearch) || !hasTool(s.toolDefs(), tools.Fetch) {
		t.Error("plan mode should expose web tools for research")
	}
	s.planCtl.Cancel()
	// Read-only explorers never egress — hard-denied regardless.
	s.readOnly = true
	if hasTool(s.toolDefs(), tools.WebSearch) || hasTool(s.toolDefs(), tools.Fetch) {
		t.Error("explorers must never get web tools")
	}
}

// TestBrowserToolsGatedOnChromeFlag asserts the browser tools are ABSENT from the
// tool list when --chrome is not set (so the model can't hallucinate a browser
// tool that isn't wired), and PRESENT when it is. This is the gating that makes
// --chrome safe: the model only sees browser_navigate etc. when a real Chrome
// instance is available to back them.
func TestBrowserToolsGatedOnChromeFlag(t *testing.T) {
	s := newTodoSession(t)

	// Default: browser tools absent — the model must not see them.
	for _, name := range []string{
		tools.BrowserNavigate, tools.BrowserClick, tools.BrowserType,
		tools.BrowserScreenshot, tools.BrowserEval, tools.BrowserText,
	} {
		if hasTool(s.toolDefs(), name) {
			t.Errorf("browser tool %q must be absent when --chrome is not set", name)
		}
	}

	// Enable --chrome → all six browser tools appear.
	s.SetBrowserEnabled(true)
	for _, name := range []string{
		tools.BrowserNavigate, tools.BrowserClick, tools.BrowserType,
		tools.BrowserScreenshot, tools.BrowserEval, tools.BrowserText,
	} {
		if !hasTool(s.toolDefs(), name) {
			t.Errorf("browser tool %q must be present when --chrome is set", name)
		}
	}

	// Read-only explorers never get browser tools (the browser is an
	// executive-session capability — an explorer can't drive Chrome).
	s.readOnly = true
	for _, name := range []string{
		tools.BrowserNavigate, tools.BrowserScreenshot, tools.BrowserText,
	} {
		if hasTool(s.toolDefs(), name) {
			t.Errorf("browser tool %q must be absent for read-only explorers", name)
		}
	}
}
