package vxui

import (
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/config"
)

// extraMileOptions are the two states of "extra mile" mode (Off first so the default reads
// first). On asks the agent to go above and beyond — edge cases + feature completeness — on
// every plan and execution; the gateway injects the rule when the fact is set.
var extraMileOptions = []struct {
	on   bool
	name string
	desc string
}{
	{false, "Off", "Do what's asked — nothing extra."},
	{true, "On", "Go above and beyond: check edge cases + feature completeness on every plan and execution."},
}

// openExtraMile opens the on/off selector with the cursor on the current state.
func (s *appState) openExtraMile() {
	if s.busy() {
		s.sysln("can't open /extramile mid-turn — wait for the current task to finish")
		return
	}
	sel := 0
	if s.w.sess.ExtraMile() {
		sel = 1
	}
	s.SetState(func() {
		s.extraMileSel = sel
		s.extraMileChoosing = true
	})
}

// handleExtraMileKey drives the selector while open: ↑↓ choose, Enter applies, Esc cancels.
func (s *appState) handleExtraMileKey(k string) ui.EventResult {
	switch k {
	case "Up":
		if s.extraMileSel > 0 {
			s.SetState(func() { s.extraMileSel-- })
		}
	case "Down", "Tab":
		if s.extraMileSel < len(extraMileOptions)-1 {
			s.SetState(func() { s.extraMileSel++ })
		}
	case "Enter":
		o := extraMileOptions[s.extraMileSel]
		s.SetState(func() { s.extraMileChoosing = false })
		s.applyExtraMile(o.on)
	case "Escape":
		s.SetState(func() { s.extraMileChoosing = false })
		s.sysln("extra mile unchanged")
	}
	return ui.EventHandled
}

// extraMilePickerView is the modal on/off selector: ❯ marks the cursor, ● the current state,
// with a short description and a MUTED notice that the mode costs extra tokens.
func (s *appState) extraMilePickerView() ui.Widget {
	cur := s.w.sess.ExtraMile()
	rows := []ui.Widget{
		ui.RichText{Spans: []ui.TextSpan{{Text: "Extra mile mode", Style: s.sty.brand}}},
		ui.RichText{Spans: []ui.TextSpan{{Text: "Go above and beyond the request — check edge cases and feature completeness.", Style: s.sty.muted}}, SoftWrap: true, MaxLines: 2},
		ui.SizedBox{Height: 1},
	}
	for i, o := range extraMileOptions {
		marker, nameStyle := "  ", s.sty.muted
		if i == s.extraMileSel {
			marker, nameStyle = "❯ ", s.sty.emph
		}
		saved := "  "
		if o.on == cur {
			saved = "● "
		}
		spans := []ui.TextSpan{
			{Text: marker + saved, Style: s.sty.brand},
			{Text: o.name, Style: nameStyle},
		}
		if i == s.extraMileSel {
			spans = append(spans, ui.TextSpan{Text: "   " + o.desc, Style: s.sty.muted})
		}
		rows = append(rows, ui.RichText{Spans: spans, SoftWrap: true, MaxLines: 3})
	}
	rows = append(rows, ui.SizedBox{Height: 1},
		s.hintRow("↑↓ choose · Enter apply · Esc cancel"),
		s.hintRow("note: this mode consumes extra tokens — longer plans and more thorough work"))
	return ui.Flex{Axis: ui.Vertical, MainAxisSize: ui.MainAxisSizeMin, CrossAxisAlignment: ui.CrossAxisStart, Children: rows}
}

// applyExtraMile sets + persists the mode (best-effort save), with a muted token-cost notice.
func (s *appState) applyExtraMile(on bool) {
	s.w.sess.SetExtraMile(on)
	if cfg, err := config.Load(s.w.sess.Root()); err == nil {
		cfg.ExtraMile = on
		_ = cfg.Save()
	}
	if on {
		s.sysln("extra mile → on   (above-and-beyond on every plan + execution · consumes extra tokens)")
	} else {
		s.sysln("extra mile → off")
	}
}

// applyEffort forces the per-turn thinking effort for THIS session (/effort). Session-scoped on
// purpose — it resets to auto on restart, since silently forcing high on every future session
// would inflate token cost. "auto" hands control back to the per-turn heuristic.
func (s *appState) applyEffort(level string) {
	s.w.sess.SetEffortOverride(level)
	if level == "auto" {
		s.sysln("thinking effort → auto   (memcode decides per turn)")
	} else {
		s.sysln("thinking effort → " + level + "   (forced every turn this session · higher = more tokens)")
	}
}
