package vxui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/checkpoint"
)

// The /rewind picker: a two-stage modal — pick a rewind point, then CONFIRM the
// restore — replacing the old wall of unlabeled text. Rewind is destructive to
// the working tree (it discards agent edits made since the chosen turn), so it
// gets the same select→confirm treatment as any consequential action, never a
// bare "type /rewind 3 and hope".

// openRewind opens the picker. A non-empty arg (/rewind <n>) jumps straight to
// the confirm for that seq; bare /rewind shows the selector.
func (s *appState) openRewind(arg string) {
	if s.busy() {
		s.sysln("can't rewind mid-turn — wait for the current task to finish")
		return
	}
	points := s.w.sess.Checkpoints() // oldest first
	if len(points) == 0 {
		s.sysln("no rewind points yet — they appear once a turn edits files.")
		return
	}
	// Present newest first: you almost always undo the most recent turn.
	reversed := make([]checkpoint.Manifest, len(points))
	for i, p := range points {
		reversed[len(points)-1-i] = p
	}

	if arg = strings.TrimSpace(arg); arg != "" {
		seq, err := strconv.Atoi(arg)
		if err != nil {
			s.sysln("usage: /rewind (opens the picker) or /rewind <n>")
			return
		}
		idx := -1
		for i, p := range reversed {
			if p.Seq == seq {
				idx = i
			}
		}
		if idx < 0 {
			s.sysln(fmt.Sprintf("no rewind point %d — run /rewind to see the list.", seq))
			return
		}
		s.SetState(func() {
			s.rewindPoints = reversed
			s.rewindSel = idx
			s.rewindConfirm = true // skip the list, go straight to confirm
			s.rewindChoosing = true
		})
		return
	}
	s.SetState(func() {
		s.rewindPoints = reversed
		s.rewindSel = 0
		s.rewindConfirm = false
		s.rewindChoosing = true
	})
}

// handleRewindKey drives the picker: list stage navigates + advances to confirm;
// confirm stage restores or backs out.
func (s *appState) handleRewindKey(k string) ui.EventResult {
	if s.rewindConfirm {
		switch k {
		case "Enter":
			s.doRewind()
		case "Escape", "Left":
			s.SetState(func() { s.rewindConfirm = false }) // back to the list
		}
		return ui.EventHandled
	}
	switch k {
	case "Up":
		if s.rewindSel > 0 {
			s.SetState(func() { s.rewindSel-- })
		}
	case "Down", "Tab":
		if s.rewindSel < len(s.rewindPoints)-1 {
			s.SetState(func() { s.rewindSel++ })
		}
	case "Enter", "Right":
		s.SetState(func() { s.rewindConfirm = true })
	case "Escape":
		s.SetState(func() { s.rewindChoosing = false })
		s.sysln("rewind cancelled — nothing changed.")
	}
	return ui.EventHandled
}

// doRewind performs the restore for the selected point and closes the picker.
func (s *appState) doRewind() {
	p := s.rewindPoints[s.rewindSel]
	s.SetState(func() { s.rewindChoosing = false })
	restored, err := s.w.sess.Rewind(p.Seq)
	if err != nil {
		s.sysln("rewind: " + err.Error())
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "↩ rewound to before \"%s\" — %d file(s) restored:", clipStr(p.Label, 60), len(restored))
	for _, f := range restored {
		b.WriteString("\n  " + f)
	}
	s.sysln(b.String())
}

// rewindPickerView renders the current stage (list or confirm).
func (s *appState) rewindPickerView() ui.Widget {
	if s.rewindConfirm {
		return s.rewindConfirmView()
	}
	rows := []ui.Widget{
		ui.RichText{Spans: []ui.TextSpan{{Text: "Rewind — undo agent edits", Style: s.sty.brand}}},
		ui.RichText{Spans: []ui.TextSpan{{Text: "Pick a turn; its files return to the state they were in BEFORE it ran. Git is untouched.", Style: s.sty.muted}}, SoftWrap: true, MaxLines: 2},
		ui.SizedBox{Height: 1},
	}
	for i, p := range s.rewindPoints {
		marker, nameStyle := "  ", s.sty.muted
		if i == s.rewindSel {
			marker, nameStyle = "❯ ", s.sty.emph
		}
		meta := fmt.Sprintf("%s · %d file(s)", p.At.Local().Format("15:04"), len(p.Files))
		spans := []ui.TextSpan{
			{Text: marker, Style: s.sty.brand},
			{Text: clipStr(p.Label, 52), Style: nameStyle},
			{Text: "   " + meta, Style: s.sty.muted},
		}
		rows = append(rows, ui.RichText{Spans: spans, SoftWrap: true, MaxLines: 2})
	}
	rows = append(rows, ui.SizedBox{Height: 1}, s.hintRow("↑↓ choose · Enter review · Esc cancel"))
	return ui.Flex{Axis: ui.Vertical, MainAxisSize: ui.MainAxisSizeMin, CrossAxisAlignment: ui.CrossAxisStart, Children: rows}
}

// rewindConfirmView is the destructive-action confirm: names exactly what will be
// restored and warns that later agent edits are discarded.
func (s *appState) rewindConfirmView() ui.Widget {
	p := s.rewindPoints[s.rewindSel]
	// Files edited AT this turn AND every turn after it are what a restore reverts
	// (Restore rolls the tree back to before this point). Show this point's files;
	// the count of newer points tells the user how much else rolls back.
	newer := s.rewindSel // points listed above the selected one (newer)
	rows := []ui.Widget{
		ui.RichText{Spans: []ui.TextSpan{{Text: "Restore to before this turn?", Style: s.sty.brand}}},
		ui.RichText{Spans: []ui.TextSpan{
			{Text: "  " + clipStr(p.Label, 60), Style: s.sty.emph},
			{Text: "   " + p.At.Local().Format("15:04"), Style: s.sty.muted},
		}, SoftWrap: true, MaxLines: 2},
	}
	files := make([]string, 0, len(p.Files))
	for _, f := range p.Files {
		files = append(files, f.Path)
	}
	rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
		{Text: "  files: ", Style: s.sty.muted},
		{Text: clipStr(strings.Join(files, ", "), 120), Style: s.sty.muted},
	}, SoftWrap: true, MaxLines: 3})
	warn := "This discards agent edits made since then. Git history is untouched."
	if newer > 0 {
		warn = fmt.Sprintf("This also rolls back %d later edit-turn(s). Agent edits since then are discarded. Git is untouched.", newer)
	}
	rows = append(rows,
		ui.SizedBox{Height: 1},
		ui.RichText{Spans: []ui.TextSpan{{Text: warn, Style: s.sty.warn}}, SoftWrap: true, MaxLines: 3},
		ui.SizedBox{Height: 1},
		s.hintRow("Enter restore · Esc back"),
	)
	return ui.Flex{Axis: ui.Vertical, MainAxisSize: ui.MainAxisSizeMin, CrossAxisAlignment: ui.CrossAxisStart, Children: rows}
}

// clipStr truncates s to n display runes with an ellipsis (rune-safe).
func clipStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
