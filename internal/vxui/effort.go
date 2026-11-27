package vxui

import (
	"github.com/memcode-ai/vaxis/ui"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/config"
)

// effortOptions is memcode's universal effort scale, plus Default (send no
// effort; the vendor's own setting applies). The level names are what
// SetEffortOverride takes.
var effortOptions = []struct {
	level string
	name  string
	desc  string
}{
	{"default", "Default", "The model's own default effort."},
	{"low", "Low", "Least thinking. Fastest and cheapest."},
	{"medium", "Medium", "Balanced. memcode's default."},
	{"high", "High", "Thinks harder on every step."},
	{"xhigh", "Extra high", "Deeper than high, where the model offers it."},
	{"max", "Max", "The model's deepest thinking. Slowest, most tokens."},
}

// effortModel is the model the picker describes: the session's pin.
func (s *appState) effortModel() (label, name string) {
	label = s.w.sess.DisplayModel()
	name = label
	if m, ok := catalog.LookupModel(label); ok && m.Name != "" {
		name = m.Name
	}
	return label, name
}

// visibleEffortOptions are the rows for the current model: Default, every
// level the model supports natively (all of them when the catalog declares
// none), and the saved level even when the model doesn't have it, so the
// picker always shows where the cursor starts. Levels that would run exactly
// like another row are hidden.
func (s *appState) visibleEffortOptions() []int {
	label, _ := s.effortModel()
	native := map[string]bool{}
	for _, l := range catalog.ModelEfforts(label) {
		native[l] = true
	}
	cur := s.w.sess.EffortOverride()
	var rows []int
	for i, o := range effortOptions {
		if o.level == "default" || len(native) == 0 || native[o.level] || o.level == cur {
			rows = append(rows, i)
		}
	}
	return rows
}

// effortRunsAs explains a level the model doesn't have natively: "runs as
// xhigh on Grok 4.7". Empty when the level is native (or Default).
func (s *appState) effortRunsAs(level string) string {
	label, name := s.effortModel()
	if level == "default" {
		return ""
	}
	if got := catalog.ResolveEffort(label, level); got != level {
		return "runs as " + got + " on " + name
	}
	return ""
}

// openEffort opens the effort selector with the cursor on the current level.
func (s *appState) openEffort() {
	if s.busy() {
		s.sysln("can't open /effort mid-turn — wait for the current task to finish")
		return
	}
	cur := s.w.sess.EffortOverride()
	rows := s.visibleEffortOptions()
	sel := 0
	for i, idx := range rows {
		if effortOptions[idx].level == cur {
			sel = i
		}
	}
	s.SetState(func() {
		s.effortRows = rows
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
		if s.effortSel < len(s.effortRows)-1 {
			s.SetState(func() { s.effortSel++ })
		}
	case "Enter":
		o := effortOptions[s.effortRows[s.effortSel]]
		s.SetState(func() { s.effortChoosing = false })
		s.applyEffort(o.level)
	case "Escape":
		s.SetState(func() { s.effortChoosing = false })
		s.sysln("thinking effort unchanged")
	}
	return ui.EventHandled
}

// effortPickerView is the modal selector: ❯ marks the cursor, ● the saved level.
func (s *appState) effortPickerView() ui.Widget {
	cur := s.w.sess.EffortOverride()
	_, name := s.effortModel()
	rows := []ui.Widget{
		ui.RichText{Spans: []ui.TextSpan{{Text: "Thinking effort", Style: s.sty.brand}}},
		ui.RichText{Spans: []ui.TextSpan{{Text: "How hard " + name + " thinks on each step. Remembered across sessions.", Style: s.sty.muted}}, SoftWrap: true, MaxLines: 2},
		ui.SizedBox{Height: 1},
	}
	for i, idx := range s.effortRows {
		o := effortOptions[idx]
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
			desc := o.desc
			if note := s.effortRunsAs(o.level); note != "" {
				desc += " (" + note + ")"
			}
			spans = append(spans, ui.TextSpan{Text: "   " + desc, Style: s.sty.muted})
		}
		rows = append(rows, ui.RichText{Spans: spans, SoftWrap: true, MaxLines: 3})
	}
	rows = append(rows, ui.SizedBox{Height: 1},
		s.hintRow("↑↓ choose · Enter apply · Esc cancel"),
		s.hintRow("levels mean different budgets on different models · higher uses more tokens"))
	return ui.Flex{Axis: ui.Vertical, MainAxisSize: ui.MainAxisSizeMin, CrossAxisAlignment: ui.CrossAxisStart, Children: rows}
}

// applyEffort sets the session's thinking effort and remembers it. The level
// is the user's preference: switching models keeps it, and each model runs it
// as its closest native level.
func (s *appState) applyEffort(level string) {
	s.w.sess.SetEffortOverride(level)
	level = s.w.sess.EffortOverride()
	s.updateConfig(func(cfg *config.Config) { cfg.Effort = level })
	msg := "thinking effort → " + level
	if level == "default" {
		_, name := s.effortModel()
		msg += "   (" + name + "'s own default)"
	} else if note := s.effortRunsAs(level); note != "" {
		msg += "   (" + note + ")"
	}
	s.sysln(msg)
}
