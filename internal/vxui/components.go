package vxui

import (
	"strconv"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"
)

// choice is one selectable row in an option list: a label, plus an optional muted
// description that wraps beneath it.
type choice struct {
	label string
	desc  string
}

// card stacks a modal/selector's content rows in the live region — shrink-wrapped to content,
// left-aligned, no border. Every selector (approval, ask, plan) builds on this so they share
// one frame. (A border read badly against soft-wrapped long lines, so the chrome is just the
// rows themselves, set off from scrollback by the surrounding blank lines.)
func (s *appState) card(rows ...ui.Widget) ui.Widget {
	return ui.Flex{
		Axis:               ui.Vertical,
		MainAxisSize:       ui.MainAxisSizeMin,
		CrossAxisAlignment: ui.CrossAxisStart,
		Children:           rows,
	}
}

// optionList renders a navigable list of choices: the selected row gets a ❯ marker + emphasis,
// the others a blank indent + muted text. numbered prefixes each label "N. " (1-based). A
// choice's desc, when set, wraps as a muted line indented under its label.
func (s *appState) optionList(opts []choice, selected int, numbered bool) []ui.Widget {
	rows := make([]ui.Widget, 0, len(opts))
	for i, o := range opts {
		marker, st := "  ", s.sty.muted
		if i == selected {
			marker, st = "❯ ", s.sty.emph
		}
		spans := []ui.TextSpan{{Text: marker, Style: s.sty.brand}}
		if numbered {
			spans = append(spans, ui.TextSpan{Text: strconv.Itoa(i+1) + ". ", Style: s.sty.muted})
		}
		spans = append(spans, ui.TextSpan{Text: o.label, Style: st})
		rows = append(rows, ui.RichText{Spans: spans, SoftWrap: true, MaxLines: 2})
		if o.desc != "" {
			rows = append(rows, ui.RichText{Spans: []ui.TextSpan{{Text: "   " + o.desc, Style: s.sty.muted}}, SoftWrap: true, MaxLines: 4})
		}
	}
	return rows
}

// hintRow is the muted help line at the foot of a card (key bindings).
func (s *appState) hintRow(text string) ui.Widget {
	return ui.RichText{Spans: []ui.TextSpan{{Text: text, Style: s.sty.muted}}}
}
