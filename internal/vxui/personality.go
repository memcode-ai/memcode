package vxui

import (
	"strings"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/config"
)

type personalityOption struct{ key, display, sample string }

// personalityCatalog is the voice axis (voice ONLY — never changes behavior or the permission
// floor). "" = default; "random" picks a different voice per message. The doctrine
// composer owns the actual prose/tone; this UI just records the chosen voice as a fact.
var personalityCatalog = []personalityOption{
	{"", "Default", "Neutral, no special voice."},
	{"professional", "Professional", "Done. Tests pass, build is clean."},
	{"joker", "Joker", "Done — tests passed first try, which statistically shouldn't happen. 🎲"},
	{"funny", "Funny", "Boom. Tests green, build happy, we all go home early."},
	{"insulting", "Insulting", "Fixed your mess. Tests pass now — you're welcome, champ."},
	{"emoji", "Emoji", "✅ Done! Tests pass 🧪 build clean 🚀"},
	{"mirror", "Mirror", "Matches your energy — terse, formal, or hyped, whatever you bring."},
	{"zen", "Zen", "All is well. The tests pass; the build is at peace."},
	{"dry", "Dry", "Tests pass. Try to contain your excitement."},
	{"random", "Random", "A different voice every message — chaos mode. 🎲"},
}

// openPersonality opens the voice picker with the cursor on the active voice.
func (s *appState) openPersonality() {
	if s.busy() {
		s.sysln("can't open /personality mid-turn — wait for the current task to finish")
		return
	}
	cur := s.w.sess.Personality()
	sel := 0
	for i, o := range personalityCatalog {
		if o.key == cur {
			sel = i
			break
		}
	}
	s.SetState(func() {
		s.personalityOrig = cur
		s.personalitySel = sel
		s.personalityChoosing = true
	})
}

// handlePersonalityKey drives the voice picker while it's open: ↑↓ choose, Enter applies +
// persists, Esc keeps the saved voice. No live preview (voice lands next turn), so moving the
// cursor is harmless. Owns the keyboard (every key is consumed).
func (s *appState) handlePersonalityKey(k string) ui.EventResult {
	switch k {
	case "Up":
		if s.personalitySel > 0 {
			s.SetState(func() { s.personalitySel-- })
		}
	case "Down", "Tab":
		if s.personalitySel < len(personalityCatalog)-1 {
			s.SetState(func() { s.personalitySel++ })
		}
	case "Enter":
		o := personalityCatalog[s.personalitySel]
		s.SetState(func() { s.personalityChoosing = false })
		s.applyPersonality(o.key, o.display)
	case "Escape":
		s.SetState(func() { s.personalityChoosing = false })
		s.sysln("personality unchanged")
	}
	return ui.EventHandled
}

// personalityPickerView is the modal voice picker: ❯ marks the cursor, ● the saved voice, and
// the highlighted row shows its sample so the voice reads before committing.
func (s *appState) personalityPickerView() ui.Widget {
	rows := []ui.Widget{
		ui.RichText{Spans: []ui.TextSpan{{Text: "Set the agent's voice", Style: s.sty.brand}}},
		ui.SizedBox{Height: 1},
	}
	for i, o := range personalityCatalog {
		marker, nameStyle := "  ", s.sty.muted
		if i == s.personalitySel {
			marker, nameStyle = "❯ ", s.sty.emph
		}
		saved := "  "
		if o.key == s.personalityOrig {
			saved = "● "
		}
		spans := []ui.TextSpan{
			{Text: marker + saved, Style: s.sty.brand},
			{Text: o.display, Style: nameStyle},
		}
		if i == s.personalitySel {
			spans = append(spans, ui.TextSpan{Text: "   " + o.sample, Style: s.sty.muted})
		}
		rows = append(rows, ui.RichText{Spans: spans, SoftWrap: true, MaxLines: 3})
	}
	rows = append(rows, ui.SizedBox{Height: 1},
		s.hintRow("↑↓ choose · Enter apply · Esc cancel · or /personality <your own voice>"),
		s.hintRow("voice only — never changes what the agent does or its permission floor"))
	return ui.Flex{Axis: ui.Vertical, MainAxisSize: ui.MainAxisSizeMin, CrossAxisAlignment: ui.CrossAxisStart, Children: rows}
}

// setPersonalityArg applies a free-form /personality argument: a known display/key maps to its
// canonical voice, anything else is taken as a custom voice verbatim.
func (s *appState) setPersonalityArg(arg string) {
	arg = strings.TrimSpace(arg)
	key, display := arg, arg
	for _, o := range personalityCatalog {
		if o.key != "" && (strings.EqualFold(arg, o.display) || strings.EqualFold(arg, o.key)) {
			key, display = o.key, o.display
			break
		}
	}
	s.applyPersonality(key, display)
}

// applyPersonality sets + persists the chosen voice (best-effort save).
func (s *appState) applyPersonality(key, display string) {
	s.w.sess.SetPersonality(key)
	s.updateConfig(func(cfg *config.Config) { cfg.Personality = key })
	if key == "" {
		s.sysln("personality → default (neutral voice)")
	} else {
		s.sysln("personality → " + display + "   (voice only — never changes what I do)")
	}
}
