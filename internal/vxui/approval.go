package vxui

import (
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"
)

// approvalOption is one card row: its text plus what Enter on it means.
type approvalOption struct {
	label string
	kind  string // "yes" | "remember" | "scope" | "tell" | "cancel"
	scope string // ApprovalScope key when kind == "scope"
}

// approvalOptions builds the card's rows. Scoped requests (MCP) read Execute / one row per
// remember scope / Cancel — Cancel is a PLAIN deny (the model sees the refusal and moves on),
// unlike "tell", which interrupts the turn to redirect it. Everything else keeps the classic
// three: Yes / don't-ask-again / tell.
func (s *appState) approvalOptions() []approvalOption {
	p := s.pending
	if len(p.RememberScopes) > 0 {
		opts := make([]approvalOption, 0, len(p.RememberScopes)+2)
		opts = append(opts, approvalOption{label: "Execute", kind: "yes"})
		for _, sc := range p.RememberScopes {
			opts = append(opts, approvalOption{label: sc.Label, kind: "scope", scope: sc.Key})
		}
		return append(opts, approvalOption{label: "Cancel", kind: "cancel"})
	}
	labels := s.approvalOptionLabels()
	last := approvalOption{label: labels[2], kind: "tell"}
	if p.Label == "Use skill" {
		// Declining a skill load is not a redirection — it's a simple "skip it": plain deny,
		// the model sees the refusal and continues the turn. No "tell" interrupt dance.
		last.kind = "cancel"
	}
	return []approvalOption{
		{label: labels[0], kind: "yes"},
		{label: labels[1], kind: "remember"},
		last,
	}
}

// approvalEnterAction decides what Enter does on the approval card, given the highlighted
// option's kind and whether the user typed feedback. Typed feedback is always "tell" (deny +
// redirect). With nothing typed, "tell" returns "hint" — NOT an interrupt. The old code sent
// Interrupt on an empty "tell" Enter, which silently STOPPED the whole turn when the user just
// meant to pick an option. Esc remains the explicit stop. "cancel" with nothing typed is a
// plain deny, no hint dance — Cancel IS the chosen outcome.
func approvalEnterAction(kind string, hasFeedback bool) string {
	if hasFeedback {
		return "tell"
	}
	if kind == "tell" {
		return "hint" // "tell" with no feedback → do nothing but prompt, never interrupt
	}
	return kind
}

// answerApproval sends the decision back to the blocked engine goroutine and clears the card.
func (s *appState) answerApproval(d runtime.ApprovalDecision) {
	if s.approval != nil {
		s.approval <- d
	}
	s.SetState(func() {
		s.pending = nil
		s.approval = nil
		s.approveChoice = 0
		s.advanceHitl() // pop this card; show the next queued prompt (or resume the clock if none)
	})
}

// answerApprovalChoice maps a card outcome to a decision. "tell" is "No, and tell me what to
// do differently": stop the turn and feed the typed redirection back so the agent acts on it
// instead of the path the user just rejected. "cancel" (scoped cards) denies without stopping
// the turn — the model sees the refusal and continues. scope carries the chosen remember key.
func (s *appState) answerApprovalChoice(kind, scope, feedback string) {
	switch kind {
	case "yes":
		s.answerApproval(runtime.ApprovalDecision{Allow: true})
	case "remember":
		// A SCOPED "don't ask again" — the backend decides what to remember by the action
		// kind (this binary's commands / this skill / edits this session), never a global
		// allow-all (that's Shift+Tab).
		s.answerApproval(runtime.ApprovalDecision{Allow: true, Remember: true})
	case "scope":
		s.answerApproval(runtime.ApprovalDecision{Allow: true, RememberScope: scope})
	case "cancel":
		s.answerApproval(runtime.ApprovalDecision{})
	case "tell":
		s.answerApproval(runtime.ApprovalDecision{Interrupt: true, Reason: feedback})
		s.SetState(s.clearComposerInput)
	}
}

// approvalCard renders the HITL approval prompt (Claude-Code style): header · command/detail ·
// "Do you want to proceed?" · numbered options, framed in a rounded box above the composer.
func (s *appState) approvalCard() ui.Widget {
	p := s.pending
	skill := p.Label == "Use skill"
	rows := []ui.Widget{
		ui.RichText{Spans: s.approvalHeaderSpans(), SoftWrap: true, MaxLines: 2},
		ui.SizedBox{Height: 1},
	}
	if !skill { // a skill's name is already in the header; everything else shows its command/path
		// Muted body (it's evidence, not chrome), and an ellipsis on overflow so a long command —
		// e.g. a heredoc that buries the risk-driving sub-command past the cut — SAYS it was clipped
		// instead of silently dropping the very token the gate is about.
		rows = append(rows, ui.RichText{Spans: []ui.TextSpan{{Text: "  " + p.Title, Style: s.sty.muted}}, SoftWrap: true, MaxLines: 10, Overflow: ui.TextOverflowEllipsis})
	}
	if p.Detail != "" {
		rows = append(rows, ui.RichText{Spans: []ui.TextSpan{{Text: "  " + p.Detail, Style: s.sty.muted}}, SoftWrap: true, MaxLines: 2})
	}
	rows = append(rows,
		ui.SizedBox{Height: 1},
		ui.RichText{Spans: []ui.TextSpan{{Text: "Do you want to proceed?", Style: s.sty.emph}}},
	)
	options := s.approvalOptions()
	opts := make([]choice, 0, len(options))
	for _, o := range options {
		opts = append(opts, choice{label: o.label})
	}
	rows = append(rows, s.optionList(opts, s.approveChoice, true)...)
	return s.card(rows...)
}

// approvalOptionLabels returns the three card options (Claude-Code style), with the
// "don't ask again" scope filled in by action kind.
func (s *appState) approvalOptionLabels() []string {
	p := s.pending
	remember := "Yes, and don't ask again for edits this session"
	if cmd := strings.TrimSpace(p.Command); cmd != "" {
		// Scope on the sub-command that DROVE the gate (RiskHead), not the leading token — so
		// the label matches BOTH the header "(go)" and what the backend actually remembers
		// (rememberPattern keys on RiskHead). For `cd … && go vet` that's "go", not "cd".
		bin := permissions.RiskHead(cmd)
		if bin == "" {
			if f := strings.Fields(cmd); len(f) > 0 {
				bin = f[0]
			}
		}
		remember = "Yes, and don't ask again for " + bin + " commands in " + filepath.Base(s.w.sess.Root())
	} else if p.Label == "Use skill" {
		return []string{"Yes", "Yes, and don't ask again for this skill", "Skip (esc)"}
	}
	return []string{"Yes", remember, "No, and tell me what to do differently (esc)"}
}

// approvalHeaderSpans builds the card header — the category, then a parenthetical naming
// WHAT (a skill's name, or the sub-command that drove a bash escalation) plus the risk class.
func (s *appState) approvalHeaderSpans() []ui.TextSpan {
	p := s.pending
	head := p.Label
	if head == "" {
		head = "Approval"
	}
	spans := []ui.TextSpan{{Text: head, Style: s.sty.warn}}
	switch {
	case p.Label == "Use skill" && p.Title != "":
		spans = append(spans, ui.TextSpan{Text: "  (" + p.Title + ")", Style: s.sty.muted})
	case p.Label == "Bash command" && p.Command != "":
		// Name WHAT drove the prompt (the risk-head binary) but do NOT brand a
		// severity verdict: an rm may or may not be dangerous — the classifier
		// can't know (that's WHY it prompts), and the user judges the actual
		// command better than a chip that pre-judges it.
		if h := permissions.RiskHead(p.Command); h != "" {
			spans = append(spans, ui.TextSpan{Text: "  (" + h + ")", Style: s.sty.muted})
		}
	default:
		if p.Risk != "" {
			spans = append(spans, ui.TextSpan{Text: "  (" + p.Risk + ")", Style: s.sty.muted})
		}
	}
	return spans
}
