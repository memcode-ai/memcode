package vxui

import "github.com/memcode-ai/vaxis/ui"

// effortOptions are the /effort levels, auto first because it is the default.
// The level names are what SetEffortOverride takes.
var effortOptions = []struct {
	level string
	name  string
	desc  string
}{
	{"auto", "Auto", "memcode picks the thinking depth each turn."},
	{"off", "Off", "No extended thinking. Fastest, fewest tokens."},
	{"medium", "Medium", "Some thinking on every turn."},
	{"high", "High", "Deepest thinking on every turn. Slowest, most tokens."},
}

// openEffort opens the effort selector with the cursor on the current level.
func (s *appState) openEffort() {
	if s.busy() {
		s.sysln("can't open /effort mid-turn — wait for the current task to finish")
		return
	}
	cur := s.w.sess.EffortOverride()
	sel := 0
	for i, o := range effortOptions {
		if o.level == cur {
			sel = i
		}
	}
	s.SetState(func() {
		s.effortSel = sel
		s.effortChoosing = true
	})
}

// handleEffortKey drives the selector while open: ↑↓ choose, Enter applies, Esc cancels.
func (s *appState) handleEffortKey(k string) ui.EventResult {
	switch k {
	case "Up":
		if s.effortSel > 0 {
			s.SetState(func() { s.effortSel-- })
		}
	case "Down", "Tab":
		if s.effortSel < len(effortOptions)-1 {
			s.SetState(func() { s.effortSel++ })
		}
	case "Enter":
		o := effortOptions[s.effortSel]
		s.SetState(func() { s.effortChoosing = false })
		s.applyEffort(o.level)
	case "Escape":
		s.SetState(func() { s.effortChoosing = false })
		s.sysln("thinking effort unchanged")
	}
	return ui.EventHandled
}

// effortPickerView is the modal selector: ❯ marks the cursor, ● the current level.
func (s *appState) effortPickerView() ui.Widget {
	cur := s.w.sess.EffortOverride()
	rows := []ui.Widget{
		ui.RichText{Spans: []ui.TextSpan{{Text: "Thinking effort", Style: s.sty.brand}}},
		ui.RichText{Spans: []ui.TextSpan{{Text: "How hard the model thinks on each turn of this session.", Style: s.sty.muted}}, SoftWrap: true, MaxLines: 2},
		ui.SizedBox{Height: 1},
	}
	for i, o := range effortOptions {
		marker, nameStyle := "  ", s.sty.muted
		if i == s.effortSel {
			marker, nameStyle = "❯ ", s.sty.emph
		}
		saved := "  "
		if o.level == cur {
			saved = "● "
		}
		spans := []ui.TextSpan{
			{Text: marker + saved, Style: s.sty.brand},
			{Text: o.name, Style: nameStyle},
		}
		if i == s.effortSel {
			spans = append(spans, ui.TextSpan{Text: "   " + o.desc, Style: s.sty.muted})
		}
		rows = append(rows, ui.RichText{Spans: spans, SoftWrap: true, MaxLines: 3})
	}
	rows = append(rows, ui.SizedBox{Height: 1},
		s.hintRow("↑↓ choose · Enter apply · Esc cancel"),
		s.hintRow("applies to this session only · higher effort uses more tokens"))
	return ui.Flex{Axis: ui.Vertical, MainAxisSize: ui.MainAxisSizeMin, CrossAxisAlignment: ui.CrossAxisStart, Children: rows}
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
